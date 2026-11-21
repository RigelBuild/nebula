package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"time"
)

// recallTimeout matches the OMP Hindsight client's default recall timeout.
const recallTimeout = 30 * time.Second

// safeIDPattern is the memory-id shape the dreamer accepts: letters, digits, underscore and hyphen.
var safeIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// recallRequest is the body of POST /v1/default/banks/{bank_id}/memories/recall.
type recallRequest struct {
	Query     string `json:"query"`
	MaxTokens int    `json:"max_tokens"`
}

// recallResult is one memory in the response. Hindsight requires id and text.
type recallResult struct {
	ID   *string `json:"id"`
	Text *string `json:"text"`
}

type recallResponse struct {
	Results []recallResult `json:"results"`
}

// recall posts one query to a bank and returns its results after checking the shape.
// Path, body and response follow the Hindsight REST API (see README.md).
func recall(ctx context.Context, baseURL, apiKey, bank, query string, maxTokens int) (results []recallResult, err error) {
	payload, err := json.Marshal(recallRequest{Query: query, MaxTokens: maxTokens})
	if err != nil {
		return nil, fmt.Errorf("bank %q: encode recall request: %w", bank, err)
	}
	ctx, cancel := context.WithTimeout(ctx, recallTimeout)
	defer cancel()
	endpoint := baseURL + "/v1/default/banks/" + url.PathEscape(bank) + "/memories/recall"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("bank %q: build recall request: %w", bank, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bank %q: recall request failed: %w", bank, err)
	}
	defer func() { err = errors.Join(err, resp.Body.Close()) }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("bank %q: recall returned HTTP %d", bank, resp.StatusCode)
	}
	var decoded recallResponse
	if decodeErr := json.NewDecoder(resp.Body).Decode(&decoded); decodeErr != nil {
		return nil, fmt.Errorf("bank %q: malformed recall response: %w", bank, decodeErr)
	}
	// An empty results array decodes to a non-nil slice, so nil means the field was absent or null.
	if decoded.Results == nil {
		return nil, fmt.Errorf("bank %q: malformed recall response: missing results", bank)
	}
	for i, r := range decoded.Results {
		if r.ID == nil || *r.ID == "" {
			return nil, fmt.Errorf("bank %q: malformed recall response: result %d has no id", bank, i)
		}
		// The id is printed into the dreamer's shell input and the skip list, so it must be a plain token.
		if !safeIDPattern.MatchString(*r.ID) {
			return nil, fmt.Errorf("bank %q: malformed recall response: result %d has an unsafe id", bank, i)
		}
		if r.Text == nil {
			return nil, fmt.Errorf("bank %q: malformed recall response: result %d has no text", bank, i)
		}
	}
	return decoded.Results, nil
}
