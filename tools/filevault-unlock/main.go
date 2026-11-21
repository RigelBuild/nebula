package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

const (
	dialTimeout      = 3 * time.Second
	maxAuthFailures  = 3
	failureCountName = "auth-failures"
)

type environment func(string) (string, bool)

type dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

type hostKeyPolicy struct {
	callback   ssh.HostKeyCallback
	algorithms []string
}

type config struct {
	lanAddress, tailnetAddress, user, passwordFile, knownHosts, stateDir string
}

type captureModeError struct{}

func (*captureModeError) Error() string { return "host key captured without authentication" }

type hostKeyVerificationError struct{ err error }

func (e *hostKeyVerificationError) Error() string {
	return fmt.Sprintf("host key verification failed: %v", e.err)
}
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.LookupEnv, &net.Dialer{Timeout: dialTimeout}, slog.Default())
	stop()
	os.Exit(code)
}

func run(ctx context.Context, getenv environment, networkDialer dialer, logger *slog.Logger) int {
	cfg, err := loadConfig(getenv)
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		return 1
	}
	counterPath := filepath.Join(cfg.stateDir, failureCountName)
	failures, err := readFailureCount(counterPath)
	if err != nil {
		logger.Error("reading authentication failure counter", "error", err)
		return 1
	}
	if failures >= maxAuthFailures {
		logger.Error("authentication failure limit reached; operator must clear the counter", "failures", failures)
		return 1
	}
	connected, err := probe(ctx, networkDialer, cfg.tailnetAddress, logger)
	if connected {
		if err := resetFailureCount(counterPath); err != nil {
			logger.Error("resetting authentication failure counter", "error", err)
			return 1
		}
		logger.Info("host is reachable on the tailnet; already unlocked")
		return 0
	}
	if ctx.Err() != nil {
		logger.Error("tailnet probe canceled", "error", ctx.Err())
		return 1
	}
	if err != nil {
		logger.Info("host is not reachable on the tailnet", "error", err)
	}
	lanConn, err := dial(ctx, networkDialer, cfg.lanAddress)
	if err != nil {
		if ctx.Err() != nil {
			logger.Error("LAN probe canceled", "error", ctx.Err())
			return 1
		}
		logger.Info("host is down or still booting", "error", err)
		return 0
	}
	hostKeyPolicy, err := newHostKeyCallback(cfg.knownHosts, cfg.lanAddress, logger)
	if err != nil {
		closeConnection(lanConn, logger)
		logger.Error("loading known_hosts", "error", err)
		return 1
	}
	var passwordOffered bool
	var passwordReadErr error
	readPassword := func() (password string, err error) {
		passwordRoot, err := os.OpenRoot(filepath.Dir(cfg.passwordFile))
		if err != nil {
			passwordReadErr = fmt.Errorf("opening password directory: %w", err)
			return "", passwordReadErr
		}
		defer func() {
			if closeErr := passwordRoot.Close(); closeErr != nil {
				passwordReadErr = errors.Join(passwordReadErr, fmt.Errorf("closing password directory: %w", closeErr))
			}
		}()
		contents, err := passwordRoot.ReadFile(filepath.Base(cfg.passwordFile))
		if err != nil {
			passwordReadErr = fmt.Errorf("reading password file: %w", err)
			return "", passwordReadErr
		}
		return strings.TrimSuffix(strings.TrimSuffix(string(contents), "\n"), "\r"), nil
	}
	err = (systemSSHClient{}).Connect(ctx, lanConn, cfg.lanAddress, cfg.user, hostKeyPolicy.callback, hostKeyPolicy.algorithms, readPassword, func() { passwordOffered = true })
	closeConnection(lanConn, logger)
	if passwordReadErr != nil {
		logger.Error("reading password file", "error", passwordReadErr)
		return 1
	}
	if _, ok := errors.AsType[*captureModeError](err); ok {
		return 0
	}
	if hostKeyErr, ok := errors.AsType[*hostKeyVerificationError](err); ok {
		logger.Error("SSH host key mismatch; refusing to send the password", "error", hostKeyErr)
		return 1
	}
	if err == nil {
		logger.Info("unlock sent")
		if resetErr := resetFailureCount(counterPath); resetErr != nil {
			logger.Error("resetting authentication failure counter", "error", resetErr)
			return 1
		}
		return 0
	}
	if passwordOffered && strings.Contains(err.Error(), "unable to authenticate") {
		failures++
		if writeErr := writeFailureCount(counterPath, failures); writeErr != nil {
			logger.Error("recording authentication failure", "error", writeErr)
			return 1
		}
		logger.Error("SSH authentication rejected", "failures", failures, "limit", maxAuthFailures, "error", err)
		return 1
	}
	if passwordOffered {
		logger.Error("unlock indeterminate", "error", err)
		return 0
	}
	logger.Error("SSH connection failed before password authentication", "error", err)
	return 1
}

