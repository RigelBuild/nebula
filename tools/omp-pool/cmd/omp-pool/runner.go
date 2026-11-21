// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec" //nolint:depguard // OMP and SSH need argv-based child execution with isolated environments.
	"strconv"
	"strings"
	"sync"
	"time"
)

type commandRunner interface {
	RunOMP(ctx context.Context, args, env []string, stdin io.Reader, stdout, stderr io.Writer) error
	ReadToken(ctx context.Context, d deployment) (string, error)
	StartForward(ctx context.Context, d deployment, remotePort int) (brokerForward, error)
	RequestBroker(ctx context.Context, method, baseURL, token, path string) ([]byte, error)
}

type execRunner struct{}

var brokerHTTPClient = &http.Client{Timeout: 10 * time.Second}

func (execRunner) RequestBroker(ctx context.Context, method, baseURL, token, path string) (body []byte, retErr error) {
	request, err := http.NewRequestWithContext(ctx, method, baseURL+path, nil) //nolint:gosec // baseURL is the local SSH forward and path is an internal broker endpoint.
	if err != nil {
		return nil, fmt.Errorf("create broker request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := brokerHTTPClient.Do(request) //nolint:gosec // Request URL derives from the trusted local SSH forward and fixed broker endpoints.
	if err != nil {
		return nil, fmt.Errorf("send broker request: %w", err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close broker response: %w", err))
		}
	}()
	body, err = io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("read broker response: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("broker request %s %s returned %s", method, path, response.Status)
	}
	return body, nil
}

func (execRunner) RunOMP(
	ctx context.Context,
	args, env []string,
	stdin io.Reader,
	stdout, stderr io.Writer,
) error {
	cmd := exec.CommandContext(ctx, commandOMP, args...) //nolint:gosec // Fixed executable; argument slice bypasses shell parsing.
	cmd.Env = env
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", commandOMP, err)
	}
	return nil
}

func (execRunner) ReadToken(ctx context.Context, d deployment) (string, error) {
	args := append([]string{"-T", d.host}, kubectlGetSecretArgs(d)...)
	cmd := exec.CommandContext(ctx, commandSSH, args...) //nolint:gosec // Fixed command and arguments bypass local shell parsing.
	cmd.Env = envWithoutBrokerAuth()
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = io.Discard
	cmd.Stdin = strings.NewReader("")
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("read k3s broker Secret over SSH: %w", err)
	}
	encoded := strings.TrimSpace(output.String())
	token, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("decode k3s broker token from Secret %s: %w", d.secret, err)
	}
	return string(token), nil
}

type brokerForward interface {
	Port() int
	WaitReady(ctx context.Context) error
	Close() error
}

type sshProcess struct {
	cmd       *exec.Cmd
	stdin     *os.File
	done      chan struct{}
	waitErr   error
	closeOnce sync.Once
	closeErr  error
}

type sshForward struct {
	host       string
	sshCommand string
	port       int
	output     *portForwardOutput
	kubectl    *sshProcess
	tunnel     *sshProcess
}

type portForwardOutput struct {
	destination io.Writer
	targetPort  int
	ready       chan int

	mu      sync.Mutex
	pending []byte
	output  []byte
}

const maxPortForwardOutput = 8 << 10

func newPortForwardOutput(destination io.Writer, targetPort int) *portForwardOutput {
	return &portForwardOutput{
		destination: destination,
		targetPort:  targetPort,
		ready:       make(chan int, 1),
	}
}

func (w *portForwardOutput) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.destination != nil {
		n, err := w.destination.Write(data)
		if err != nil {
			return n, err
		}
		if n != len(data) {
			return n, io.ErrShortWrite
		}
	}

	w.pending = append(w.pending, data...)
	for {
		lineEnd := bytes.IndexByte(w.pending, '\n')
		if lineEnd < 0 {
			break
		}
		line := w.pending[:lineEnd]
		w.recordLine(line)
		w.signalForwarding(line)
		w.pending = w.pending[lineEnd+1:]
	}
	if len(w.pending) > maxPortForwardOutput {
		w.pending = append(w.pending[:0], w.pending[len(w.pending)-maxPortForwardOutput:]...)
	}
	return len(data), nil
}

func (w *portForwardOutput) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	output := make([]byte, 0, len(w.output)+len(w.pending))
	output = append(output, w.output...)
	output = append(output, w.pending...)
	return strings.TrimSpace(string(output))
}

