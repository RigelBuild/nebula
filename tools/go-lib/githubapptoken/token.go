// SPDX-License-Identifier: MIT OR Apache-2.0

// Package githubapptoken mints GitHub App installation tokens.
package githubapptoken

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/samber/oops"
)

// Token is a GitHub installation token.
type Token string

// Mint resolves the repository installation and returns a new installation token.
func Mint(ctx context.Context, appID, repo string, pemBytes []byte) (Token, error) {
	return mintWithClient(ctx, appID, repo, pemBytes, http.DefaultClient)
}

// MintWithClient resolves the repository installation with the supplied HTTP client.
func MintWithClient(ctx context.Context, appID, repo string, pemBytes []byte, client *http.Client) (Token, error) {
	return mintWithClient(ctx, appID, repo, pemBytes, client)
}

// MintScoped resolves the repository installation and returns a token limited to that repository and its permissions.
func MintScoped(ctx context.Context, appID, repo string, pemBytes []byte, perms map[string]string) (Token, error) {
	return mintScopedWithClient(ctx, appID, repo, pemBytes, perms, http.DefaultClient)
}

// MintScopedWithClient resolves the repository installation with the supplied HTTP client.
func MintScopedWithClient(ctx context.Context, appID, repo string, pemBytes []byte, perms map[string]string, client *http.Client) (Token, error) {
	return mintScopedWithClient(ctx, appID, repo, pemBytes, perms, client)
}

func mintScopedWithClient(ctx context.Context, appID, repo string, pemBytes []byte, perms map[string]string, client *http.Client) (Token, error) {
	if len(perms) == 0 {
		return "", oops.In("github-app-token").Code("missing_permissions").Errorf("at least one permission is required for a scoped token")
	}
	repoName := repo
	if _, name, found := strings.Cut(repo, "/"); found {
		repoName = name
	}
	return mintWithClientRequest(ctx, appID, repo, pemBytes, client, &installationTokenRequest{
		Repositories: []string{repoName},
		Permissions:  perms,
	})
}

func mintWithClient(ctx context.Context, appID, repo string, pemBytes []byte, client *http.Client) (Token, error) {
	return mintWithClientRequest(ctx, appID, repo, pemBytes, client, nil)
}

func mintWithClientRequest(ctx context.Context, appID, repo string, pemBytes []byte, client *http.Client, tokenRequest *installationTokenRequest) (result Token, resultErr error) {
	if strings.TrimSpace(appID) == "" {
		return "", oops.In("github-app-token").Code("missing_identifier").Errorf("app identifier is required")
	}
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", oops.In("github-app-token").Code("invalid_repo").Errorf("github-app-token: repo must be in owner/name format, got: %s", repo)
	}
	if client == nil {
		return "", oops.In("github-app-token").Code("missing_http_client").Errorf("http client is required")
	}
	key, err := parsePrivateKey(pemBytes)
	if err != nil {
		return "", oops.In("github-app-token").Tags("authentication").Wrapf(err, "parse app private key")
	}
	now := time.Now()
	claims := jwt.RegisteredClaims{
		Issuer:    appID,
		IssuedAt:  jwt.NewNumericDate(now.Add(-time.Minute)),
		ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute)),
	}
	appJWT, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(key)
	if err != nil {
		return "", oops.In("github-app-token").Tags("authentication").Wrapf(err, "sign app JWT")
	}
	lookupURL := "https://api.github.com/repos/" + parts[0] + "/" + parts[1] + "/installation"
	lookup, err := http.NewRequestWithContext(ctx, http.MethodGet, lookupURL, nil)
	if err != nil {
		return "", oops.In("github-app-token").Wrapf(err, "build installation lookup request")
	}
	setGitHubHeaders(lookup, appJWT)
	response, err := client.Do(lookup)
	if err != nil {
		return "", oops.In("github-app-token").Tags("network").Wrapf(err, "request repository installation")
	}
	defer func() { resultErr = errors.Join(resultErr, response.Body.Close()) }()
	body, readErr := io.ReadAll(response.Body)
	if readErr != nil {
		return "", oops.In("github-app-token").Wrapf(readErr, "read repository installation response")
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", lookupError(response.StatusCode, repo, body)
	}
	var installation struct {
		ID json.Number `json:"id"`
	}
	if err := json.Unmarshal(body, &installation); err != nil {
		return "", oops.In("github-app-token").With("content_type", response.Header.Get("Content-Type")).Errorf("github-app-token: installation lookup for %s returned HTTP %d with a non-JSON body (expected JSON; got a %s response). Body: %s", repo, response.StatusCode, contentType(response), truncate(body, 200))
	}
	id, err := installation.ID.Int64()
	if err != nil || id <= 0 {
		return "", oops.In("github-app-token").Code("missing_installation_id").Errorf("github-app-token: installation lookup for %s returned no numeric installation id. The App may not be installed on %s", repo, repo)
	}
	return mintToken(ctx, client, appJWT, id, tokenRequest)
}

type installationTokenRequest struct {
	Repositories []string          `json:"repositories"`
	Permissions  map[string]string `json:"permissions"`
}

func mintToken(ctx context.Context, client *http.Client, appJWT string, installationID int64, tokenRequest *installationTokenRequest) (result Token, resultErr error) {
	url := fmt.Sprintf("https://api.github.com/app/installations/%d/access_tokens", installationID)
	var requestBody io.Reader
	if tokenRequest != nil {
		data, err := json.Marshal(tokenRequest)
		if err != nil {
			return "", oops.In("github-app-token").Wrapf(err, "encode installation token request")
		}
		requestBody = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, requestBody)
	if err != nil {
		return "", oops.In("github-app-token").Wrapf(err, "build installation token request")
	}
	setGitHubHeaders(request, appJWT)
	if tokenRequest != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return "", oops.In("github-app-token").Tags("network").Wrapf(err, "request installation token")
	}
	defer func() { resultErr = errors.Join(resultErr, response.Body.Close()) }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return "", oops.In("github-app-token").Wrapf(err, "read installation token response")
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", lookupError(response.StatusCode, strconv.FormatInt(installationID, 10), body)
	}
	var payload struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", oops.In("github-app-token").With("content_type", response.Header.Get("Content-Type")).Wrapf(err, "parse installation token response")
	}
	if payload.Token == "" {
		return "", oops.In("github-app-token").Code("missing_token").Errorf("installation token response did not include a token")
	}
	return Token(payload.Token), nil
}

func setGitHubHeaders(request *http.Request, appJWT string) {
	request.Header.Set("Authorization", "Bearer "+appJWT)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-Github-Api-Version", "2022-11-28")
}

func lookupError(status int, repo string, body []byte) error {
	return oops.In("github-app-token").Code("installation_lookup_failed").With("status", status).Errorf("github-app-token: installation lookup for %s failed — HTTP %d. %s Response body: %s", repo, status, diagnoseStatus(status), string(body))
}

func contentType(response *http.Response) string {
	value := response.Header.Get("Content-Type")
	if value == "" {
		return "unknown"
	}
	return value
}
func truncate(body []byte, limit int) string {
	return string(body[:min(len(body), limit)])
}

func parsePrivateKey(data []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("private key pem block is missing")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse rsa private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not rsa")
	}
	return key, nil
}

func diagnoseStatus(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "The App JWT was rejected — the private key (PEM) or client id is bad; rotate the App key."
	case http.StatusForbidden:
		return "The App is installed but lacks a required permission grant — grant it on the App installation."
	case http.StatusNotFound:
		return "The App is not installed on this repo — install it on the repo/org."
	default:
		return "Unexpected status from GitHub."
	}
}
