// Package alloyrun runs one Alloy process over stream files and waits until
// every line is echoed or dropped.
package alloyrun

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec" //nolint:depguard // Alloy is the system under test; it runs from argv, never through a shell.
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/RigelBuild/nebula/tools/alloy-log-caps/internal/echo"
)

const (
	drainDeadline = 180 * time.Second
	drainPoll     = 200 * time.Millisecond
	scrapeTimeout = 5 * time.Second
)

// Stream is one file Alloy tails. Paced lines follow the burst at PerSecond.
// Dump is appended first, to empty a burst that refilled meanwhile.
type Stream struct {
	Name   string
	Labels map[string]string
	Lines  []string
	Paced  *Paced
}

// Paced is the rate-limited tail of a Stream.
type Paced struct {
	Lines     []string
	PerSecond float64
	Dump      []string
}

// Run writes the Alloy config, starts Alloy, feeds the streams, and collects
// the echoed lines until every read line is echoed or counted as dropped.
func Run(ctx context.Context, label, alloy, pre, stages string, streams []Stream) (echo.Result, error) {
	return run(ctx, label, alloy, pre, stages, streams, drainDeadline)
}

func run(ctx context.Context, label, alloy, pre, stages string, streams []Stream, deadline time.Duration) (echo.Result, error) {
	dir, err := os.MkdirTemp("", "alloy-log-caps-"+label+"-")
	if err != nil {
		return echo.Result{}, fmt.Errorf("%s: temp dir: %w", label, err)
	}
	defer removeDir(dir)

	s := &session{label: label, dir: dir, streams: streams, paths: map[string]string{}, deadline: deadline}
	if err := s.writeInputs(pre, stages); err != nil {
		return echo.Result{}, err
	}
	port, err := freePort()
	if err != nil {
		return echo.Result{}, fmt.Errorf("%s: %w", label, err)
	}
	started := time.Now()
	if err := s.start(ctx, alloy, port); err != nil {
		return echo.Result{}, err
	}
	defer func() {
		if err := s.stop(); err != nil {
			slog.Warn("stop alloy", "label", label, "err", err)
		}
	}()
	if err := s.drain(ctx, port); err != nil {
		return echo.Result{}, err
	}

	// Drain finished. Stop Alloy now so the log is complete, then read it once.
	if err := s.stop(); err != nil {
		return echo.Result{}, fmt.Errorf("%s: stop alloy: %w", label, err)
	}
	out, err := echo.ReadEchoed(s.logPath)
	if err != nil {
		return echo.Result{}, fmt.Errorf("%s: %w", label, err)
	}
	out.Seconds = time.Since(started).Seconds()
	return out, nil
}

type session struct {
	deadline   time.Duration
	label      string
	dir        string
	logPath    string
	streams    []Stream
	paths      map[string]string
	total      int
	prewritten int
	process    *os.Process
	exited     chan struct{}
	exitCode   int
	pacers     sync.WaitGroup
	pacerMu    sync.Mutex
	pacerErr   error
	stopOnce   sync.Once
	stopErr    error
}

// writeInputs writes each stream file and the Alloy config that tails them.
func (s *session) writeInputs(pre, stages string) error {
	var targets []string
	for _, st := range s.streams {
		path := filepath.Join(s.dir, st.Name+".log")
		s.paths[st.Name] = path
		if err := os.WriteFile(path, []byte(joinLines(st.Lines)), 0o600); err != nil {
			return fmt.Errorf("%s: write %s: %w", s.label, path, err)
		}
		targets = append(targets, fmt.Sprintf("{\"__path__\" = %q, %s}", path, labelPairs(st.Labels, st.Name)))
		s.total += len(st.Lines)
		s.prewritten += len(st.Lines)
		if st.Paced != nil {
			s.total += len(st.Paced.Lines) + len(st.Paced.Dump)
		}
	}
	config := fmt.Sprintf(`local.file_match "f" { path_targets = [%s] }
loki.source.file "f" {
  targets       = local.file_match.f.targets
  tail_from_end = false
  forward_to    = [loki.process.p.receiver]
}
loki.process "p" {
  forward_to = [loki.echo.e.receiver]
%s
%s
}
loki.echo "e" {}
`, strings.Join(targets, ", "), pre, stages)
	if err := os.WriteFile(filepath.Join(s.dir, "config.alloy"), []byte(config), 0o600); err != nil {
		return fmt.Errorf("%s: write config: %w", s.label, err)
	}
	s.logPath = filepath.Join(s.dir, "alloy.log")
	return nil
}

// start launches Alloy with stdout and stderr both going to the log file.
func (s *session) start(ctx context.Context, alloy string, port int) error {
	logFile, err := os.Create(s.logPath)
	if err != nil {
		return fmt.Errorf("%s: create log: %w", s.label, err)
	}
	cmd := exec.CommandContext(ctx, alloy, "run", //nolint:gosec // The executable is the Alloy under test; argv values are passed without a shell.
		fmt.Sprintf("--server.http.listen-addr=127.0.0.1:%d", port),
		"--storage.path="+filepath.Join(s.dir, "data"),
		filepath.Join(s.dir, "config.alloy"),
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		closeQuietly(logFile)
		return fmt.Errorf("%s: start alloy: %w", s.label, err)
	}
	s.process = cmd.Process
	s.exited = make(chan struct{})
	go func() {
		waitErr := cmd.Wait()
		closeQuietly(logFile)
		if waitErr != nil {
			// Expected after our own SIGTERM; an early exit is reported by drain.
			slog.Debug("alloy wait", "label", s.label, "err", waitErr)
		}
		s.exitCode = cmd.ProcessState.ExitCode()
		close(s.exited)
	}()
	return nil
}