func (w *portForwardOutput) recordLine(line []byte) {
	w.output = append(w.output, line...)
	w.output = append(w.output, '\n')
	if len(w.output) > maxPortForwardOutput {
		w.output = append([]byte(nil), w.output[len(w.output)-maxPortForwardOutput:]...)
	}
}

func (w *portForwardOutput) signalForwarding(line []byte) {
	const prefix = "Forwarding from 127.0.0.1:"
	if !bytes.HasPrefix(line, []byte(prefix)) {
		return
	}
	line = line[len(prefix):]
	const separator = " -> "
	separatorIndex := bytes.Index(line, []byte(separator))
	if separatorIndex < 0 {
		return
	}
	localPort, ok := parseForwardPort(line[:separatorIndex])
	if !ok {
		return
	}
	targetPort, ok := parseForwardPort(bytes.TrimSpace(line[separatorIndex+len(separator):]))
	if !ok || targetPort != w.targetPort {
		return
	}
	select {
	case w.ready <- localPort:
	default:
	}
}

func parseForwardPort(value []byte) (int, bool) {
	if len(value) == 0 {
		return 0, false
	}
	port := 0
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return 0, false
		}
		value := int(digit - '0')
		if port > (65535-value)/10 {
			return 0, false
		}
		port = port*10 + value
	}
	return port, port > 0
}

func (execRunner) StartForward(ctx context.Context, d deployment, remotePort int) (brokerForward, error) {
	return startForward(ctx, d, remotePort, commandSSH)
}

// Kubectl chooses a host port for :<targetPort> at runtime, so the SSH
// tunnel cannot start until WaitReady parses that port from its forwarding line.
func startForward(ctx context.Context, d deployment, remotePort int, sshCommand string) (brokerForward, error) {
	localPort, err := freeLocalPort()
	if err != nil {
		return nil, err
	}
	output := newPortForwardOutput(os.Stderr, remotePort)
	remoteCommand := shellRemoteCommand(kubectlPortForwardArgs(d, remotePort))
	kubectl, err := startSSHProcess(ctx, sshCommand, []string{"-T", d.host, remoteCommand}, output, output)
	if err != nil {
		return nil, err
	}
	return &sshForward{
		host:       d.host,
		sshCommand: sshCommand,
		port:       localPort,
		output:     output,
		kubectl:    kubectl,
	}, nil
}

func kubectlGetSecretArgs(d deployment) []string {
	return []string{
		"kubectl",
		"--kubeconfig", d.kubeconfig,
		"-n", d.namespace,
		"get", "secret", d.secret,
		"-o", "jsonpath={.data.masterBearer}",
	}
}

func kubectlPortForwardArgs(d deployment, remotePort int) []string {
	return []string{
		"kubectl",
		"--kubeconfig", d.kubeconfig,
		"-n", d.namespace,
		"port-forward",
		"--address=127.0.0.1",
		"pod/" + d.pod,
		fmt.Sprintf(":%d", remotePort),
	}
}

func shellRemoteCommand(args []string) string {
	quoted := make([]string, 0, len(args)+4)
	quoted = append(quoted, "sh", "-c", shellQuote(remoteChildScript), "omp-pool")
	for _, arg := range args {
		quoted = append(quoted, shellQuote(arg))
	}
	return strings.Join(quoted, " ")
}

