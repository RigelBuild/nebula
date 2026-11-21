// Command dev-hygiene prunes Go tool caches and reclaims idle jj workspaces on dev hosts.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

const (
	defaultIdle     = 48 * time.Hour
	defaultCacheAge = 24 * time.Hour
)

type config struct {
	roots []string
	// defaultRoot is set when roots came from the default, not -root. A missing default root is skipped, not an error.
	defaultRoot bool
	stateDir    string
	reportTo    string
	cotalBin    string
	to          string
	server      string
	creds       string
	idle        time.Duration
	cacheAge    time.Duration
	apply       bool
}

// rootFlags is a repeatable -root flag. Each occurrence adds one workspaces root to scan.
type rootFlags []string

func (r *rootFlags) String() string { return fmt.Sprint([]string(*r)) }

func (r *rootFlags) Set(value string) error {
	*r = append(*r, value)
	return nil
}

// printer writes lines and keeps the first write failure, so call sites stay linear.
// debug, when set, receives per-workspace verdict reasons. Production leaves it nil; tests route it to t.Logf.
type printer struct {
	w     io.Writer
	err   error
	debug func(format string, args ...any)
}

func (p *printer) printf(format string, args ...any) {
	if p.err != nil {
		return
	}
	_, p.err = fmt.Fprintf(p.w, format, args...)
}

// debugf reports a decision reason. It never affects output or errors.
func (p *printer) debugf(format string, args ...any) {
	if p.debug != nil {
		p.debug(format, args...)
	}
}

func main() {
	// SIGTERM from systemd or launchd cancels the run. A cancel can stop a delete partway; the marker lets the next run finish it.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cfg, err := parseFlags(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		return failure(stderr, 2, err)
	}

	out := &printer{w: stdout}
	out.printf("dev-hygiene: %s, roots %v\n", modeName(cfg.apply), cfg.roots)

	var errs []error
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		errs = append(errs, fmt.Errorf("resolve user cache dir: %w", err))
	} else {
		errs = append(errs, pruneGoCaches(ctx, cacheDir, time.Now().Add(-cfg.cacheAge), cfg.apply, out))
	}

	// One live-cwd scan per run. A failed scan skips workspace work entirely: it never guesses "none live".
	live, err := liveCwds(ctx)
	if err != nil {
		errs = append(errs, fmt.Errorf("live cwd scan: %w", err))
	} else {
		kept, err := cleanWorkspaces(ctx, cfg, live, out)
		errs = append(errs, err)
		if len(kept) > 0 {
			errs = append(errs, reportKeptRun(ctx, cfg, kept, out))
		}
	}
	if out.err != nil {
		errs = append(errs, fmt.Errorf("write output: %w", out.err))
	}

	if err := errors.Join(errs...); err != nil {
		// A failed apply run DMs Nemo too: the scheduled units are otherwise fire-and-forget.
		if cfg.apply {
			host, hostErr := os.Hostname()
			if hostErr != nil {
				host = "unknown host"
			}
			if sendErr := sendDM(ctx, cfg, failureText(host, err)); sendErr != nil {
				err = errors.Join(err, sendErr)
			}
		}
		return failure(stderr, 1, err)
	}
	return 0
}

// reportKeptRun loads the reported set, sends the new entries, and saves the set only when the send succeeded.
func reportKeptRun(ctx context.Context, cfg config, kept []kept, out *printer) error {
	st, err := loadReportState(stateFile(cfg.stateDir))
	if err != nil {
		return err
	}
	st, err = reportKept(ctx, cfg, kept, st, cfg.reportTo, out)
	if err != nil {
		return err
	}
	if !cfg.apply {
		return nil
	}
	return saveReportState(stateFile(cfg.stateDir), pruneReported(st))
}

// failure reports err on stderr and returns code. A failed stderr write leaves no channel to report on, so it returns 2.
func failure(stderr io.Writer, code int, err error) int {
	if _, writeErr := fmt.Fprintf(stderr, "dev-hygiene: %v\n", err); writeErr != nil {
		return 2
	}
	return code
}

func parseFlags(args []string, stderr io.Writer) (config, error) {
	var cfg config
	flags := flag.NewFlagSet("dev-hygiene", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Var((*rootFlags)(&cfg.roots), "root", "Workspaces root to scan; repeat for more. Default: $HOME/agents/workspaces.")
	flags.StringVar(&cfg.stateDir, "state-dir", "", "State dir for the reported set. Default: $XDG_STATE_HOME or ~/.local/state.")
	flags.StringVar(&cfg.reportTo, "report-file", "", "Full list of kept workspaces. Default: <state-dir>/dev-hygiene/kept.txt.")
	flags.StringVar(&cfg.cotalBin, "cotal", "cotal", "Cotal binary used to send the report DM.")
	flags.StringVar(&cfg.to, "to", "nemo", "Cotal seat that receives the report DM.")
	flags.StringVar(&cfg.server, "server", "", "NATS server for cotal send, as nemoAlertSend passes it. Empty uses the cotal config.")
	flags.StringVar(&cfg.creds, "creds", "", "NATS credentials file for cotal send. Empty uses the cotal config.")
	flags.DurationVar(&cfg.idle, "idle", defaultIdle, "Idle window before a workspace is eligible for cleanup.")
	flags.DurationVar(&cfg.cacheAge, "cache-age", defaultCacheAge, "Go cache files untouched this long are pruned.")
	flags.BoolVar(&cfg.apply, "apply", false, "Delete workspaces, prune caches and send the report DM. Default: dry run.")
	if err := flags.Parse(args); err != nil {
		return config{}, err
	}
	if flags.NArg() != 0 {
		return config{}, fmt.Errorf("unexpected arguments: %v", flags.Args())
	}
	// A non-positive window would make every workspace look idle.
	if cfg.idle <= 0 || cfg.cacheAge <= 0 {
		return config{}, errors.New("-idle and -cache-age must be positive")
	}
	if len(cfg.roots) == 0 {
		home, err := os.UserHomeDir()
		if err != nil {
			return config{}, fmt.Errorf("resolve home: %w", err)
		}
		cfg.roots = []string{filepath.Join(home, "agents", "workspaces")}
		cfg.defaultRoot = true
	}
	if cfg.stateDir == "" {
		dir, err := defaultStateDir()
		if err != nil {
			return config{}, err
		}
		cfg.stateDir = dir
	}
	if cfg.reportTo == "" {
		cfg.reportTo = filepath.Join(cfg.stateDir, "dev-hygiene", "kept.txt")
	}
	return cfg, nil
}

// defaultStateDir follows the XDG base directory spec: $XDG_STATE_HOME, else ~/.local/state.
func defaultStateDir() (string, error) {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home: %w", err)
	}
	return filepath.Join(home, ".local", "state"), nil
}

func modeName(apply bool) string {
	if apply {
		return "apply"
	}
	return "dry run"
}
