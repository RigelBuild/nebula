package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestRequestBroker(t *testing.T) {
	t.Run("sends authenticated request and reads response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodDelete || r.URL.Path != "/v1/credential/3/blocks" {
				t.Errorf("request = %s %s, want DELETE /v1/credential/3/blocks", r.Method, r.URL.Path)
			}
			if r.Header.Get("Authorization") != "Bearer token-value" {
				t.Errorf("authorization = %q, want bearer token", r.Header.Get("Authorization"))
			}
			if _, err := io.WriteString(w, "deleted"); err != nil {
				t.Errorf("write HTTP response: %v", err)
			}
		}))
		defer server.Close()

		body, err := (execRunner{}).RequestBroker(context.Background(), http.MethodDelete, server.URL, "token-value", "/v1/credential/3/blocks")
		if err != nil || string(body) != "deleted" {
			t.Fatalf("RequestBroker() = (%q, %v), want deleted response", body, err)
		}
	})

	t.Run("reports unsuccessful response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}))
		defer server.Close()

		_, err := (execRunner{}).RequestBroker(context.Background(), http.MethodPost, server.URL, "token-value", "/v1/usage/stale")
		if err == nil || !strings.Contains(err.Error(), "503 Service Unavailable") {
			t.Fatalf("RequestBroker() error = %v, want status error", err)
		}
	})
}

func TestRequestBrokerDoesNotFollowRedirects(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{name: "301 moved permanently", status: http.StatusMovedPermanently},
		{name: "302 found", status: http.StatusFound},
		{name: "307 temporary redirect", status: http.StatusTemporaryRedirect},
		{name: "308 permanent redirect", status: http.StatusPermanentRedirect},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var targetRequests, targetAuthorized atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				targetRequests.Add(1)
				if r.Header.Get("Authorization") != "" {
					targetAuthorized.Add(1)
				}
			}))
			defer target.Close()
			redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target.URL+r.URL.Path, tt.status)
			}))
			defer redirector.Close()

			want := fmt.Sprintf("%d %s", tt.status, http.StatusText(tt.status))
			_, err := (execRunner{}).RequestBroker(t.Context(), http.MethodPost, redirector.URL, "token-value", "/v1/usage/stale")
			if n := targetRequests.Load(); n != 0 {
				t.Errorf("redirect target received %d requests, %d with Authorization; want 0", n, targetAuthorized.Load())
			}
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("RequestBroker() error = %v, want %s status error", err, want)
			}
			if strings.Contains(err.Error(), "token-value") {
				t.Errorf("RequestBroker() error %q contains bearer token", err)
			}
		})
	}
}

func TestReadTokenUsesK3sSecret(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "ssh-args")
	token := "pool-token"
	encoded := base64.StdEncoding.EncodeToString([]byte(token))
	script := fmt.Sprintf(
		"#!/bin/sh\nprintf '%%s\\n' \"$@\" > %s\nprintf '%%s' %s\n",
		shellQuote(argsPath),
		shellQuote(encoded),
	)
	if err := os.WriteFile(filepath.Join(dir, commandSSH), []byte(script), 0o700); err != nil {
		t.Fatalf("write SSH helper: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	d := testDeployment()
	got, err := (execRunner{}).ReadToken(context.Background(), d)
	if err != nil || got != token {
		t.Fatalf("ReadToken() = (%q, %v), want decoded token", got, err)
	}
	actual, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read SSH arguments: %v", err)
	}
	want := []string{"-T", d.host, shellJoin(kubectlGetSecretArgs(d))}
	if got := strings.Split(strings.TrimSpace(string(actual)), "\n"); !slices.Equal(got, want) {
		t.Fatalf("SSH arguments = %v, want %v", got, want)
	}
}

