package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

type runTestCase struct {
	name           string
	knownHosts     bool
	tailnetUp      bool
	lanUp          bool
	initialCounter string
	wantCode       int
	wantPassword   bool
	wantCounter    string
}

func TestRun(t *testing.T) {
	tests := []runTestCase{
		{name: "tailnet up skips LAN and clears failures", tailnetUp: true, lanUp: true, initialCounter: "2\n", wantCode: 0},
		{name: "LAN down is not an auth attempt", wantCode: 0},
		{name: "empty known hosts captures key without auth", lanUp: true, wantCode: 0},
		{name: "authenticated then close", knownHosts: true, lanUp: true, initialCounter: "2\n", wantCode: 0, wantPassword: true},
		{name: "reject password", knownHosts: true, lanUp: true, initialCounter: "2\n", wantCode: 1, wantPassword: true, wantCounter: "3\n"},
		{name: "close during auth", knownHosts: true, lanUp: true, initialCounter: "2\n", wantCode: 0, wantPassword: true, wantCounter: "2\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { testRunCase(t, tt) })
	}
}

func testRunCase(t *testing.T, tt runTestCase) {
	t.Helper()
	stateDir := t.TempDir()
	passwordPath := filepath.Join(stateDir, "password")
	if err := os.WriteFile(passwordPath, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	knownHostsPath := ""
	if tt.knownHosts {
		knownHostsPath = filepath.Join(stateDir, "known_hosts")
		writeKnownHost(t, knownHostsPath, "192.0.2.10", testSigner(t).PublicKey())
	}
	if tt.initialCounter != "" {
		if err := os.WriteFile(filepath.Join(stateDir, failureCountName), []byte(tt.initialCounter), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dialer := newRunTestDialer(t, tt)
	var logs strings.Builder
	code := run(context.Background(), lookupEnvironment(validEnvironment(stateDir, passwordPath, knownHostsPath)), dialer, slog.New(slog.NewTextHandler(&logs, nil)))
	checkRunResult(t, tt, code, logs.String(), dialer, stateDir)
}

func newRunTestDialer(t *testing.T, tt runTestCase) *sshTestDialer {
	t.Helper()
	behavior := serverAcceptAndClose
	switch tt.name {
	case "reject password":
		behavior = serverRejectPassword
	case "close during auth":
		behavior = serverCloseDuringAuth
	}
	return &sshTestDialer{
		up: map[string]bool{
			"192.0.2.20:22": tt.tailnetUp,
			"192.0.2.10:22": tt.lanUp,
		},
		calls:        make(map[string]int),
		behavior:     behavior,
		passwordSeen: make(chan string, 1),
		signer:       testSigner(t),
	}
}

func checkRunResult(t *testing.T, tt runTestCase, code int, logs string, dialer *sshTestDialer, stateDir string) {
	t.Helper()
	checkRunStatus(t, tt, code, dialer)
	if tt.wantPassword {
		checkPasswordReceived(t, dialer.passwordSeen)
	}
	if tt.name == "empty known hosts captures key without auth" {
		select {
		case password := <-dialer.passwordSeen:
			t.Errorf("capture mode sent password %q", password)
		default:
		}
	}
	checkRunLogs(t, tt.name, logs, dialer.signer.PublicKey())
	if got := failureCountContents(t, stateDir); got != tt.wantCounter {
		t.Errorf("failure counter = %q, want %q", got, tt.wantCounter)
	}
}

func checkRunStatus(t *testing.T, tt runTestCase, code int, dialer *sshTestDialer) {
	t.Helper()
	if code != tt.wantCode {
		t.Fatalf("run() = %d, want %d", code, tt.wantCode)
	}
	if tt.tailnetUp && dialer.calls["192.0.2.10:22"] != 0 {
		t.Errorf("LAN dials = %d, want 0 while tailnet is reachable", dialer.calls["192.0.2.10:22"])
	}
	wantProbe := !tt.tailnetUp && !strings.HasPrefix(tt.initialCounter, "3")
	if got := dialer.calls["192.0.2.10:22"] > 0; got != wantProbe {
		t.Errorf("LAN probe attempted = %t, want %t", got, wantProbe)
	}
}

func checkPasswordReceived(t *testing.T, passwordSeen <-chan string) {
	t.Helper()
	select {
	case password := <-passwordSeen:
		if password != "secret" {
			t.Errorf("server received password %q, want secret", password)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SSH server did not receive the password")
	}
}

func checkRunLogs(t *testing.T, name, logs string, hostKey ssh.PublicKey) {
	t.Helper()
	switch name {
	case "empty known hosts captures key without auth":
		if strings.Contains(logs, "unlock sent") {
			t.Errorf("capture mode logged unlock sent: %s", logs)
		}
		checkCapturedKnownHost(t, logs, "192.0.2.10:22", hostKey)
	case "close during auth":
		if !strings.Contains(logs, "unlock indeterminate") {
			t.Errorf("indeterminate disconnect log missing: %s", logs)
		}
	case "authenticated then close":
		if !strings.Contains(logs, "unlock sent") {
			t.Errorf("success log missing: %s", logs)
		}
	case "reject password":
		if !strings.Contains(logs, "SSH authentication rejected") {
			t.Errorf("rejection log missing: %s", logs)
		}
	}
}

func checkCapturedKnownHost(t *testing.T, logs, address string, hostKey ssh.PublicKey) {
	t.Helper()
	entry, ok := loggedKnownHostEntry(logs)
	if !ok {
		t.Fatalf("capture log has no known_hosts entry: %s", logs)
	}
	if !strings.Contains(logs, "fingerprint="+ssh.FingerprintSHA256(hostKey)) {
		t.Errorf("capture log fingerprint does not match host key: %s", logs)
	}
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(path, []byte(entry+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	callback, err := knownhosts.New(path)
	if err != nil {
		t.Fatalf("parse logged known_hosts entry: %v", err)
	}
	remote := &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 22}
	if err := callback(address, remote, hostKey); err != nil {
		t.Errorf("logged known_hosts entry does not match host key: %v", err)
	}
}

func loggedKnownHostEntry(logs string) (string, bool) {
	const marker = " known_hosts="
	_, entryAndFingerprint, ok := strings.Cut(logs, marker)
	if !ok {
		return "", false
	}
	quotedEntry, _, ok := strings.Cut(entryAndFingerprint, " fingerprint=")
	if !ok {
		return "", false
	}
	entry, err := strconv.Unquote(quotedEntry)
	return entry, err == nil
}

func TestRunHostKeyMismatchDoesNotReadPassword(t *testing.T) {
	stateDir := t.TempDir()
	passwordPath := filepath.Join(stateDir, "password")
	knownHostsPath := filepath.Join(stateDir, "known_hosts")
	writeKnownHost(t, knownHostsPath, "192.0.2.10", testSigner(t).PublicKey())
	dialer := &sshTestDialer{
		up:           map[string]bool{"192.0.2.10:22": true},
		calls:        make(map[string]int),
		behavior:     serverAcceptAndClose,
		passwordSeen: make(chan string, 1),
		signer:       otherTestSigner(t),
	}
	var logs strings.Builder
	code := run(context.Background(), lookupEnvironment(validEnvironment(stateDir, passwordPath, knownHostsPath)), dialer, slog.New(slog.NewTextHandler(&logs, nil)))
	if code != 1 {
		t.Fatalf("run() = %d, want 1; logs: %s", code, logs.String())
	}
	if !strings.Contains(logs.String(), "refusing to send the password") {
		t.Errorf("mismatch log missing refusal: %s", logs.String())
	}
	select {
	case password := <-dialer.passwordSeen:
		t.Fatalf("password was sent after host-key mismatch: %q", password)
	default:
	}
}

func TestRunFailureCounterBlocksAtThree(t *testing.T) {
	stateDir := t.TempDir()
	passwordPath := filepath.Join(stateDir, "password")
	knownHostsPath := filepath.Join(stateDir, "known_hosts")
	writeKnownHost(t, knownHostsPath, "192.0.2.10", testSigner(t).PublicKey())
	if err := os.WriteFile(filepath.Join(stateDir, failureCountName), []byte("3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dialer := &sshTestDialer{
		up:           map[string]bool{"192.0.2.10:22": true},
		calls:        make(map[string]int),
		behavior:     serverAcceptAndClose,
		passwordSeen: make(chan string, 1),
		signer:       testSigner(t),
	}
	code := run(context.Background(), lookupEnvironment(validEnvironment(stateDir, passwordPath, knownHostsPath)), dialer, slog.New(slog.DiscardHandler))
	if code != 1 {
		t.Fatalf("run() = %d, want 1", code)
	}
	if dialer.calls["192.0.2.10:22"] != 0 {
		t.Fatalf("lockout performed LAN/SSH attempts: dials=%v", dialer.calls)
	}
}

func TestRunMissingEnvironmentFailsFast(t *testing.T) {
	code := run(context.Background(), func(string) (string, bool) { return "", false }, &sshTestDialer{}, slog.New(slog.DiscardHandler))
	if code != 1 {
		t.Fatalf("run() = %d, want 1", code)
	}
}

func TestWriteFailureCountCreatesPrivateState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", failureCountName)
	if err := writeFailureCount(path, 1); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("counter mode = %04o, want 0600", info.Mode().Perm())
	}
}

const (
	serverAcceptAndClose = iota
	serverRejectPassword
	serverCloseDuringAuth
)

type sshTestDialer struct {
	up           map[string]bool
	calls        map[string]int
	behavior     int
	passwordSeen chan string
	signer       ssh.Signer
}

func (d *sshTestDialer) DialContext(_ context.Context, _, address string) (net.Conn, error) {
	d.calls[address]++
	if !d.up[address] {
		return nil, errors.New("unreachable")
	}
	clientConn, serverConn := net.Pipe()
	if address == "192.0.2.10:22" {
		go d.serve(serverConn)
	} else {
		_ = serverConn.Close()
	}
	clientConn = newAsyncPipeConn(clientConn)
	return &testAddressConn{Conn: clientConn, remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 22}}, nil
}

func (d *sshTestDialer) serve(conn net.Conn) {
	defer conn.Close()
	serverConfig := &ssh.ServerConfig{PasswordCallback: func(_ ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
		d.passwordSeen <- string(password)
		if d.behavior == serverCloseDuringAuth {
			_ = conn.Close()
			return nil, errors.New("close during authentication")
		}
		if d.behavior == serverRejectPassword {
			return nil, errors.New("password rejected")
		}
		return &ssh.Permissions{}, nil
	}}
	serverConfig.AddHostKey(d.signer)
	serverConn, chans, reqs, err := ssh.NewServerConn(conn, serverConfig)
	if err != nil {
		return
	}
	defer serverConn.Close()
	go ssh.DiscardRequests(reqs)
	for channel := range chans {
		_ = channel.Reject(ssh.Prohibited, "unused")
	}
}

type asyncPipeConn struct {
	net.Conn
	writes chan []byte
	done   chan struct{}
	once   sync.Once
}

func newAsyncPipeConn(conn net.Conn) *asyncPipeConn {
	async := &asyncPipeConn{Conn: conn, writes: make(chan []byte, 64), done: make(chan struct{})}
	go func() {
		for {
			select {
			case <-async.done:
				return
			case packet := <-async.writes:
				if _, err := async.Conn.Write(packet); err != nil {
					return
				}
			}
		}
	}()
	return async
}

func (c *asyncPipeConn) Write(packet []byte) (int, error) {
	copyOfPacket := append([]byte(nil), packet...)
	select {
	case <-c.done:
		return 0, net.ErrClosed
	case c.writes <- copyOfPacket:
		return len(packet), nil
	}
}

func (c *asyncPipeConn) Close() error {
	var err error
	c.once.Do(func() {
		close(c.done)
		err = c.Conn.Close()
	})
	return err
}

type testAddressConn struct {
	net.Conn
	remote net.Addr
}

func (c *testAddressConn) RemoteAddr() net.Addr { return c.remote }

func validEnvironment(stateDir, passwordPath, knownHostsPath string) map[string]string {
	return map[string]string{
		"FVUNLOCK_LAN_ADDR":      "192.0.2.10:22",
		"FVUNLOCK_TAILNET_ADDR":  "192.0.2.20:22",
		"FVUNLOCK_USER":          "fvunlock",
		"FVUNLOCK_PASSWORD_FILE": passwordPath,
		"FVUNLOCK_KNOWN_HOSTS":   knownHostsPath,
		"FVUNLOCK_STATE_DIR":     stateDir,
	}
}

func lookupEnvironment(values map[string]string) environment {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

type generatedTestSigners struct {
	host  ssh.Signer
	other ssh.Signer
	err   error
}

var testSigners = sync.OnceValue(func() generatedTestSigners {
	host, err := generateTestSigner()
	if err != nil {
		return generatedTestSigners{err: err}
	}
	other, err := generateTestSigner()
	if err != nil {
		return generatedTestSigners{err: err}
	}
	return generatedTestSigners{host: host, other: other}
})

func generateTestSigner() (ssh.Signer, error) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return ssh.NewSignerFromKey(privateKey)
}

func testSigner(t *testing.T) ssh.Signer {
	t.Helper()
	signers := testSigners()
	if signers.err != nil {
		t.Fatalf("generate SSH test keys: %v", signers.err)
	}
	return signers.host
}

func otherTestSigner(t *testing.T) ssh.Signer {
	t.Helper()
	signers := testSigners()
	if signers.err != nil {
		t.Fatalf("generate SSH test keys: %v", signers.err)
	}
	return signers.other
}

func writeKnownHost(t *testing.T, path, address string, key ssh.PublicKey) {
	t.Helper()
	if err := os.WriteFile(path, []byte(knownhosts.Line([]string{address}, key)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func failureCountContents(t *testing.T, stateDir string) string {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(stateDir, failureCountName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ""
		}
		t.Fatal(err)
	}
	return string(contents)
}
