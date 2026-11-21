package seat

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/exec" //nolint:depguard // The holder drives tmux, systemctl and wave-host directly, without a shell.
	"runtime"
	"syscall"
	"time"

	"github.com/RigelBuild/nebula/tools/wave-seat-holder/internal/socket"
)

const connectTimeout = 250 * time.Millisecond

// ProductionDeps wires every holder effect to the real OS.
func ProductionDeps(getenv socket.Environment, stdout, stderr io.Writer) Deps {
	return Deps{
		Platform: runtime.GOOS,
		Getenv:   getenv,
		Cwd:      os.Getwd,
		Now:      time.Now,
		Sleep:    sleepContext,
		Files: Files{
			Mkdir:     os.MkdirAll,
			Lstat:     os.Lstat,
			Chmod:     os.Chmod,
			Getuid:    os.Getuid,
			WriteFile: writePrivateFile,
			Readdir:   readDirNames,
			ReadText:  readFileText,
			Connect:   connectSocket,
		},
		Which:  lookPath,
		Spawn:  spawnCommand,
		Stdout: emitTo(stdout),
		Stderr: emitTo(stderr),
	}
}

// emitTo writes each line to w. A write failure is logged because the holder has no other channel for it.
func emitTo(w io.Writer) func(string) {
	return func(line string) {
		if _, err := io.WriteString(w, line); err != nil {
			slog.Error("writing wave-seat-holder output", "error", err)
		}
	}
}

// seatConfig is the tmux configuration every seat runs with, embedded so the released binary needs no share directory.
//
//go:embed seat.conf
var seatConfig []byte

// writePrivateFile writes data to path and sets perm even when the file already exists, so the mode is never left stale.
func writePrivateFile(path string, data []byte, perm fs.FileMode) error {
	//nolint:gosec // path is the seat's own directory, created by the holder.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, perm)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Chmod(perm); err != nil {
		return errors.Join(err, file.Close())
	}
	return file.Close()
}

func lookPath(command string) (string, bool) {
	path, err := exec.LookPath(command)
	return path, err == nil
}

func readFileText(path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // Paths are the seat socket directory or a /proc entry the holder builds.
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func readDirNames(path string) ([]string, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names, nil
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// connectSocket reports whether a live server accepts a connection on path within connectTimeout.
func connectSocket(ctx context.Context, path string) bool {
	dialer := net.Dialer{Timeout: connectTimeout}
	conn, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return false
	}
	// The successful dial already answers the question; a close failure is only logged.
	if err := conn.Close(); err != nil {
		slog.Error("closing probe connection", "error", err)
	}
	return true
}

// spawnCommand runs command without a shell. A non-zero exit is a result; only a failure to start is an error.
func spawnCommand(ctx context.Context, command string, args []string, inherit bool) (CommandResult, error) {
	cmd := exec.CommandContext(ctx, command, args...) //nolint:gosec // argv is built by the holder and passed without a shell.
	var stdout, stderr bytes.Buffer
	if inherit {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	} else {
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
	}
	err := cmd.Run()
	result := CommandResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		result.Code = exitErr.ExitCode()
		return result, nil
	}
	if err != nil {
		return CommandResult{}, err
	}
	return result, nil
}