func TestReadTokenQuotesKubeconfigWithSpaces(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "ssh-args")
	encoded := base64.StdEncoding.EncodeToString([]byte("pool-token"))
	script := fmt.Sprintf(
		"#!/bin/sh\nprintf '%%s\\n' \"$@\" > %s\nprintf '%%s' %s\n",
		shellQuote(argsPath),
		shellQuote(encoded),
	)
	if err := os.WriteFile(filepath.Join(dir, commandSSH), []byte(script), 0o700); err != nil {
		t.Fatalf("write SSH helper: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	d := testDeployment()
	d.kubeconfig = "/etc/example dir/broker.kubeconfig"
	if _, err := (execRunner{}).ReadToken(t.Context(), d); err != nil {
		t.Fatalf("ReadToken() error = %v", err)
	}
	actual, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read SSH arguments: %v", err)
	}
	sshArgs := strings.Split(strings.TrimSuffix(string(actual), "\n"), "\n")
	if len(sshArgs) != 3 || sshArgs[0] != "-T" || sshArgs[1] != d.host {
		t.Fatalf("SSH arguments = %q, want -T, host, and one remote command", sshArgs)
	}
	// The remote shell must split the single command argument back into the exact kubectl argv.
	out, err := exec.CommandContext(t.Context(), "sh", "-c", "printf '%s\\n' "+sshArgs[2]).Output()
	if err != nil {
		t.Fatalf("split remote command: %v", err)
	}
	remoteArgs := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if want := kubectlGetSecretArgs(d); !slices.Equal(remoteArgs, want) {
		t.Fatalf("remote kubectl arguments = %q, want %q", remoteArgs, want)
	}
}

func TestK3sBrokerCommandsTargetPrimaryPool(t *testing.T) {
	d := testDeployment()
	forwardArgs := kubectlPortForwardArgs(d, primaryPort)
	if !slices.Contains(forwardArgs, "pod/"+testPod) || !slices.Contains(forwardArgs, ":4002") {
		t.Fatalf("port-forward arguments = %v, want primary pod and port", forwardArgs)
	}
	secretArgs := kubectlGetSecretArgs(d)
	if !slices.Contains(secretArgs, "get") || !slices.Contains(secretArgs, "secret") ||
		!slices.Contains(secretArgs, testSecret) || !slices.Contains(secretArgs, "jsonpath={.data.masterBearer}") {
		t.Fatalf("Secret arguments = %v, want primary masterBearer Secret", secretArgs)
	}
}

func TestSSHForwardHelper(t *testing.T) {
	if os.Getenv("OMP_POOL_SSH_HELPER") != "1" {
		return
	}
	serveSSHHelper(t)
}

func TestRemoteChildScriptStopsChildOnStdinEOF(t *testing.T) {
	process := startRemoteChildTestProcess(t, false)
	process.childPID = waitForRemoteChildPID(t, process)
	assertProcessRunning(t, process.childPID)
	started := time.Now()
	if err := process.stdin.Close(); err != nil {
		t.Fatalf("close wrapper SSH stdin: %v", err)
	}
	process.waitForShutdown(t, 143, 2*time.Second)
	if elapsed := time.Since(started); elapsed >= 2*time.Second {
		t.Fatalf("remote wrapper shutdown took %s, want less than 2s", elapsed)
	}
	assertProcessGone(t, process.childPID)
	process.childStopped = true
}

func TestRemoteChildScriptKillsStoppedChildOnStdinEOF(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatalf("find sleep: %v", err)
	}
	process := startRemoteChildTestProcess(t, true)
	process.childPID = waitForRemoteChildPID(t, process)
	waitForRemoteChildExec(t, process.childPID, sleep)
	child, err := os.FindProcess(process.childPID)
	if err != nil {
		t.Fatalf("find remote child: %v", err)
	}
	if err := child.Signal(syscall.SIGSTOP); err != nil {
		t.Fatalf("stop remote child: %v", err)
	}
	if err := process.stdin.Close(); err != nil {
		t.Fatalf("close wrapper SSH stdin: %v", err)
	}
	process.waitForShutdown(t, 137, 5*time.Second)
	assertProcessGone(t, process.childPID)
	process.childStopped = true
}

