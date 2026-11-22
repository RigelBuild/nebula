package client

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/RigelBuild/nebula/tools/akiflow/internal/core"
	"github.com/RigelBuild/nebula/tools/akiflow/internal/keychain"
)

// loginOutcome carries the authorization code, or the callback failure, to the waiting login.
type loginOutcome struct {
	code string
	err  error
}

// login runs the browser OAuth flow on the loopback callback and stores the tokens.
func (a *Akiflow) login(ctx context.Context, stdout io.Writer) error {
	verifier, state, err := newPkceState()
	if err != nil {
		return err
	}
	outcome := make(chan loginOutcome, 1)
	var handled atomic.Bool
	stop, err := a.serve(callbackPort, func(target *url.URL) keychain.CallbackResult {
		return handleLoginCallback(target, state, &handled, outcome)
	})
	if err != nil {
		return err
	}
	defer stopServer(stop)

	redirectURI := "http://" + net.JoinHostPort(keychain.CallbackHost, strconv.Itoa(callbackPort)) + "/callback"
	clientID, err := a.register(ctx, redirectURI)
	if err != nil {
		return err
	}
	authURL := core.BuildAuthorizationURL(clientID, redirectURI, state, core.MakePkceChallenge(verifier))
	timer := time.NewTimer(loginTimeout)
	defer timer.Stop()
	exitCode, err := a.open(ctx, authURL)
	if err != nil {
		return err
	}
	if exitCode != 0 {
		return errors.New("Could not open browser with /usr/bin/open") //nolint:staticcheck // ST1005: user-facing message, text is fixed.
	}
	code, err := awaitLoginCode(ctx, outcome, timer.C)
	if err != nil {
		return err
	}
	tokens, err := a.exchangeCode(ctx, code, redirectURI, clientID, verifier)
	if err != nil {
		return err
	}
	if err := a.storeTokens(ctx, tokens); err != nil {
		return err
	}
	return WriteLine(stdout, "Akiflow login saved to Keychain.")
}

// newPkceState returns a fresh PKCE verifier and OAuth state, each from 32 random bytes.
func newPkceState() (string, string, error) {
	verifierBytes, err := randomBytes(32)
	if err != nil {
		return "", "", err
	}
	stateBytes, err := randomBytes(32)
	if err != nil {
		return "", "", err
	}
	return core.MakePkceVerifier(verifierBytes), core.MakeState(stateBytes), nil
}

func randomBytes(size int) ([]byte, error) {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("read random bytes: %w", err)
	}
	return buf, nil
}

// handleLoginCallback accepts the first callback that carries the expected state and a code or an error.
func handleLoginCallback(target *url.URL, state string, handled *atomic.Bool, outcome chan<- loginOutcome) keychain.CallbackResult {
	query := target.Query()
	if query.Get("state") != state {
		return keychain.CallbackResult{Status: keychain.CallbackIgnored}
	}
	if !query.Has("error") && !query.Has("code") {
		return keychain.CallbackResult{Status: keychain.CallbackIgnored}
	}
	if !handled.CompareAndSwap(false, true) {
		return keychain.CallbackResult{Status: keychain.CallbackIgnored}
	}
	code, err := core.CallbackCode(target, state)
	if err != nil {
		outcome <- loginOutcome{err: err}
		return keychain.CallbackResult{Status: keychain.CallbackFailed}
	}
	outcome <- loginOutcome{code: code}
	return keychain.CallbackResult{Status: keychain.CallbackSucceeded}
}

func awaitLoginCode(ctx context.Context, outcome <-chan loginOutcome, timeout <-chan time.Time) (string, error) {
	select {
	case result := <-outcome:
		return result.code, result.err
	case <-timeout:
		return "", errors.New("OAuth login timed out after 5 minutes")
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func stopServer(stop func() error) {
	if err := stop(); err != nil {
		slog.Warn("stop oauth callback server", "error", err)
	}
}
