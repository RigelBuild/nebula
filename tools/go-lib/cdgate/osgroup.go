package cdgate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec" //nolint:depguard // The deploy group runs the supplied argv directly, without a shell.
	"sync"
	"syscall"
)

// OSRunner starts commands in their own process group, so a lost Lease can
// signal the whole group and its descendants. It is the production Runner.
type OSRunner struct {
	Stdin io.Reader
	// Env is the deploy environment. It must not carry the CD API token.
	Env []string
}

// Start runs argv in a new process group. Output copies join after the executor
// has stopped any descendants that could inherit the pipe writers.
func (r OSRunner) Start(argv []string, stdout, stderr io.Writer) (Process, error) {
	if len(argv) == 0 {
		return nil, errors.New("deploy command is empty")
	}
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // argv is the configured deploy command, not untrusted input.
	cmd.Env = r.Env
	cmd.Stdin = r.Stdin
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	copies := &outputCopies{}
	stdoutRead, stdoutWrite, err := processOutput(cmd, stdout, true)
	if err != nil {
		return nil, err
	}
	stderrRead, stderrWrite, err := processOutput(cmd, stderr, false)
	if err != nil {
		return nil, errors.Join(err, closeOutputFiles(stdoutRead, stdoutWrite))
	}
	closeReadsOnError := func(err error) error {
		return errors.Join(err, closeOutputFiles(stdoutRead, stdoutWrite, stderrRead, stderrWrite))
	}
	if err := cmd.Start(); err != nil {
		return nil, closeReadsOnError(err)
	}
	if err := closeOutputFiles(stdoutWrite, stderrWrite); err != nil {
		killErr := cmd.Process.Kill()
		waitErr := cmd.Wait()
		return nil, closeReadsOnError(errors.Join(err, killErr, waitErr))
	}
	if stdoutRead != nil {
		copies.wg.Add(1)
		go copyOutput(copies, stdout, stdoutRead)
	}
	if stderrRead != nil {
		copies.wg.Add(1)
		go copyOutput(copies, stderr, stderrRead)
	}
	return &osProcess{cmd: cmd, copies: copies}, nil
}

func processOutput(cmd *exec.Cmd, dst io.Writer, stdout bool) (*os.File, *os.File, error) {
	if dst == nil || dst == io.Discard {
		return nil, nil, nil
	}
	if file, ok := dst.(*os.File); ok {
		if stdout {
			cmd.Stdout = file
		} else {
			cmd.Stderr = file
		}
		return nil, nil, nil
	}
	read, write, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	if stdout {
		cmd.Stdout = write
	} else {
		cmd.Stderr = write
	}
	return read, write, nil
}

func closeOutputFiles(files ...*os.File) error {
	var errs []error
	for _, file := range files {
		if file != nil {
			if err := file.Close(); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

type outputCopies struct {
	wg  sync.WaitGroup
	mu  sync.Mutex
	err error
}

func (c *outputCopies) addError(err error) {
	c.mu.Lock()
	c.err = errors.Join(c.err, err)
	c.mu.Unlock()
}

func (c *outputCopies) result() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func copyOutput(copies *outputCopies, dst io.Writer, src *os.File) {
	defer copies.wg.Done()
	if _, err := io.Copy(dst, src); err != nil {
		copies.addError(fmt.Errorf("copy deploy output: %w", err))
	}
	if err := src.Close(); err != nil {
		copies.addError(fmt.Errorf("close deploy output pipe: %w", err))
	}
}

// osProcess is one started deploy group. The group id is the child's pid,
// because the child leads its own group.
type osProcess struct {
	cmd    *exec.Cmd
	copies *outputCopies
}

func (p *osProcess) JoinOutput(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		p.copies.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return p.copies.result()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *osProcess) PID() int { return p.cmd.Process.Pid }

// Wait returns the child's exit code. A child killed by a signal reports
// 128 plus the signal number, as a shell does.
func (p *osProcess) Wait() (int, error) {
	err := p.cmd.Wait()
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	exitErr, ok := errors.AsType[*exec.ExitError](err)
	if ok {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal()), nil
		}
		return exitErr.ExitCode(), nil
	}
	return 0, err
}

// OSGroupProbe probes and signals a real process group.
type OSGroupProbe struct{}

// Signal sends sig to the process group pid. A group that is already gone is
// not an error: the stop has what it wanted.
func (OSGroupProbe) Signal(pid int, sig syscall.Signal) error {
	err := syscall.Kill(-pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// Alive reports whether group pid still has a live member. It sends signal 0
// to test existence. EPERM means the signal was refused, so the process table
// decides: a zombie does not count, and any other member does.
func (OSGroupProbe) Alive(pid int) bool {
	alive, err := SignalGroup(pid, 0, syscall.Kill, listGroups)
	return err == nil && alive
}

// listGroups returns the `ps -A -o pgid=,stat=` table that SignalGroup reads to
// tell a live member from a zombie after EPERM.
func listGroups() (string, error) {
	out, err := exec.Command("ps", "-A", "-o", "pgid=,stat=").Output()
	return string(out), err
}

// Ensure OSRunner and OSGroupProbe satisfy the seams at compile time.
var (
	_ Runner     = OSRunner{}
	_ GroupProbe = OSGroupProbe{}
)