func TestRemoteChildScriptPreservesKillGraceWithWholeSecondSleep(t *testing.T) {
	realSleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatalf("find sleep: %v", err)
	}
	sleepDir := t.TempDir()
	sleepStub := filepath.Join(sleepDir, "sleep")
	script := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in\n *.*) exit 1 ;;\nesac\nexec %s \"$@\"\n", shellQuote(realSleep))
	if err := os.WriteFile(sleepStub, []byte(script), 0o700); err != nil {
		t.Fatalf("write fractional-rejecting sleep: %v", err)
	}
	t.Setenv("PATH", sleepDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	process := startRemoteChildTestProcessWithSleep(t, true, realSleep)
	process.childPID = waitForRemoteChildPID(t, process)
	waitForRemoteChildExec(t, process.childPID, realSleep)
	child, err := os.FindProcess(process.childPID)
	if err != nil {
		t.Fatalf("find remote child: %v", err)
	}
	if err := child.Signal(syscall.SIGSTOP); err != nil {
		t.Fatalf("stop remote child: %v", err)
	}
	started := time.Now()
	if err := process.stdin.Close(); err != nil {
		t.Fatalf("close wrapper SSH stdin: %v", err)
	}
	process.waitForShutdown(t, 137, 5*time.Second)
	if elapsed := time.Since(started); elapsed < 1800*time.Millisecond {
		t.Fatalf("remote wrapper sent KILL after %s, want at least 1.8s", elapsed)
	}
	assertProcessGone(t, process.childPID)
	process.childStopped = true
}

type remoteChildTestProcess struct {
	cmd          *exec.Cmd
	stdin        *os.File
	traceReader  *os.File
	traceWriter  *os.File
	done         chan error
	childPID     int
	childStopped bool
	finished     bool
}

func startRemoteChildTestProcess(t *testing.T, childReady bool) *remoteChildTestProcess {
	t.Helper()
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatalf("find sleep: %v", err)
	}
	return startRemoteChildTestProcessWithSleep(t, childReady, sleep)
}

func startRemoteChildTestProcessWithSleep(t *testing.T, childReady bool, sleep string) *remoteChildTestProcess {
	t.Helper()
	stdinReader, stdinWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("create wrapper stdin pipe: %v", err)
	}
	traceReader, traceWriter, err := os.Pipe()
	if err != nil {
		_ = stdinReader.Close()
		_ = stdinWriter.Close()
		t.Fatalf("create wrapper trace pipe: %v", err)
	}
	script := remoteChildScript
	args := []string{"sh", "-c", script, "omp-pool", sleep, "600"}
	if childReady {
		args = []string{"sh", "-c", script, "omp-pool", "sh", "-c", "trap '' TERM; printf '%s\\n' \"$$\" >&3; exec \"$@\"", "child", sleep, "600"}
	} else {
		args[2] = strings.Replace(script, "child=$!\n", "child=$!\nprintf '%s\\n' \"$child\" >&3\n", 1)
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin = stdinReader
	cmd.ExtraFiles = []*os.File{traceWriter}
	if err := cmd.Start(); err != nil {
		_ = stdinReader.Close()
		_ = stdinWriter.Close()
		_ = traceReader.Close()
		_ = traceWriter.Close()
		t.Fatalf("start remote wrapper: %v", err)
	}
	process := &remoteChildTestProcess{
		cmd:         cmd,
		stdin:       stdinWriter,
		traceReader: traceReader,
		traceWriter: traceWriter,
		done:        make(chan error, 1),
	}
	go func() {
		process.done <- cmd.Wait()
	}()
	t.Cleanup(process.cleanup)
	if err := stdinReader.Close(); err != nil {
		t.Fatalf("close parent wrapper stdin reader: %v", err)
	}
	if err := traceWriter.Close(); err != nil {
		t.Fatalf("close parent wrapper trace writer: %v", err)
	}
	return process
}