// The watcher ties remote kubectl to SSH stdin EOF, independent of SSH signals.
const remoteChildScript = `child=
watcher=
cleanup() {
	trap - 0 HUP INT TERM
	if [ -n "$watcher" ]; then
		kill -TERM "$watcher" 2>/dev/null || true
		wait "$watcher" 2>/dev/null || true
	fi
	if [ -n "$child" ]; then
		kill -TERM "$child" 2>/dev/null || true
		wait "$child" 2>/dev/null || true
	fi
}
trap cleanup 0
trap 'exit 1' HUP INT TERM
"$@" </dev/null &
child=$!
exec 3<&0
(
	while IFS= read -r line <&3; do :; done
	# A pre-exec shell can consume TERM, so retry until it exits or the grace period ends.
	timer=
	trap 'if [ -n "$timer" ]; then kill "$timer" 2>/dev/null; wait "$timer" 2>/dev/null; fi; exit' TERM
	attempt=0
	delay=0.1
	step=1
	while [ "$attempt" -lt 20 ] && kill -0 "$child" 2>/dev/null; do
		kill -TERM "$child" 2>/dev/null || break
		sleep "$delay" &
		timer=$!
		if ! wait "$timer" 2>/dev/null; then
			delay=1
			step=10
			timer=
			continue
		fi
		timer=
		attempt=$((attempt + step))
	done
	if [ "$attempt" -ge 20 ] && kill -0 "$child" 2>/dev/null; then
		kill -KILL "$child" 2>/dev/null || true
	fi
) &
watcher=$!
exec 3<&-
wait "$child"
status=$?
child=
kill -TERM "$watcher" 2>/dev/null || true
wait "$watcher" 2>/dev/null || true
watcher=
exit "$status"
`

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func startSSHProcess(ctx context.Context, sshCommand string, args []string, stdout, stderr io.Writer) (*sshProcess, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("start SSH process: %w", err)
	}
	stdinReader, stdinWriter, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create SSH stdin pipe: %w", err)
	}
	cmd := exec.Command(sshCommand, args...) //nolint:gosec // Fixed executable and constructed argv avoid a local shell.
	cmd.Env = envWithoutBrokerAuth()
	cmd.Stdin = stdinReader
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		_ = stdinReader.Close()
		_ = stdinWriter.Close()
		return nil, fmt.Errorf("start SSH process: %w", err)
	}
	process := &sshProcess{cmd: cmd, stdin: stdinWriter, done: make(chan struct{})}
	go func() {
		process.waitErr = cmd.Wait()
		close(process.done)
	}()
	if err := stdinReader.Close(); err != nil {
		closeErr := process.Close()
		return nil, errors.Join(fmt.Errorf("close local SSH stdin reader: %w", err), closeErr)
	}
	go func() {
		select {
		case <-ctx.Done():
			_ = process.Close()
		case <-process.done:
		}
	}()
	return process, nil
}

func (f *sshForward) Port() int { return f.port }

func (f *sshForward) WaitReady(ctx context.Context) error {
	var remotePort int
	select {
	case remotePort = <-f.output.ready:
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("wait for kubectl port-forward readiness: %w", err)
		}
		if processExited(f.kubectl.done) {
			return f.kubectlExitError("kubectl port-forward exited before tunnel startup")
		}
	case <-f.kubectl.done:
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("wait for kubectl port-forward readiness: %w", err)
		}
		return f.kubectlExitError("kubectl port-forward exited before reporting readiness")
	case <-ctx.Done():
		return fmt.Errorf("wait for kubectl port-forward readiness: %w", ctx.Err())
	}
	if err := f.startTunnel(ctx, remotePort); err != nil {
		return err
	}
	return f.waitForTunnelReady(ctx)
}

func (f *sshForward) Close() error {
	var closeErr error
	if err := f.tunnel.Close(); err != nil {
		closeErr = errors.Join(closeErr, fmt.Errorf("close SSH tunnel: %w", err))
	}
	if err := f.kubectl.Close(); err != nil {
		closeErr = errors.Join(closeErr, fmt.Errorf("close remote kubectl port-forward: %w", err))
	}
	return closeErr
}

func (f *sshForward) startTunnel(ctx context.Context, remotePort int) error {
	args := []string{
		"-N",
		"-o", "ExitOnForwardFailure=yes",
		"-L", fmt.Sprintf("%d:127.0.0.1:%d", f.port, remotePort),
		f.host,
	}
	tunnel, err := startSSHProcess(ctx, f.sshCommand, args, io.Discard, os.Stderr)
	if err != nil {
		return fmt.Errorf("start SSH tunnel to k3s broker: %w", err)
	}
	f.tunnel = tunnel
	return nil
}

func (f *sshForward) waitForTunnelReady(ctx context.Context) error {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		address := net.JoinHostPort("127.0.0.1", strconv.Itoa(f.port))
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
		if err == nil {
			if err := conn.Close(); err != nil {
				return fmt.Errorf("close SSH tunnel readiness connection: %w", err)
			}
			return nil
		}
		select {
		case <-f.tunnel.done:
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("wait for SSH tunnel readiness: %w", err)
			}
			return processExitError("SSH tunnel exited before accepting connections", f.tunnel)
		case <-f.kubectl.done:
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("wait for SSH tunnel readiness: %w", err)
			}
			return f.kubectlExitError("kubectl port-forward exited before tunnel readiness")
		case <-ticker.C:
		case <-ctx.Done():
			return fmt.Errorf("wait for SSH tunnel readiness: %w", ctx.Err())
		}
	}
}

