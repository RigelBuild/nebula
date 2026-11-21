// SPDX-License-Identifier: MIT OR Apache-2.0

// Package linearauth mints and revokes Linear app-actor OAuth tokens.
package linearauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	tokenURL  = "https://api.linear.app/oauth/token" //nolint:gosec // An endpoint URL, not a credential.
	revokeURL = "https://api.linear.app/oauth/revoke"
)

// headerWait bounds a request until response headers arrive, like the TS fetch abort.
const headerWait = 10 * time.Second

// Credentials are the OAuth client credentials for the Linear app.
type Credentials struct {
	ClientID     string
	ClientSecret string
}

// Token is a minted app-actor bearer token and its OAuth response metadata.
type Token struct {
	AccessToken string
	TokenType   string
	Scope       string
	ExpiresIn   int
}

// MintAppToken requests a client-credentials token with the given scope.
func MintAppToken(ctx context.Context, client *http.Client, credentials Credentials, scope string) (Token, error) {
	form := url.Values{
		"grant_type": {"client_credentials"},
		"scope":      {scope},
	}
	status, body, err := postForm(ctx, client, tokenURL, credentials, form)
	if err != nil {
		return Token{}, fmt.Errorf("requesting Linear app token: %w", err)
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return Token{}, fmt.Errorf(
			"client_credentials token request failed: %d %s\n%s",
			status,
			http.StatusText(status),
			body,
		)
	}

	var payload struct {
		AccessToken string          `json:"access_token"`
		TokenType   string          `json:"token_type"`
		Scope       json.RawMessage `json:"scope"`
		ExpiresIn   int             `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return Token{}, fmt.Errorf("decoding Linear token response: %w", err)
	}
	var responseScope string
	var scopeMetadata any
	if len(payload.Scope) > 0 {
		if err := json.Unmarshal(payload.Scope, &scopeMetadata); err != nil {
			return Token{}, fmt.Errorf("decoding Linear token scope: %w", err)
		}
	}
	switch scope := scopeMetadata.(type) {
	case string:
		responseScope = scope
	case []any:
		scopes := make([]string, 0, len(scope))
		valid := true
		for _, item := range scope {
			value, ok := item.(string)
			if !ok {
				valid = false
				break
			}
			scopes = append(scopes, value)
		}
		if valid {
			responseScope = strings.Join(scopes, ",")
		}
	}
	if payload.AccessToken == "" {
		var responseJSON any
		if err := json.Unmarshal(body, &responseJSON); err != nil {
			return Token{}, fmt.Errorf("decoding Linear token response: %w", err)
		}
		encoded, err := json.Marshal(responseJSON)
		if err != nil {
			return Token{}, fmt.Errorf("encoding Linear token response: %w", err)
		}
		return Token{}, fmt.Errorf("client_credentials request returned no access_token: %s", encoded)
	}
	return Token{
		AccessToken: payload.AccessToken,
		TokenType:   payload.TokenType,
		Scope:       responseScope,
		ExpiresIn:   payload.ExpiresIn,
	}, nil
}

func RevokeAppToken(ctx context.Context, client *http.Client, credentials Credentials, token Token) error {
	form := url.Values{"token": {token.AccessToken}}
	status, _, err := postForm(ctx, client, revokeURL, credentials, form)
	if err != nil {
		return fmt.Errorf("requesting Linear app token revocation: %w", err)
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return fmt.Errorf("client_credentials token revocation failed: %d", status)
	}
	return nil
}

// postForm sends a Basic-authenticated form POST and returns the status and full body.
func postForm(ctx context.Context, client *http.Client, endpoint string, credentials Credentials, form url.Values) (int, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, fmt.Errorf("creating Linear OAuth request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(credentials.ClientID+":"+credentials.ClientSecret)))

	if client == nil {
		client = http.DefaultClient
	}
	// Bound dial, TLS and headers like the fetch abort; the body read stays unbounded.
	requestCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	timer := time.AfterFunc(headerWait, cancel)
	response, err := client.Do(request.WithContext(requestCtx))
	timer.Stop()
	if err != nil {
		return 0, nil, fmt.Errorf("sending Linear OAuth request: %w", err)
	}
	body, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return 0, nil, fmt.Errorf("reading Linear OAuth response: %w", err)
	}
	return response.StatusCode, body, nil
}