func (p *remoteChildTestProcess) cleanup() {
	_ = p.stdin.Close()
	_ = p.traceReader.Close()
	_ = p.traceWriter.Close()
	if p.childPID != 0 && !p.childStopped {
		if child, err := os.FindProcess(p.childPID); err == nil {
			// A stopped child cannot handle SIGTERM; cleanup is best effort.
			_ = child.Kill()
		}
	}
	if p.finished {
		return
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(2 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.done
	}
}

func waitForRemoteChildPID(t *testing.T, process *remoteChildTestProcess) int {
	t.Helper()
	type childResult struct {
		pid int
		err error
	}
	resultChannel := make(chan childResult, 1)
	go func() {
		scanner := bufio.NewScanner(process.traceReader)
		for scanner.Scan() {
			pid, err := strconv.Atoi(scanner.Text())
			if err == nil && pid > 0 {
				resultChannel <- childResult{pid: pid}
				return
			}
		}
		err := scanner.Err()
		if err == nil {
			err = errors.New("wrapper exited without reporting child PID")
		}
		resultChannel <- childResult{err: err}
	}()
	select {
	case result := <-resultChannel:
		if result.err != nil {
			t.Fatalf("read remote child PID from wrapper: %v", result.err)
		}
		return result.pid
	case err := <-process.done:
		process.finished = true
		t.Fatalf("remote wrapper exited before reporting its child PID: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("remote wrapper did not start its long-lived child")
	}
	return 0
}

func waitForRemoteChildExec(t *testing.T, pid int, executable string) {
	t.Helper()
	resolvedExecutable, err := filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatalf("resolve child executable %q: %v", executable, err)
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		childExecutable, err := executableOf(pid)
		if err == nil && childExecutable == resolvedExecutable {
			return
		}
		select {
		case <-ticker.C:
		case <-timer.C:
			t.Fatalf("remote child PID %d did not exec %q", pid, resolvedExecutable)
		}
	}
}

// executableOf returns the executable path of pid. Linux reads /proc/<pid>/exe. Other platforms
// have no /proc, so the path comes from ps.
func executableOf(pid int) (string, error) {
	if runtime.GOOS == "linux" {
		return os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
	}
	out, err := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", fmt.Errorf("ps for pid %d: %w", pid, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (p *remoteChildTestProcess) waitForShutdown(t *testing.T, wantExitCode int, timeout time.Duration) {
	t.Helper()
	select {
	case err := <-p.done:
		p.finished = true
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) || exitError.ExitCode() != wantExitCode {
			t.Fatalf("remote wrapper exit = %v, want exit status %d", err, wantExitCode)
		}
	case <-time.After(timeout):
		t.Fatalf("remote wrapper did not exit after SSH stdin EOF within %s", timeout)
	}
}

func assertProcessRunning(t *testing.T, pid int) {
	t.Helper()
	process, err := os.FindProcess(pid)
	if err != nil {
		t.Fatalf("find remote child PID %d: %v", pid, err)
	}
	if err := process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("remote child PID %d is not running before SSH stdin EOF: %v", pid, err)
	}
}

func assertProcessGone(t *testing.T, pid int) {
	t.Helper()
	process, err := os.FindProcess(pid)
	if err != nil {
		t.Fatalf("find remote child PID %d: %v", pid, err)
	}
	if err := process.Signal(syscall.Signal(0)); err == nil {
		t.Fatalf("remote child PID %d is still running", pid)
	} else if !errors.Is(err, syscall.ESRCH) && !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("probe remote child PID %d: %v", pid, err)
	}
}

func serveSSHHelper(t *testing.T) {
	t.Helper()
	args := sshHelperArgs(os.Args)
	if forwarding := sshForwardingArg(args); forwarding != "" {
		parts := strings.Split(forwarding, ":")
		if len(parts) < 3 {
			t.Fatalf("invalid forward specification %q", forwarding)
		}
		localPort, err := strconv.Atoi(parts[0])
		if err != nil {
			t.Fatalf("parse local port: %v", err)
		}
		remotePort, err := strconv.Atoi(parts[len(parts)-1])
		if err != nil {
			t.Fatalf("parse remote port: %v", err)
		}
		serveSSHListener(t, localPort, remotePort)
		return
	}
	serveKubectlForwardHelper(t, args)
}

func sshHelperArgs(args []string) []string {
	for i, arg := range args {
		if arg == "--" {
			return args[i+1:]
		}
	}
	return args
}