func loadConfig(getenv environment) (config, error) {
	values := make(map[string]string, 5)
	missing := make([]string, 0, 5)
	for _, name := range []string{"FVUNLOCK_LAN_ADDR", "FVUNLOCK_TAILNET_ADDR", "FVUNLOCK_USER", "FVUNLOCK_PASSWORD_FILE", "FVUNLOCK_STATE_DIR"} {
		value, ok := getenv(name)
		if !ok {
			missing = append(missing, name)
			continue
		}
		values[name] = value
	}
	if len(missing) > 0 {
		return config{}, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}
	for _, name := range []string{"FVUNLOCK_LAN_ADDR", "FVUNLOCK_TAILNET_ADDR", "FVUNLOCK_USER", "FVUNLOCK_PASSWORD_FILE", "FVUNLOCK_STATE_DIR"} {
		if values[name] == "" {
			return config{}, fmt.Errorf("environment variable %s is empty", name)
		}
	}
	knownHosts, _ := getenv("FVUNLOCK_KNOWN_HOSTS")
	return config{values["FVUNLOCK_LAN_ADDR"], values["FVUNLOCK_TAILNET_ADDR"], values["FVUNLOCK_USER"], values["FVUNLOCK_PASSWORD_FILE"], knownHosts, values["FVUNLOCK_STATE_DIR"]}, nil
}

func probe(ctx context.Context, networkDialer dialer, address string, logger *slog.Logger) (bool, error) {
	conn, err := dial(ctx, networkDialer, address)
	if err != nil {
		return false, err
	}
	closeConnection(conn, logger)
	return true, nil
}

func dial(ctx context.Context, networkDialer dialer, address string) (net.Conn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	conn, err := networkDialer.DialContext(dialCtx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("dialing %s: %w", address, err)
	}
	return conn, nil
}

func newHostKeyCallback(path, lanAddress string, logger *slog.Logger) (hostKeyPolicy, error) {
	var contents []byte
	if path != "" {
		data, err := os.ReadFile(path) //nolint:gosec // path is FVUNLOCK_KNOWN_HOSTS, an operator-configured file.
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return hostKeyPolicy{}, fmt.Errorf("reading known_hosts file: %w", err)
		}
		contents = data
	}
	if !hasHostKeyEntries(contents) {
		callback := func(_ string, _ net.Addr, key ssh.PublicKey) error {
			line := knownhosts.Line([]string{lanAddress}, key)
			logger.Warn("preboot host key captured; add this known_hosts line", "known_hosts", line, "fingerprint", ssh.FingerprintSHA256(key))
			return &captureModeError{}
		}
		return hostKeyPolicy{callback: callback}, nil
	}
	callback, err := knownhosts.New(path)
	if err != nil {
		return hostKeyPolicy{}, fmt.Errorf("parsing known_hosts file: %w", err)
	}
	algorithms, err := pinnedHostKeyAlgorithms(contents)
	if err != nil {
		return hostKeyPolicy{}, fmt.Errorf("reading known_hosts algorithms: %w", err)
	}
	return hostKeyPolicy{callback: func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		if err := callback(hostname, remote, key); err != nil {
			return &hostKeyVerificationError{err: err}
		}
		return nil
	}, algorithms: algorithms}, nil
}

func pinnedHostKeyAlgorithms(contents []byte) ([]string, error) {
	algorithms := make([]string, 0)
	for len(contents) > 0 {
		_, _, key, _, rest, err := ssh.ParseKnownHosts(contents)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		for _, algorithm := range hostKeyAlgorithmsForType(key.Type()) {
			if !slices.Contains(algorithms, algorithm) {
				algorithms = append(algorithms, algorithm)
			}
		}
		contents = rest
	}
	return algorithms, nil
}