// stop terminates Alloy and waits for it and the pacers. It runs once; later
// calls return the first result.
func (s *session) stop() error {
	s.stopOnce.Do(func() {
		s.stopErr = s.terminate()
	})
	return s.stopErr
}

func (s *session) terminate() error {
	if s.process == nil {
		return errors.New("alloy was never started")
	}
	select {
	case <-s.exited:
	default:
		if err := s.process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("signal alloy: %w", err)
		}
	}
	<-s.exited
	s.pacers.Wait()
	return nil
}

// drain polls Alloy's exporter and log until every line is echoed or dropped.
func (s *session) drain(ctx context.Context, port int) error {
	deadline := time.Now().Add(s.deadline)
	pacing := false
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("%s: Alloy did not drain %d lines", s.label, s.total)
		}
		select {
		case <-s.exited:
			return s.exitError()
		case <-ctx.Done():
			return fmt.Errorf("%s: %w", s.label, ctx.Err())
		case <-time.After(drainPoll):
		}
		if err := s.pacerError(); err != nil {
			return fmt.Errorf("%s: %w", s.label, err)
		}
		metrics, err := fetchMetrics(ctx, port)
		if err != nil {
			// A failed scrape reads as no samples; the deadline bounds the retries.
			slog.Debug("scrape alloy metrics", "label", s.label, "err", err)
		}
		dropped := echo.MetricSum(metrics, "loki_process_dropped_lines_total")
		parsed, err := echo.ReadEchoed(s.logPath)
		if err != nil {
			return fmt.Errorf("%s: %w", s.label, err)
		}
		echoed := float64(parsed.Count())
		if !pacing && echoed+dropped >= float64(s.prewritten) {
			s.startPacers()
			pacing = true
		}
		if echoed+dropped >= float64(s.total) {
			return nil
		}
	}
}

func (s *session) exitError() error {
	tail, err := tailFile(s.logPath, 2000)
	if err != nil {
		return fmt.Errorf("%s: Alloy exited %d; log unreadable: %w", s.label, s.exitCode, err)
	}
	return fmt.Errorf("%s: Alloy exited %d:\n%s", s.label, s.exitCode, tail)
}

// startPacers appends each paced stream at its rate once the burst is drained.
func (s *session) startPacers() {
	for _, st := range s.streams {
		if st.Paced == nil {
			continue
		}
		path := s.paths[st.Name]
		p := st.Paced
		s.pacers.Go(func() { s.pace(path, p) })
	}
}

// pace writes the paced lines to path, keyed to the wall clock.
func (s *session) pace(path string, p *Paced) {
	if err := appendLines(path, p.Dump); err != nil {
		s.setPacerError(err)
		return
	}
	begin := time.Now()
	for sent := 0; sent < len(p.Lines); {
		select {
		case <-s.exited:
			return
		case <-time.After(time.Duration(float64(time.Second) / p.PerSecond)):
		}
		// Write what is due by the wall clock, so a slow runner still offers PerSecond.
		due := min(len(p.Lines), int(math.Floor(time.Since(begin).Seconds()*p.PerSecond)))
		if due <= sent {
			continue
		}
		if err := appendLines(path, p.Lines[sent:due]); err != nil {
			s.setPacerError(err)
			return
		}
		sent = due
	}
}

func (s *session) setPacerError(err error) {
	s.pacerMu.Lock()
	defer s.pacerMu.Unlock()
	if s.pacerErr == nil {
		s.pacerErr = err
	}
}

func (s *session) pacerError() error {
	s.pacerMu.Lock()
	defer s.pacerMu.Unlock()
	return s.pacerErr
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("free port: %w", err)
	}
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		closeQuietly(l)
		return 0, fmt.Errorf("probe listener address is %T, want *net.TCPAddr", l.Addr())
	}
	port := addr.Port
	if err := l.Close(); err != nil {
		return 0, fmt.Errorf("close probe listener: %w", err)
	}
	return port, nil
}

func removeDir(dir string) {
	if err := os.RemoveAll(dir); err != nil {
		slog.Warn("remove temp dir", "dir", dir, "err", err)
	}
}

func closeQuietly(c io.Closer) {
	if err := c.Close(); err != nil {
		slog.Warn("close file", "err", err)
	}
}

func joinLines(lines []string) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return b.String()
}

// appendLines appends a batch to a stream file.
func appendLines(path string, lines []string) error {
	if len(lines) == 0 {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // path is a stream file this run created in its temp directory.
	if err != nil {
		return fmt.Errorf("open %s for append: %w", path, err)
	}
	if _, err := f.WriteString(joinLines(lines)); err != nil {
		closeQuietly(f)
		return fmt.Errorf("append %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}

// labelPairs renders the Alloy label list for one stream, with stream_id last.
func labelPairs(labels map[string]string, name string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys)+1)
	for _, k := range keys {
		pairs = append(pairs, fmt.Sprintf("%q = %q", k, labels[k]))
	}
	pairs = append(pairs, fmt.Sprintf("%q = %q", "stream_id", name))
	return strings.Join(pairs, ", ")
}

// fetchMetrics scrapes Alloy's exporter.
func fetchMetrics(ctx context.Context, port int) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, scrapeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/metrics", port), nil)
	if err != nil {
		return "", fmt.Errorf("build scrape request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("scrape alloy: %w", err)
	}
	defer closeQuietly(resp.Body)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read scrape: %w", err)
	}
	return string(body), nil
}

func tailFile(path string, n int) (string, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // path is the Alloy log this run created in its temp directory.
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	if len(raw) > n {
		raw = raw[len(raw)-n:]
	}
	return string(raw), nil
}