func serveKubectlForwardHelper(t *testing.T, args []string) {
	t.Helper()
	command := strings.Join(args, " ")
	pod, targetPort := brokerTargetFromCommand(command)
	if pod == "" {
		t.Fatalf("remote command does not target a k3s broker pod and port: %s", command)
	}
	writeSSHHelperMarker(t, targetPort)
	if sshHelperShouldExit(targetPort) {
		_, err := fmt.Fprintf(os.Stderr, "Error from server (NotFound): pods %q not found\n", pod)
		if err != nil {
			t.Fatalf("write simulated kubectl error: %v", err)
		}
		t.Fatal("simulated k3s pod does not exist")
	}
	noReadyPort, err := strconv.Atoi(os.Getenv("OMP_POOL_SSH_NO_READY_PORT"))
	if err != nil && os.Getenv("OMP_POOL_SSH_NO_READY_PORT") != "" {
		t.Fatalf("parse no-ready port: %v", err)
	}
	if targetPort == noReadyPort {
		waitForSSHInputEOF()
	}
	remotePort, err := strconv.Atoi(os.Getenv("OMP_POOL_SSH_FORWARDING_PORT"))
	if err != nil {
		t.Fatalf("parse dynamic remote port: %v", err)
	}
	if _, err := fmt.Fprintf(os.Stderr, "Forwarding from 127.0.0.1:%d -> %d\n", remotePort, targetPort); err != nil {
		t.Fatalf("write simulated kubectl readiness: %v", err)
	}
	waitForSSHInputEOF()
}

func brokerTargetFromCommand(command string) (string, int) {
	if strings.Contains(command, shellQuote("pod/"+testPod)) && strings.Contains(command, shellQuote(fmt.Sprintf(":%d", primaryPort))) {
		return testPod, primaryPort
	}
	return "", 0
}

func waitForSSHInputEOF() {
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func sshForwardingArg(args []string) string {
	for i, arg := range args {
		if arg == "-L" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func sshHelperShouldExit(remotePort int) bool {
	exitPort, err := strconv.Atoi(os.Getenv("OMP_POOL_SSH_EXIT_PORT"))
	return err == nil && exitPort != 0 && exitPort == remotePort
}

func serveSSHListener(t *testing.T, localPort, remotePort int) {
	t.Helper()
	noReadyPort, err := strconv.Atoi(os.Getenv("OMP_POOL_SSH_NO_READY_PORT"))
	if err != nil && os.Getenv("OMP_POOL_SSH_NO_READY_PORT") != "" {
		t.Fatalf("parse no-ready port: %v", err)
	}
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(localPort))
	if remotePort == noReadyPort {
		address = "127.0.0.1:0"
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("listen for SSH forward: %v", err)
	}
	defer func() {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close SSH helper listener: %v", err)
		}
	}()
	writeSSHHelperMarker(t, remotePort)
	stopped := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		close(stopped)
		_ = listener.Close()
	}()
	acceptSSHConnections(t, listener, stopped)
}

func writeSSHHelperMarker(t *testing.T, remotePort int) {
	t.Helper()
	markerDir := os.Getenv("OMP_POOL_SSH_MARKER_DIR")
	if markerDir == "" {
		return
	}
	marker := filepath.Join(markerDir, strconv.Itoa(remotePort)+".started")
	if err := os.WriteFile(marker, []byte("started"), 0o600); err != nil {
		t.Fatalf("write SSH helper marker: %v", err)
	}
}

func acceptSSHConnections(t *testing.T, listener net.Listener, stopped <-chan struct{}) {
	t.Helper()
	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-stopped:
				return
			default:
				t.Fatalf("accept SSH readiness connection: %v", err)
			}
		}
		if err := conn.Close(); err != nil {
			t.Fatalf("close SSH readiness connection: %v", err)
		}
	}
}

type lifecycleRunner struct {
	sshCommand string
	forwards   []*sshForward
	failAt     int
}

