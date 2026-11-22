package keychain

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os/exec" //nolint:depguard // Keychain and browser access run fixed system tools with argv, never a shell.
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	securityBinary = "/usr/bin/security"
	openBinary     = "/usr/bin/open"
	CallbackHost   = "127.0.0.1"
)

var (
	hexPattern          = regexp.MustCompile(`^[0-9a-f]+$`)
	keychainNamePattern = regexp.MustCompile(`[\s'"]`)
)

// decodeKeychainValue reverses the hex encoding that `security -w` can apply to JSON.
// Anything else is returned with trailing whitespace trimmed.
func decodeKeychainValue(value string) string {
	trimmed := strings.TrimRightFunc(value, unicode.IsSpace)
	if trimmed == "" || len(trimmed)%2 != 0 || !hexPattern.MatchString(trimmed) {
		return trimmed
	}
	raw, err := hex.DecodeString(trimmed)
	if err != nil || !utf8.Valid(raw) || !json.Valid(raw) {
		return trimmed
	}
	return string(raw)
}

// CommandResult is the exit code and captured output of one finished command.
type CommandResult struct {
	exitCode int
	stdout   []byte
	stderr   []byte
}

// CommandRunner runs argv with optional stdin. Tests inject a fake; production uses ExecRunner.
type CommandRunner func(ctx context.Context, argv []string, stdin string) (CommandResult, error)

// ExecRunner runs argv with stdin and returns its exit code and captured output.
func ExecRunner(ctx context.Context, argv []string, stdin string) (CommandResult, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // G204: argv is a fixed system tool, no shell.
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	result := CommandResult{}
	err := cmd.Run()
	result.stdout = stdout.Bytes()
	result.stderr = stderr.Bytes()
	if err == nil {
		return result, nil
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		result.exitCode = exitErr.ExitCode()
		return result, nil
	}
	return CommandResult{}, fmt.Errorf("run %s: %w", argv[0], err)
}

// OpenBrowser opens target with /usr/bin/open and returns its exit code.
func OpenBrowser(ctx context.Context, target string) (int, error) {
	result, err := ExecRunner(ctx, []string{openBinary, target}, "")
	return result.exitCode, err
}

// CredentialStore reads and writes the single Akiflow credential item.
type CredentialStore interface {
	// get returns the decoded credential, or "" when no item exists.
	Get(ctx context.Context) (string, error)
	Set(ctx context.Context, value string) error
}

// keychainStore implements CredentialStore with the macOS security tool.
type keychainStore struct {
	service string
	account string
	run     CommandRunner
}

// NewKeychainStore validates the Keychain service and account names and returns the store.
func NewKeychainStore(service, account string, run CommandRunner) (CredentialStore, error) {
	if keychainNamePattern.MatchString(service) {
		return keychainStore{}, errors.New("Invalid Keychain service: whitespace and quotes are not allowed") //nolint:staticcheck // ST1005: user-facing message, text is fixed.
	}
	if keychainNamePattern.MatchString(account) {
		return keychainStore{}, errors.New("Invalid Keychain account: whitespace and quotes are not allowed") //nolint:staticcheck // ST1005: user-facing message, text is fixed.
	}
	return keychainStore{service: service, account: account, run: run}, nil
}

func (k keychainStore) Get(ctx context.Context) (string, error) {
	result, err := k.run(ctx, []string{
		securityBinary, "find-generic-password", "-w", "-s", k.service, "-a", k.account,
	}, "")
	if err != nil {
		return "", err
	}
	if result.exitCode != 0 {
		return "", nil
	}
	return decodeKeychainValue(string(result.stdout)), nil
}

// set writes the value through `security -i` on stdin, so the secret never appears in argv.
func (k keychainStore) Set(ctx context.Context, value string) error {
	script := fmt.Sprintf("add-generic-password -U -s %s -a %s -X %s\n",
		k.service, k.account, hex.EncodeToString([]byte(value)))
	result, err := k.run(ctx, []string{securityBinary, "-i"}, script)
	if err != nil {
		return err
	}
	if result.exitCode != 0 {
		return fmt.Errorf("Keychain write failed: %s", strings.TrimSpace(string(result.stderr))) //nolint:staticcheck // ST1005: user-facing message, text is fixed.
	}
	return nil
}

// CallbackStatus is the outcome of one OAuth callback request.
type CallbackStatus int

const (
	CallbackIgnored CallbackStatus = iota
	CallbackSucceeded
	CallbackFailed
)

// CallbackResult is how the login flow answers one OAuth redirect.
type CallbackResult struct {
	Status CallbackStatus
}

// handleCallbackRequest maps one OAuth redirect to the HTTP status and body the browser sees.
func handleCallbackRequest(target *url.URL, onCallback func(*url.URL) CallbackResult) (int, string) {
	if target.Path != "/callback" {
		return http.StatusNotFound, "Not found"
	}
	switch onCallback(target).Status {
	case CallbackSucceeded:
		return http.StatusOK, "Akiflow login complete. You can close this tab."
	case CallbackFailed:
		return http.StatusBadRequest, "Akiflow login failed. Return to the terminal for details."
	case CallbackIgnored:
		return http.StatusBadRequest, "Invalid OAuth state"
	}
	return http.StatusInternalServerError, "Unexpected callback status"
}

// CallbackServer serves the OAuth redirect and returns a stop function.
type CallbackServer func(port int, onCallback func(*url.URL) CallbackResult) (func() error, error)

// StartCallbackServer listens on the loopback interface only.
func StartCallbackServer(port int, onCallback func(*url.URL) CallbackResult) (func() error, error) {
	listener, err := net.Listen("tcp", net.JoinHostPort(CallbackHost, strconv.Itoa(port)))
	if err != nil {
		return nil, fmt.Errorf("listen for oauth callback: %w", err)
	}
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			status, body := handleCallbackRequest(r.URL, onCallback)
			w.WriteHeader(status)
			if _, err := io.WriteString(w, body); err != nil {
				slog.Warn("write oauth callback response", "error", err)
			}
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("oauth callback server stopped", "error", err)
		}
	}()
	return server.Close, nil
}
