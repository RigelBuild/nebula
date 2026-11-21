package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/samber/oops"
)

const (
	outputText        = "text"
	outputJSON        = "json"
	usageStage        = "usage"
	toolFailureGate   = "tool-failure"
	submitCommandName = "submit"
)

type failure struct {
	stage, gate, reason, fix string
	code                     int
	cause                    error
}

func (f *failure) Error() string { return f.stage + ": " + f.gate + ": " + f.reason }
func (f *failure) Unwrap() error { return f.cause }
func refusal(stage, gate, reason, fix string) error {
	return &failure{stage: stage, gate: gate, reason: reason, fix: fix, code: 1}
}
func toolFailure(stage string, err error) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*failure](err); ok {
		return err
	}
	return &failure{stage: stage, gate: toolFailureGate, reason: err.Error(), fix: "inspect the full log and repair the failing tool before rerunning", code: 3, cause: err}
}

type cli struct {
	output, codename, profilesFlag, message, logPath string
	stdout, stderr                                   io.Writer
	logfile                                          *os.File
	restoreLog                                       func()
	statusRows                                       []StatusRow
	ran                                              bool
	cfg                                              *Config
}

// config resolves and loads the caller's profile file on first use, then returns the cached result. Every command
// reads one load, so the file is never parsed twice in a run.
func (c *cli) config() (Config, error) {
	if c.cfg != nil {
		return *c.cfg, nil
	}
	path, err := resolveProfilesPath(c.profilesFlag)
	if err != nil {
		return Config{}, &failure{stage: usageStage, gate: "profiles", reason: err.Error(), code: 2}
	}
	cfg, err := loadConfig(path)
	if err != nil {
		return Config{}, toolFailure("preflight", err)
	}
	c.cfg = &cfg
	return cfg, nil
}
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	c := &cli{output: outputText, stdout: stdout, stderr: stderr}
	root, err := c.root()
	if err != nil {
		return 3
	}
	root.SetArgs(args)
	err = root.ExecuteContext(ctx)
	if c.restoreLog != nil {
		c.restoreLog()
	}
	if c.logfile != nil {
		if err != nil {
			_, writeErr := fmt.Fprintln(c.logfile, err)
			err = errors.Join(err, writeErr)
		}
		err = errors.Join(err, c.logfile.Close())
	}
	return c.render(err)
}

func (c *cli) initialize(ctx context.Context) (context.Context, *repository, error) {
	c.ran = true
	if c.output != outputText && c.output != outputJSON {
		return ctx, nil, &failure{stage: usageStage, gate: "output", reason: "-o must be text or json", code: 2}
	}
	s, _ := ctx.Value(servicesKey{}).(*services)
	var r *repository
	if s != nil {
		r = s.jj
	}
	state := os.Getenv("XDG_STATE_HOME")
	if r != nil {
		state = filepath.Dir(r.stateDir)
	}
	if state == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ctx, nil, toolFailure("preflight", err)
		}
		state = filepath.Join(home, ".local", "state")
	}
	logDir := filepath.Join(state, "rjc", "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil { //nolint:gosec // The state directory is explicitly selected by the caller's environment.
		return ctx, nil, toolFailure("preflight", oops.Wrapf(err, "create log directory"))
	}
	var err error
	c.logfile, err = os.CreateTemp(logDir, "run-*.log")
	if err != nil {
		return ctx, nil, toolFailure("preflight", err)
	}
	c.logPath = c.logfile.Name()
	if r == nil {
		cfg, err := c.config()
		if err != nil {
			return ctx, nil, err
		}
		r, err = preflight(ctx, cfg, c.codename, c.logfile)
		if err != nil {
			return ctx, nil, err
		}
	}
	previousLog := r.log
	c.restoreLog = func() { r.log = previousLog }
	r.log = c.logfile
	if s == nil {
		s = &services{}
	}
	s.jj = r
	return withServices(ctx, s), r, nil
}

func (c *cli) render(err error) int {
	if err != nil {
		return c.renderFailure(err)
	}
	if c.statusRows != nil && c.output == outputJSON {
		if err := json.NewEncoder(c.stdout).Encode(c.statusRows); err != nil {
			return 3
		}
		return 0
	}
	if c.output == outputJSON {
		if err := json.NewEncoder(c.stdout).Encode(map[string]any{"ok": true, "message": c.message, "log": c.logPath}); err != nil {
			return 3
		}
		return 0
	}
	if c.message != "" {
		if _, err := fmt.Fprintf(c.stdout, "%s\nFull log: %s\n", c.message, c.logPath); err != nil {
			return 3
		}
	}
	return 0
}

func (c *cli) renderFailure(err error) int {
	code := 3
	if !c.ran {
		code = 2
	}
	detail := &failure{stage: usageStage, gate: "arguments", fix: "rjc --help"}
	if errors.As(err, &detail) {
		code = detail.code
	}
	if c.output == outputJSON {
		if encodeErr := json.NewEncoder(c.stderr).Encode(map[string]any{"ok": false, "exitCode": code, "error": err.Error(), "stage": detail.stage, "gate": detail.gate, "fix": detail.fix, "log": c.logPath}); encodeErr != nil {
			return 3
		}
		return code
	}
	if _, writeErr := fmt.Fprintf(c.stderr, "%s\nFix: %s\nFull log: %s\n", err, detail.fix, c.logPath); writeErr != nil {
		return 3
	}
	return code
}
