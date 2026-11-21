// Package alloyproc runs one Alloy process for a harness and reads its log.
package alloyproc

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec" //nolint:depguard // Alloy is the system under test; it runs from argv, never through a shell.
	"path/filepath"
	"syscall"
)

// Proc is one running Alloy. Its stdout and stderr go to LogPath.
type Proc struct {
	LogPath  string
	cmd      *exec.Cmd
	logFile  *os.File
	done     chan struct{}
	waitErr  error
	closeErr error
}

// Start runs alloy on config, with its HTTP listener on port and its data under dir.
func Start(alloy, dir, config string, port int) (*Proc, error) {
	logPath := filepath.Join(dir, "alloy.log")
	logFile, err := os.Create(logPath) //nolint:gosec // logPath is the Alloy log in the temp directory this run created.
	if err != nil {
		return nil, fmt.Errorf("create alloy log: %w", err)
	}
	cmd := exec.Command(alloy, "run", //nolint:gosec // The executable is the Alloy under test; argv values are passed without a shell.
		fmt.Sprintf("--server.http.listen-addr=127.0.0.1:%d", port),
		"--storage.path="+filepath.Join(dir, "data"),
		config,
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		return nil, errors.Join(fmt.Errorf("start alloy: %w", err), logFile.Close())
	}
	p := &Proc{LogPath: logPath, cmd: cmd, logFile: logFile, done: make(chan struct{})}
	go p.wait()
	return p, nil
}

// Done closes once Alloy has exited.
func (p *Proc) Done() <-chan struct{} {
	return p.done
}

// ExitCode is Alloy's exit code. Call it only after Done closes.
func (p *Proc) ExitCode() int {
	return p.cmd.ProcessState.ExitCode()
}

// Stop terminates Alloy unless it already exited, then waits for it. A signal exit is
// the normal end here, so only other wait or close failures are returned.
func (p *Proc) Stop() error {
	select {
	case <-p.done:
	default:
		if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("signal alloy: %w", err)
		}
	}
	<-p.done
	var exitErr *exec.ExitError
	if p.waitErr != nil && !errors.As(p.waitErr, &exitErr) {
		return errors.Join(fmt.Errorf("wait for alloy: %w", p.waitErr), p.closeErr)
	}
	return p.closeErr
}

// wait reaps Alloy and closes its log once it exits.
func (p *Proc) wait() {
	p.waitErr = p.cmd.Wait()
	p.closeErr = p.logFile.Close()
	close(p.done)
}

// FreePort returns a loopback TCP port that is free right now.
func FreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("listen for a free port: %w", err)
	}
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		return 0, errors.Join(fmt.Errorf("listener address is %T, want *net.TCPAddr", l.Addr()), l.Close())
	}
	if err := l.Close(); err != nil {
		return 0, fmt.Errorf("release free port: %w", err)
	}
	return addr.Port, nil
}

// ReadLog returns the Alloy log at path as it is now.
func ReadLog(path string) (string, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // path is the Alloy log this run created in its own temp directory.
	if err != nil {
		return "", fmt.Errorf("read alloy log: %w", err)
	}
	return string(raw), nil
}

// ReadConfig returns the rendered host config the Nix check passes on argv.
func ReadConfig(path string) (string, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // path is the rendered host config passed on argv by the Nix check.
	if err != nil {
		return "", fmt.Errorf("read rendered config: %w", err)
	}
	return string(raw), nil
}

// WriteConfig writes an Alloy config into a temp directory this run created.
func WriteConfig(path, body string) error {
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return fmt.Errorf("write alloy config: %w", err)
	}
	return nil
}

// Tail returns the last n bytes of the log at path. A read failure is reported as text,
// because the tail is only diagnostics for a failure that is already being returned.
func Tail(path string, n int) string {
	raw, err := ReadLog(path)
	if err != nil {
		return err.Error()
	}
	if len(raw) > n {
		raw = raw[len(raw)-n:]
	}
	return raw
}

// RemoveDir deletes a temp dir this run created. A failure is logged, not returned,
// because the run's own result does not depend on the cleanup.
func RemoveDir(dir string) {
	if err := os.RemoveAll(dir); err != nil {
		slog.Warn("remove temp dir", "dir", dir, "err", err)
	}
}