func hostKeyAlgorithmsForType(keyType string) []string {
	algorithms := []string{keyType}
	var certificate string
	switch keyType {
	case ssh.KeyAlgoRSA:
		return append(algorithms, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSASHA512, ssh.CertAlgoRSASHA256v01, ssh.CertAlgoRSASHA512v01, ssh.CertAlgoRSAv01)
	case ssh.KeyAlgoED25519:
		certificate = ssh.CertAlgoED25519v01
	case ssh.KeyAlgoECDSA256:
		certificate = ssh.CertAlgoECDSA256v01
	case ssh.KeyAlgoECDSA384:
		certificate = ssh.CertAlgoECDSA384v01
	case ssh.KeyAlgoECDSA521:
		certificate = ssh.CertAlgoECDSA521v01
	case ssh.KeyAlgoSKECDSA256:
		certificate = ssh.CertAlgoSKECDSA256v01
	case ssh.KeyAlgoSKED25519:
		certificate = ssh.CertAlgoSKED25519v01
	case ssh.KeyAlgoMLDSA44:
		certificate = ssh.CertAlgoMLDSA44v01Go
	case ssh.KeyAlgoMLDSA65:
		certificate = ssh.CertAlgoMLDSA65v01Go
	case ssh.KeyAlgoMLDSA87:
		certificate = ssh.CertAlgoMLDSA87v01Go
	}
	if certificate != "" {
		algorithms = append(algorithms, certificate)
	}
	return algorithms
}

func hasHostKeyEntries(contents []byte) bool {
	for line := range strings.SplitSeq(string(contents), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line != "" && !strings.HasPrefix(line, "#") {
			return true
		}
	}
	return false
}

func readFailureCount(path string) (count int, returnErr error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("opening state directory: %w", err)
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("closing authentication counter directory: %w", closeErr))
		}
	}()

	contents, err := root.ReadFile(filepath.Base(path))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("reading %s: %w", path, err)
	}
	count, err = strconv.Atoi(strings.TrimSpace(string(contents)))
	if err != nil || count < 0 {
		return 0, fmt.Errorf("invalid authentication failure counter in %s", path)
	}
	return count, nil
}

func writeFailureCount(path string, count int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating state directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(count)+"\n"), 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("setting mode on %s: %w", path, err)
	}
	return nil
}

func resetFailureCount(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing %s: %w", path, err)
	}
	return nil
}

func closeConnection(conn net.Conn, logger *slog.Logger) {
	if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		logger.Warn("closing TCP connection", "error", err)
	}
}

type systemSSHClient struct{}

func (systemSSHClient) Connect(ctx context.Context, conn net.Conn, address, user string, hostKeyCallback ssh.HostKeyCallback, hostKeyAlgorithms []string, readPassword func() (string, error), passwordOffered func()) (returnErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	closed := make(chan error, 1)
	stop := context.AfterFunc(ctx, func() { closed <- conn.Close() })
	defer func() {
		if !stop() {
			if closeErr := <-closed; closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
				returnErr = errors.Join(returnErr, fmt.Errorf("closing SSH connection after cancellation: %w", closeErr))
			}
		}
	}()
	sshConfig := &ssh.ClientConfig{
		User:              user,
		HostKeyCallback:   hostKeyCallback,
		HostKeyAlgorithms: hostKeyAlgorithms,
		Auth: []ssh.AuthMethod{
			ssh.PasswordCallback(func() (string, error) {
				password, err := readPassword()
				if err != nil {
					return "", err
				}
				passwordOffered()
				return password, nil
			}),
			ssh.KeyboardInteractive(func(_ string, _ string, questions []string, echos []bool) ([]string, error) {
				if len(questions) != 1 || len(echos) != 1 || echos[0] {
					return nil, errors.New("refusing unexpected keyboard-interactive prompt")
				}
				password, err := readPassword()
				if err != nil {
					return nil, err
				}
				passwordOffered()
				return []string{password}, nil
			}),
		},
	}
	sshConn, _, _, err := ssh.NewClientConn(conn, address, sshConfig)
	if err != nil {
		return err
	}
	return sshConn.Close()
}
