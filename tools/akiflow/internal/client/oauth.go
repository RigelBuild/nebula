package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"

	"github.com/RigelBuild/nebula/tools/akiflow/internal/core"
)

// registrationRequest is the RFC 7591 client registration body. Field order matches the TS request.
type registrationRequest struct {
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
}

// register creates a dynamic OAuth client for the loopback redirect URI.
func (a *Akiflow) register(ctx context.Context, redirectURI string) (string, error) {
	value, err := a.postJSON(ctx, a.endpoints.register, registrationRequest{
		ClientName:              clientName,
		RedirectURIs:            []string{redirectURI},
		GrantTypes:              []string{core.GrantAuthCode, core.GrantRefreshToken},
		ResponseTypes:           []string{core.ParamCode},
		TokenEndpointAuthMethod: "none",
	})
	if err != nil {
		return "", err
	}
	record, ok := core.AsRecord(value)
	if !ok {
		return "", errors.New("Invalid OAuth client registration response") //nolint:staticcheck // ST1005: user-facing message, text is fixed.
	}
	return core.RequiredString(record["client_id"], "client_id")
}

// exchangeCode trades the authorization code and PKCE verifier for tokens.
func (a *Akiflow) exchangeCode(ctx context.Context, code, redirectURI, clientID, verifier string) (core.Tokens, error) {
	value, err := a.postForm(ctx, a.endpoints.token, [][2]string{
		{"grant_type", core.GrantAuthCode},
		{"code", code},
		{"redirect_uri", redirectURI},
		{core.ParamClientID, clientID},
		{"code_verifier", verifier},
	})
	if err != nil {
		return core.Tokens{}, err
	}
	record, ok := core.AsRecord(value)
	if !ok {
		return core.Tokens{}, errors.New("Invalid token response") //nolint:staticcheck // ST1005: user-facing message, text is fixed.
	}
	record["client_id"] = clientID
	return core.ParseTokens(record, a.now())
}

// refresh exchanges the refresh token. A reply without refresh_token keeps the old one.
func (a *Akiflow) refresh(ctx context.Context, tokens core.Tokens) (core.Tokens, error) {
	value, err := a.postForm(ctx, a.endpoints.token, [][2]string{
		{"grant_type", core.GrantRefreshToken},
		{"refresh_token", tokens.RefreshToken},
		{"client_id", tokens.ClientID},
	})
	if err != nil {
		return core.Tokens{}, err
	}
	result, ok := core.AsRecord(value)
	if !ok {
		return core.Tokens{}, errors.New("Invalid token refresh response") //nolint:staticcheck // ST1005: user-facing message, text is fixed.
	}
	merged := map[string]any{"refresh_token": tokens.RefreshToken}
	maps.Copy(merged, result)
	merged["client_id"] = tokens.ClientID
	return core.ParseTokens(merged, a.now())
}

// postJSON sends body as JSON. Registration uses this form (RFC 7591).
func (a *Akiflow) postJSON(ctx context.Context, target string, body any) (any, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode oauth request: %w", err)
	}
	return a.doOAuth(ctx, target, "application/json", payload)
}

// postForm sends pairs as a form body. Token requests use this form (RFC 6749).
func (a *Akiflow) postForm(ctx context.Context, target string, pairs [][2]string) (any, error) {
	return a.doOAuth(ctx, target, "application/x-www-form-urlencoded", []byte(core.FormEncode(pairs)))
}

func (a *Akiflow) doOAuth(ctx context.Context, target, contentType string, payload []byte) (value any, err error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build oauth request: %w", err)
	}
	request.Header.Set(core.HeaderContentType, contentType)
	request.Header.Set(core.HeaderAccept, "application/json")
	response, err := a.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("akiflow oauth request: %w", err)
	}
	defer func() { err = errors.Join(err, response.Body.Close()) }()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("read oauth response: %w", err)
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("Akiflow OAuth returned invalid JSON (%d)", response.StatusCode)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode > 299 {
		return nil, fmt.Errorf("Akiflow OAuth failed (%d): %s", response.StatusCode, oauthError(value))
	}
	return value, nil
}

// oauthError prefers error_description, then error, as the OAuth error body does.
func oauthError(value any) string {
	record, ok := core.AsRecord(value)
	if !ok {
		return "unknown error"
	}
	if description, ok := record["error_description"].(string); ok {
		return description
	}
	if code, ok := record["error"].(string); ok {
		return code
	}
	return "unknown error"
}