func (r *lifecycleRunner) RunOMP(_ context.Context, args, env []string, _ io.Reader, stdout, _ io.Writer) error {
	if !containsArg(args, "--dry-run") {
		_, err := io.WriteString(stdout, "upload complete\n")
		return err
	}
	result := emptyPlan()
	if len(r.forwards) > 0 && envValue(env, "OMP_AUTH_BROKER_URL") == brokerURL(r.forwards[0].port) {
		result = plan("openai", "alice")
	}
	_, err := io.WriteString(stdout, result)
	return err
}

func (r *lifecycleRunner) ReadToken(context.Context, deployment) (string, error) {
	return "test-token", nil
}

func (r *lifecycleRunner) RequestBroker(context.Context, string, string, string, string) ([]byte, error) {
	return nil, errors.New("broker requests are not used by push tests")
}

func (r *lifecycleRunner) StartForward(ctx context.Context, d deployment, remotePort int) (brokerForward, error) {
	if r.failAt == len(r.forwards)+1 {
		return nil, errors.New("injected SSH start failure")
	}
	forward, err := startForward(ctx, d, remotePort, r.sshCommand)
	if err != nil {
		return nil, err
	}
	sshForward, ok := forward.(*sshForward)
	if !ok {
		return nil, fmt.Errorf("StartForward() returned %T, want *sshForward", forward)
	}
	r.forwards = append(r.forwards, sshForward)
	return sshForward, nil
}

func TestRunPushClosesAndReapsSSHForwards(t *testing.T) {
	command := makeSSHHelperCommand(t)
	t.Setenv("OMP_POOL_SSH_HELPER", "1")
	t.Setenv("OMP_POOL_SSH_MARKER_DIR", t.TempDir())
	t.Setenv("OMP_POOL_SSH_NO_READY_PORT", "")
	t.Setenv("OMP_POOL_SSH_EXIT_PORT", "")
	t.Setenv("OMP_POOL_SSH_FORWARDING_PORT", "43123")

	runner := &lifecycleRunner{sshCommand: command}
	code, err := runLifecyclePush(context.Background(), stagePushForTest(t), runner)
	if err != nil || code != 0 {
		t.Fatalf("runPush() = (%d, %v), want success", code, err)
	}
	if len(runner.forwards) != 1 {
		t.Fatalf("started %d forwards, want one", len(runner.forwards))
	}
	for _, forward := range runner.forwards {
		assertSSHForwardReaped(t, forward)
	}
}

func TestStartBrokerForwardFailureReturnsError(t *testing.T) {
	runner := &lifecycleRunner{failAt: 1}
	_, err := startBrokerForward(context.Background(), testDeployment(), runner)
	if err == nil || !strings.Contains(err.Error(), "start SSH forward to primary k3s broker: injected SSH start failure") {
		t.Fatalf("startBrokerForward() error = %v, want wrapped start failure", err)
	}
}