func (f *sshForward) kubectlExitError(message string) error {
	output := f.output.String()
	if f.kubectl.waitErr == nil {
		if output != "" {
			return fmt.Errorf("%s: %s", message, output)
		}
		return errors.New(message)
	}
	if output != "" {
		return fmt.Errorf("%s: %s: %w", message, output, f.kubectl.waitErr)
	}
	return fmt.Errorf("%s: %w", message, f.kubectl.waitErr)
}

func processExited(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

func waitForProcessExit(done <-chan struct{}, timeout time.Duration) bool {
	if processExited(done) {
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func processExitError(message string, process *sshProcess) error {
	if process.waitErr == nil {
		return errors.New(message)
	}
	return fmt.Errorf("%s: %w", message, process.waitErr)
}

const (
	sshStdinCloseTimeout = 2 * time.Second
	sshInterruptTimeout  = time.Second
)

func (p *sshProcess) Close() error {
	if p == nil {
		return nil
	}
	p.closeOnce.Do(p.closeProcess)
	return p.closeErr
}

func (p *sshProcess) closeProcess() {
	exited := processExited(p.done)
	if p.stdin != nil {
		if err := p.stdin.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			p.closeErr = errors.Join(p.closeErr, fmt.Errorf("close SSH stdin: %w", err))
		}
	}
	if exited {
		if p.waitErr != nil {
			p.closeErr = errors.Join(p.closeErr, fmt.Errorf("SSH process exited: %w", p.waitErr))
		}
		return
	}
	p.stop()
}

func (p *sshProcess) stop() {
	if waitForProcessExit(p.done, sshStdinCloseTimeout) {
		return
	}
	if err := p.cmd.Process.Signal(os.Interrupt); err != nil && !errors.Is(err, os.ErrProcessDone) {
		p.closeErr = errors.Join(p.closeErr, fmt.Errorf("interrupt SSH process: %w", err))
	}
	if waitForProcessExit(p.done, sshInterruptTimeout) {
		return
	}
	if err := p.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		p.closeErr = errors.Join(p.closeErr, fmt.Errorf("kill SSH process: %w", err))
	}
	<-p.done
}

type redactingWriter struct {
	destination io.Writer
	secret      []byte
	pending     []byte
}

func newRedactingWriter(destination io.Writer, secret string) *redactingWriter {
	return &redactingWriter{destination: destination, secret: []byte(secret)}
}

func (w *redactingWriter) Write(data []byte) (int, error) {
	inputLength := len(data)
	w.pending = append(w.pending, data...)
	combined := w.pending
	if len(w.secret) != 0 {
		combined = bytes.ReplaceAll(combined, w.secret, []byte("[redacted]"))
	}
	safeLength := len(combined) - longestSecretPrefixSuffix(combined, w.secret)
	if err := w.writeSafe(combined[:safeLength]); err != nil {
		return 0, err
	}
	w.pending = append(w.pending[:0], combined[safeLength:]...)
	return inputLength, nil
}

func longestSecretPrefixSuffix(value, secret []byte) int {
	limit := min(len(value), len(secret))
	for n := limit; n > 0; n-- {
		if bytes.Equal(value[len(value)-n:], secret[:n]) {
			return n
		}
	}
	return 0
}

func (w *redactingWriter) Flush() error {
	if err := w.writeSafe(w.pending); err != nil {
		return err
	}
	w.pending = w.pending[:0]
	return nil
}

func (w *redactingWriter) writeSafe(data []byte) error {
	if len(w.secret) != 0 {
		data = bytes.ReplaceAll(data, w.secret, []byte("[redacted]"))
	}
	n, err := w.destination.Write(data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	return nil
}

func runSensitive(
	ctx context.Context,
	runner commandRunner,
	args, env []string,
	stdin io.Reader,
	stdout, stderr io.Writer,
	secret string,
) error {
	redactedStdout := newRedactingWriter(stdout, secret)
	redactedStderr := newRedactingWriter(stderr, secret)
	runErr := runner.RunOMP(ctx, args, env, stdin, redactedStdout, redactedStderr)
	outputErr := errors.Join(redactedStdout.Flush(), redactedStderr.Flush())
	if runErr != nil || outputErr != nil {
		return errors.Join(runErr, outputErr)
	}
	return nil
}
