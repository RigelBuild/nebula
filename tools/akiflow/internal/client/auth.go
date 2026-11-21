package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/RigelBuild/nebula/tools/akiflow/internal/core"
)

// httpError is a non-2xx MCP reply. hasSession reports whether the request carried a session id.
type httpError struct {
	status     int
	hasSession bool
}

func (e httpError) Error() string {
	return fmt.Sprintf("Akiflow MCP returned HTTP %d", e.status)
}

func isUnauthorized(err error) bool {
	replyErr, ok := errors.AsType[httpError](err)
	return ok && replyErr.status == http.StatusUnauthorized
}

// loadTokens returns the stored credential. found is false when no login exists.
func (a *Akiflow) loadTokens(ctx context.Context) (core.Tokens, bool, error) {
	raw, err := a.store.Get(ctx)
	if err != nil {
		return core.Tokens{}, false, err
	}
	if raw == "" {
		return core.Tokens{}, false, nil
	}
	tokens, err := core.ParseStoredTokens(raw)
	if err != nil {
		return core.Tokens{}, false, err
	}
	return tokens, true, nil
}

// storeTokens saves the credential as compact JSON with no HTML escaping, as JSON.stringify writes it.
func (a *Akiflow) storeTokens(ctx context.Context, tokens core.Tokens) error {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(tokens); err != nil { //nolint:gosec // G117: marshal goes only to the Keychain via security -i stdin, never argv, stdout or a log
		return fmt.Errorf("encode akiflow tokens: %w", err)
	}
	return a.store.Set(ctx, string(bytes.TrimSuffix(buffer.Bytes(), []byte("\n"))))
}

// loginHintError is the refresh failure the user sees. Its message is the login hint; Unwrap keeps the cause.
type loginHintError struct {
	cause error
}

func (e loginHintError) Error() string {
	return "Akiflow token refresh failed; run: akiflow login"
}

func (e loginHintError) Unwrap() error {
	return e.cause
}

// refreshAndStore refreshes the credential and saves it. Any failure becomes loginHintError.
func (a *Akiflow) refreshAndStore(ctx context.Context, tokens core.Tokens) (core.Tokens, error) {
	refreshed, err := a.refresh(ctx, tokens)
	if err == nil {
		err = a.storeTokens(ctx, refreshed)
	}
	if err != nil {
		return core.Tokens{}, loginHintError{cause: err}
	}
	return refreshed, nil
}

// withAuthenticatedToken runs operation with a valid access token. It refreshes once when the
// stored token has expired, and once more when the server rejects the token with 401.
func (a *Akiflow) withAuthenticatedToken(ctx context.Context, operation func(accessToken string) error) error {
	tokens, found, err := a.loadTokens(ctx)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("No Akiflow login found; run: akiflow login") //nolint:staticcheck // ST1005: user-facing message, text is fixed.
	}
	if tokens.ExpiresAt <= a.now() {
		if tokens, err = a.refreshAndStore(ctx, tokens); err != nil {
			return err
		}
	}
	err = operation(tokens.AccessToken)
	if !isUnauthorized(err) {
		return err
	}
	if tokens, err = a.refreshAndStore(ctx, tokens); err != nil {
		return err
	}
	retryErr := operation(tokens.AccessToken)
	if isUnauthorized(retryErr) {
		return errors.New("Akiflow authorization failed after token refresh; run: akiflow login")
	}
	return retryErr
}