func stagePushForTest(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	stageDir := poolStageDir(root, poolPrimary)
	if err := os.MkdirAll(stageDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stageDir, "agent.db"), []byte("staged"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func runLifecyclePush(ctx context.Context, stageRoot string, runner *lifecycleRunner) (int, error) {
	return runPush(ctx, []string{poolPrimary, "--yes"}, testEnv(map[string]string{"OMP_POOL_STAGE_DIR": stageRoot}), strings.NewReader(""), io.Discard, io.Discard, runner)
}

func TestRunPushCancelsAndReapsNeverReadyForward(t *testing.T) {
	command := makeSSHHelperCommand(t)
	t.Setenv("OMP_POOL_SSH_HELPER", "1")
	t.Setenv("OMP_POOL_SSH_MARKER_DIR", t.TempDir())
	t.Setenv("OMP_POOL_SSH_NO_READY_PORT", strconv.Itoa(primaryPort))
	t.Setenv("OMP_POOL_SSH_EXIT_PORT", "")
	t.Setenv("OMP_POOL_SSH_FORWARDING_PORT", "43123")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stageRoot := stagePushForTest(t)
	runner := &lifecycleRunner{sshCommand: command}
	result := make(chan error, 1)
	go func() {
		_, err := runLifecyclePush(ctx, stageRoot, runner)
		result <- err
	}()

	marker := filepath.Join(os.Getenv("OMP_POOL_SSH_MARKER_DIR"), strconv.Itoa(primaryPort)+".started")
	waitForFile(t, marker)
	cancel()
	if err := <-result; err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("runPush() error = %v, want cancellation", err)
	}
	if len(runner.forwards) != 1 {
		t.Fatalf("started %d forwards, want one", len(runner.forwards))
	}
	assertSSHForwardReaped(t, runner.forwards[0])
}

func TestSSHForwardNotFoundFailsReadinessAndReapsKubectl(t *testing.T) {
	command := makeSSHHelperCommand(t)
	t.Setenv("OMP_POOL_SSH_HELPER", "1")
	t.Setenv("OMP_POOL_SSH_MARKER_DIR", t.TempDir())
	t.Setenv("OMP_POOL_SSH_NO_READY_PORT", "")
	t.Setenv("OMP_POOL_SSH_EXIT_PORT", strconv.Itoa(primaryPort))
	t.Setenv("OMP_POOL_SSH_FORWARDING_PORT", "43123")

	forwardValue, err := startForward(context.Background(), testDeployment(), primaryPort, command)
	if err != nil {
		t.Fatalf("startForward() error = %v", err)
	}
	forward := forwardValue.(*sshForward)
	err = forward.WaitReady(context.Background())
	if err == nil || !strings.Contains(err.Error(), "NotFound") || !strings.Contains(err.Error(), testPod) {
		t.Fatalf("WaitReady() error = %v, want missing k3s pod", err)
	}
	if closeErr := forward.Close(); closeErr == nil {
		t.Fatal("Close() error = nil, want the exited kubectl process error")
	}
	assertSSHForwardReaped(t, forward)
}

func TestSSHForwardContextCancellationTerminatesAndReaps(t *testing.T) {
	command := makeSSHHelperCommand(t)
	t.Setenv("OMP_POOL_SSH_HELPER", "1")
	t.Setenv("OMP_POOL_SSH_MARKER_DIR", t.TempDir())
	t.Setenv("OMP_POOL_SSH_NO_READY_PORT", "")
	t.Setenv("OMP_POOL_SSH_EXIT_PORT", "")
	t.Setenv("OMP_POOL_SSH_FORWARDING_PORT", "43123")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	forwardValue, err := startForward(ctx, testDeployment(), primaryPort, command)
	if err != nil {
		t.Fatalf("startForward() error = %v", err)
	}
	forward := forwardValue.(*sshForward)
	if err := forward.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady() error = %v", err)
	}
	cancel()
	assertSSHForwardReaped(t, forward)
}

func makeSSHHelperCommand(t *testing.T) string {
	t.Helper()
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatalf("find current test binary: %v", err)
	}
	command := filepath.Join(t.TempDir(), "ssh-helper")
	script := fmt.Sprintf("#!/bin/sh\nexec %s -test.run=^TestSSHForwardHelper$ -- \"$@\"\n", shellQuote(testBinary))
	if err := os.WriteFile(command, []byte(script), 0o700); err != nil {
		t.Fatalf("write SSH helper command: %v", err)
	}
	return command
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, err := os.Stat(path)
		if err == nil {
			return
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("check helper event file: %v", err)
		}
		select {
		case <-ticker.C:
		case <-timer.C:
			t.Fatalf("timed out waiting for helper event %s", path)
		}
	}
}

func assertSSHForwardReaped(t *testing.T, forward *sshForward) {
	t.Helper()
	for _, process := range []*sshProcess{forward.kubectl, forward.tunnel} {
		if process == nil {
			continue
		}
		select {
		case <-process.done:
		case <-time.After(5 * time.Second):
			t.Fatalf("SSH process %d was not reaped", process.cmd.Process.Pid)
		}
		if process.cmd.ProcessState == nil {
			t.Fatalf("SSH process %d has no exit state after Wait", process.cmd.Process.Pid)
		}
	}
}

func containsArg(args []string, want string) bool {
	return slices.Contains(args, want)
}
