package scrapelimit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/RigelBuild/nebula/tools/alloy-harness/internal/alloyproc"
	"github.com/RigelBuild/nebula/tools/alloy-harness/internal/check"
)

const (
	pollEvery = 500 * time.Millisecond
	// A verdict must settle within this, or the run errors with the evidence.
	verdictDeadline = 90 * time.Second
	scrapeTimeout   = 5 * time.Second
)

// Result is what one Alloy run observed for its target.
type Result struct {
	Exceeded float64
	Hits     int64
}

// fixture serves one /metrics page and counts the scrapes it answers.
type fixture struct {
	srv  *http.Server
	ln   net.Listener
	hits atomic.Int64
}

func serveFixture(samples int) (*fixture, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for fixture: %w", err)
	}
	f := &fixture{ln: ln}
	body := Body(samples)
	f.srv = &http.Server{
		ReadHeaderTimeout: scrapeTimeout,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			f.hits.Add(1)
			w.Header().Set("Content-Type", "text/plain")
			if _, err := io.WriteString(w, body); err != nil {
				slog.Warn("write fixture body", "err", err)
			}
		}),
	}
	go func() {
		if err := f.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("serve fixture", "err", err)
		}
	}()
	return f, nil
}

func (f *fixture) port() (int, error) {
	addr, ok := f.ln.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("fixture listener address is %T, want *net.TCPAddr", f.ln.Addr())
	}
	return addr.Port, nil
}

func (f *fixture) close() error {
	return f.srv.Close()
}

// Scrape runs Alloy against a fixture serving samples, with sample_limit limit, and
// returns the refusal count and the number of scrapes answered.
func Scrape(ctx context.Context, label, alloy string, samples, limit int) (Result, error) {
	fx, err := serveFixture(samples)
	if err != nil {
		return Result{}, err
	}
	dir, err := os.MkdirTemp("", "alloy-scrape-limits-"+label+"-")
	if err != nil {
		return Result{}, errors.Join(fmt.Errorf("%s: make temp dir: %w", label, err), fx.close())
	}
	defer alloyproc.RemoveDir(dir)
	config := filepath.Join(dir, "config.alloy")
	fxPort, err := fx.port()
	if err != nil {
		return Result{}, errors.Join(err, fx.close())
	}
	if err := alloyproc.WriteConfig(config, Config(fxPort, limit)); err != nil {
		return Result{}, errors.Join(fmt.Errorf("%s: %w", label, err), fx.close())
	}
	port, err := alloyproc.FreePort()
	if err != nil {
		return Result{}, errors.Join(err, fx.close())
	}
	proc, err := alloyproc.Start(alloy, dir, config, port)
	if err != nil {
		return Result{}, errors.Join(err, fx.close())
	}
	res, runErr := poll(ctx, label, samples, limit, port, proc, fx)
	return res, errors.Join(runErr, proc.Stop(), fx.close())
}

// poll waits until the verdict settles, Alloy exits, or the deadline passes.
func poll(ctx context.Context, label string, samples, limit, port int, proc *alloyproc.Proc, fx *fixture) (Result, error) {
	tick := time.NewTicker(pollEvery)
	defer tick.Stop()
	deadline := time.NewTimer(verdictDeadline)
	defer deadline.Stop()
	for {
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-proc.Done():
			return Result{}, fmt.Errorf("%s: Alloy exited %d:\n%s", label, proc.ExitCode(), alloyproc.Tail(proc.LogPath, 2000))
		case <-deadline.C:
			page := scrapePage(ctx, port)
			return Result{}, fmt.Errorf("%s: fixture answered %d scrapes, exceeded=%v within %s\n%s\nalloy.log tail:\n%s",
				label, fx.hits.Load(), ExceededTotal(page), verdictDeadline, ScrapeLines(page), alloyproc.Tail(proc.LogPath, 2000))
		case <-tick.C:
			exceeded := ExceededTotal(scrapePage(ctx, port))
			hits := fx.hits.Load()
			if Settled(samples, limit, hits, exceeded) {
				return Result{Exceeded: exceeded, Hits: hits}, nil
			}
		}
	}
}

// scrapePage reads Alloy's own /metrics. A failed read is an empty page: the poll retries,
// and the deadline reports the evidence if it never settles.
func scrapePage(ctx context.Context, port int) string {
	ctx, cancel := context.WithTimeout(ctx, scrapeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/metrics", port), nil)
	if err != nil {
		return ""
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Warn("close metrics body", "err", err)
		}
	}()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return ""
	}
	return string(raw)
}

// Checks returns the named assertions in the order the harness prints them. A run that
// errors never reaches Checks: its error ends the harness with exit 1.
func Checks(over, at Result) []check.Check {
	return []check.Check{
		{Name: fmt.Sprintf("65 samples with sample_limit 64 raise %s (got %v over %d scrapes)", Exceeded, over.Exceeded, over.Hits), OK: over.Exceeded >= 1},
		{Name: fmt.Sprintf("64-sample target answered %d scrapes", at.Hits), OK: at.Hits >= MinHits},
		{Name: fmt.Sprintf("64 samples with sample_limit 64 scrape cleanly (%s = %v)", Exceeded, at.Exceeded), OK: at.Exceeded == 0},
	}
}
