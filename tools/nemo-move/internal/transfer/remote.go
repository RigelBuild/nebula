package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec" //nolint:depguard // SSH is the remote transport boundary.
	"path/filepath"
	"strings"
)

// Remote runs checked SSH commands and streams command output without buffering it.
type Remote interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
	StreamReader(ctx context.Context, name string, args ...string) (io.ReadCloser, error)
	Upload(ctx context.Context, sshPath, target, localPath, remotePath string, mode os.FileMode) error
	Hash(ctx context.Context, sshPath, target, remotePath string) (string, error)
}

type sshRemote struct{}

// SSHRemote returns the production SSH transport.
func SSHRemote() Remote { return sshRemote{} }

func (sshRemote) Upload(ctx context.Context, sshPath, target, localPath, remotePath string, mode os.FileMode) error {
	if !filepath.IsAbs(sshPath) {
		return errors.New("SSH executable must be an absolute path")
	}
	if _, err := splitTarget(target); err != nil {
		return err
	}
	file, err := os.Open(localPath) //nolint:gosec // Local binary is selected by the operator.
	if err != nil {
		return fmt.Errorf("open mac binary: %w", err)
	}
	defer file.Close() //nolint:errcheck // Read-only file.
	script := "set -eu; dest=\"$HOME/\"" + ShellQuote(remotePath) + "; " +
		"/bin/mkdir -p \"${dest%/*}\"; tmp=$(/usr/bin/mktemp \"${dest}.XXXXXX\"); " +
		"trap '/bin/rm -f \"$tmp\"' EXIT; /bin/cat > \"$tmp\"; /bin/chmod " +
		fmt.Sprintf("%04o", mode.Perm()) + " \"$tmp\"; /bin/mv -f \"$tmp\" \"$dest\""
	command := exec.CommandContext(ctx, sshPath, "-o", "BatchMode=yes", target, "/bin/sh -c "+ShellQuote(script)) //nolint:gosec // SSH path is selected by the caller.
	command.Stdin = file
	var stderr strings.Builder
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("upload mac binary: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (sshRemote) Hash(ctx context.Context, sshPath, target, remotePath string) (string, error) {
	if !filepath.IsAbs(sshPath) {
		return "", errors.New("SSH executable must be an absolute path")
	}
	if _, err := splitTarget(target); err != nil {
		return "", err
	}
	script := "/usr/bin/shasum -a 256 \"$HOME/\"" + ShellQuote(remotePath)
	output, err := (sshRemote{}).Run(ctx, sshPath, "-o", "BatchMode=yes", target, "/bin/sh -c "+ShellQuote(script))
	if err != nil {
		return "", err
	}
	fields := strings.Fields(output)
	if len(fields) == 0 {
		return "", errors.New("empty remote hash")
	}
	return fields[0], nil
}

func (sshRemote) Run(ctx context.Context, name string, args ...string) (string, error) {
	var stdout, stderr strings.Builder
	command := exec.CommandContext(ctx, name, args...) //nolint:gosec // ssh path is resolved to an absolute executable.
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func (sshRemote) StreamReader(ctx context.Context, name string, args ...string) (io.ReadCloser, error) {
	command := exec.CommandContext(ctx, name, args...) //nolint:gosec // ssh path is resolved to an absolute executable.
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("create SSH stdout pipe: %w", err)
	}
	var stderr strings.Builder
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start SSH command: %w", err)
	}
	return &commandReader{stdout: stdout, command: command, stderr: &stderr}, nil
}

type commandReader struct {
	stdout  io.ReadCloser
	command *exec.Cmd
	stderr  *strings.Builder
}

func (reader *commandReader) Read(data []byte) (int, error) { return reader.stdout.Read(data) }

func (reader *commandReader) Close() error {
	closeErr := reader.stdout.Close()
	waitErr := reader.command.Wait()
	if waitErr != nil {
		waitErr = fmt.Errorf("%s: %w: %s", reader.command.Path, waitErr, strings.TrimSpace(reader.stderr.String()))
	}
	if err := errors.Join(closeErr, waitErr); err != nil {
		return err
	}
	return nil
}
