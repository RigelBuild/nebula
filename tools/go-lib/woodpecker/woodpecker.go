// SPDX-License-Identifier: MIT OR Apache-2.0

// Package woodpecker is the small Woodpecker REST client the CD tools use.
package woodpecker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/apiurl"
)

// dispatchTimeout bounds pipeline creation. Server-side config generation has
// taken over 30 seconds, so the default is generous.
const dispatchTimeout = 180 * time.Second

// GateTimeout bounds the CD gate's own API reads.
const GateTimeout = 30 * time.Second

// DispatchWorstCase bounds one DispatchCDTargets call with the default timeout:
// POST, a confirm window that can overrun by one list request, a 1 s sleep, the
// retry POST, and a second confirm window with the same overrun.
const DispatchWorstCase = 4*dispatchTimeout + 2*GateTimeout + time.Second

// StatusError reports a non-2xx Woodpecker API response.
type StatusError struct {
	Status int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("Woodpecker API returned %d", e.Status)
}

// ErrNoContent reports a successful HTTP response with no JSON body.
var ErrNoContent = errors.New("response has no content")

// ErrInvalidAPIContext reports a CD API context the gate cannot use: a missing
// or malformed API URL, a missing CD token, a malformed CI_REPO or CI_REPO_ID,
// or a token the server rejects with 401 or 403. The gate blocks on it, because
// a run with no valid context cannot prove it is the newest main pipeline.
// Transient API failures (network, 5xx, bad JSON) are not wrapped in it.
var ErrInvalidAPIContext = errors.New("invalid CD API context")

// ErrMissingCredential reports an unset or blank CD API credential. It wraps
// ErrInvalidAPIContext.
var ErrMissingCredential = fmt.Errorf("%w: missing CD API credential", ErrInvalidAPIContext)

// RejectedCredential maps a 401 or 403 from a Woodpecker read to
// ErrInvalidAPIContext, so the gate blocks on a token the server refuses. Any
// other error, including a 5xx or a network failure, is returned unchanged and
// fails open.
func RejectedCredential(err error) error {
	status, ok := errors.AsType[*StatusError](err)
	if !ok || (status.Status != http.StatusUnauthorized && status.Status != http.StatusForbidden) {
		return err
	}
	return fmt.Errorf("%w: Woodpecker API returned %d", ErrInvalidAPIContext, status.Status)
}

// Client talks to one Woodpecker server with a bearer token.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// Context is the API base URL, the CD token, and the numeric repo id of CI_REPO.
type Context struct {
	BaseURL string
	Token   string
	RepoID  int64
}

// FetchJSON performs one request and decodes a JSON body. A 204 returns
// ErrNoContent. A timeout of zero uses GateTimeout.
func (c *Client) FetchJSON(ctx context.Context, method, path string, body any, timeout time.Duration) (out any, err error) {
	if timeout == 0 {
		timeout = GateTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, resp.Body.Close()) }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &StatusError{Status: resp.StatusCode}
	}
	if resp.StatusCode == http.StatusNoContent {
		return nil, ErrNoContent
	}
	if err = json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return out, nil
}

// LookupRepoID resolves "owner/name" to Woodpecker's numeric repo id.
func (c *Client) LookupRepoID(ctx context.Context, repo string) (int64, error) {
	raw, err := c.FetchJSON(ctx, http.MethodGet, "/api/repos/lookup/"+repo, nil, 0)
	if errors.Is(err, ErrNoContent) {
		return 0, errors.New("repo lookup returned no content")
	}
	if err != nil {
		return 0, RejectedCredential(err)
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return 0, errors.New("invalid repo lookup response")
	}
	id, ok := obj["id"].(float64)
	if !ok || id != float64(int64(id)) || id < 1 {
		return 0, errors.New("invalid repo lookup response")
	}
	return int64(id), nil
}

var repoPattern = regexp.MustCompile(`^[^/?#\s]+/[^/?#\s]+$`)

// baseAndToken reads the API base URL and the CD token, the checks both
// ContextFromEnv entry points share. Each error names the variable at fault.
func baseAndToken(env func(string) string, inClusterURL string) (string, string, error) {
	baseURL, ok := apiurl.WoodpeckerAPI(env, inClusterURL)
	if !ok {
		return "", "", fmt.Errorf("%w: missing WOODPECKER_API_URL or CI_SYSTEM_URL", ErrInvalidAPIContext)
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", "", fmt.Errorf("%w: invalid WOODPECKER_API_URL or CI_SYSTEM_URL protocol", ErrInvalidAPIContext)
	}
	token := strings.TrimSpace(env("WOODPECKER_CD_TOKEN"))
	if token == "" {
		return "", "", fmt.Errorf("%w: WOODPECKER_CD_TOKEN", ErrMissingCredential)
	}
	return baseURL, token, nil
}

// ContextFromEnv builds the CD API context from the environment, as the TS
// cdApiContext does for cd-reconcile: it validates the base URL scheme and the
// CI_REPO shape before any network call, then looks the repo id up.
// inClusterURL is the Woodpecker API address reachable from inside the
// cluster, used when KUBERNETES_SERVICE_HOST is set (see apiurl.WoodpeckerAPI).
func ContextFromEnv(ctx context.Context, env func(string) string, inClusterURL string, httpClient *http.Client) (Context, error) {
	baseURL, token, err := baseAndToken(env, inClusterURL)
	if err != nil {
		return Context{}, err
	}
	repo, err := parseEnv(env, "CI_REPO")
	if err != nil {
		return Context{}, fmt.Errorf("%w: %w", ErrInvalidAPIContext, err)
	}
	if !repoPattern.MatchString(repo) {
		return Context{}, fmt.Errorf("%w: invalid CI_REPO", ErrInvalidAPIContext)
	}
	client := &Client{BaseURL: baseURL, Token: token, HTTP: httpClient}
	id, err := client.LookupRepoID(ctx, repo)
	if err != nil {
		return Context{}, err
	}
	return Context{BaseURL: baseURL, Token: token, RepoID: id}, nil
}

// ContextFromEnvWithRepoID is ContextFromEnv for run-cd, whose TS read a set
// CI_REPO_ID directly with no CI_REPO and no lookup.
func ContextFromEnvWithRepoID(ctx context.Context, env func(string) string, inClusterURL string, httpClient *http.Client) (Context, error) {
	explicit := strings.TrimSpace(env("CI_REPO_ID"))
	if explicit == "" {
		return ContextFromEnv(ctx, env, inClusterURL, httpClient)
	}
	baseURL, token, err := baseAndToken(env, inClusterURL)
	if err != nil {
		return Context{}, err
	}
	id, err := parsePositiveID(explicit)
	if err != nil {
		return Context{}, fmt.Errorf("%w: %w", ErrInvalidAPIContext, err)
	}
	return Context{BaseURL: baseURL, Token: token, RepoID: id}, nil
}

// MaxSafeInteger is JavaScript's Number.MAX_SAFE_INTEGER. The TS parsers reject
// anything above it, and the Go port keeps the same bound.
const MaxSafeInteger = 1<<53 - 1

// digitsOnly matches a plain decimal integer: no sign, no fraction, no space.
var digitsOnly = regexp.MustCompile(`^[0-9]+$`)

// ParseBoundedInt parses a decimal env value that must be a safe integer at
// least minimum. It rejects signs, fractions, spaces and overflow, as the TS does.
// The error names the variable.
func ParseBoundedInt(value, name string, minimum int64) (int64, error) {
	if value == "" || !digitsOnly.MatchString(value) {
		return 0, fmt.Errorf("invalid %s", name)
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n > MaxSafeInteger || n < minimum {
		return 0, fmt.Errorf("invalid %s", name)
	}
	return n, nil
}

// parsePositiveID reads CI_REPO_ID: a safe integer of at least 1.
func parsePositiveID(value string) (int64, error) {
	return ParseBoundedInt(value, "CI_REPO_ID", 1)
}

func parseEnv(env func(string) string, name string) (string, error) {
	value := strings.TrimSpace(env(name))
	if value == "" {
		return "", fmt.Errorf("missing %s", name)
	}
	return value, nil
}

// dispatchClock is the time source, the sleep, and the log the dispatch
// recovery uses. Tests replace all three, so the 180-second confirmation
// window runs instantly and the confirmation line can be read back.
type dispatchClock struct {
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
	log   io.Writer
}

var realDispatchClock = dispatchClock{
	now: time.Now,
	sleep: func(ctx context.Context, d time.Duration) error {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	},
	log: os.Stdout,
}

// DispatchCDTargets starts a manual main pipeline that runs exactly targets, as
// cd-reconcile does. The caller mints dispatchID with NewDispatchID and keeps it
// to find the pipeline later. base is the checked commit the release jobs use as
// MOON_BASE; it travels as CD_CHECK_BASE. It returns the created pipeline number,
// or 0 when Woodpecker answers 204.
func DispatchCDTargets(ctx context.Context, c Context, targets []string, dispatchID string, base string, timeout time.Duration) (int64, error) {
	return dispatchCDTargets(ctx, c, targets, dispatchID, base, timeout, realDispatchClock)
}

// dispatchCDTargets creates the pipeline and returns its number, or 0 when
// Woodpecker answers 204. A 5xx is confirmed by listing manual main pipelines
// for dispatchID, then retried once; a 4xx is returned unchanged.
func dispatchCDTargets(ctx context.Context, c Context, targets []string, dispatchID string, base string, timeout time.Duration, clock dispatchClock) (int64, error) {
	if timeout == 0 {
		timeout = dispatchTimeout
	}
	nonce := dispatchID
	client := &Client{BaseURL: c.BaseURL, Token: c.Token}
	path := fmt.Sprintf("/api/repos/%d/pipelines", c.RepoID)
	variables := map[string]string{
		"PETREL_CD_TARGETS":     strings.Join(targets, ","),
		"PETREL_CD_DISPATCH_ID": nonce,
		"CD_CHECK_BASE":         base,
	}
	if err := checkDispatchKeys(variables); err != nil {
		return 0, err
	}
	body := map[string]any{"branch": "main", "variables": variables}
	startedAt := clock.now().Unix()
	created, err := client.FetchJSON(ctx, http.MethodPost, path, body, timeout)
	if err == nil || errors.Is(err, ErrNoContent) {
		return createdPipelineNumber(created), nil
	}
	statusErr, ok := errors.AsType[*StatusError](err)
	if !ok || statusErr.Status < 500 {
		return 0, err
	}
	return recoverDispatch(ctx, client, path, body, nonce, startedAt, statusErr, timeout, clock)
}

// recoverDispatch handles a 5xx from the create: it looks for this dispatch's
// pipeline first, then retries the create once and looks again.
func recoverDispatch(ctx context.Context, client *Client, path string, body map[string]any, nonce string, startedAt int64, initial *StatusError, timeout time.Duration, clock dispatchClock) (int64, error) {
	if number, err := confirmDispatch(ctx, client, path, nonce, startedAt, initial.Status, clock); err != nil || number != 0 {
		return number, err
	}
	if err := clock.sleep(ctx, time.Second); err != nil {
		return 0, err
	}
	created, err := client.FetchJSON(ctx, http.MethodPost, path, body, timeout)
	if err == nil || errors.Is(err, ErrNoContent) {
		return createdPipelineNumber(created), nil
	}
	retryErr, ok := errors.AsType[*StatusError](err)
	if !ok || retryErr.Status < 500 {
		return 0, err
	}
	number, confirmErr := confirmDispatch(ctx, client, path, nonce, startedAt, retryErr.Status, clock)
	if confirmErr != nil {
		return 0, confirmErr
	}
	if number != 0 {
		return number, nil
	}
	return 0, fmt.Errorf("CD dispatch failed after retry: %w", err)
}

// confirmDispatch polls the manual main pipelines for this nonce until the
// 180-second window closes. It returns 0 when no pipeline carries the nonce.
func confirmDispatch(ctx context.Context, client *Client, path, nonce string, startedAt int64, status int, clock dispatchClock) (int64, error) {
	deadline := clock.now().Add(dispatchTimeout)
	for {
		number, err := FindDispatch(ctx, client, path, nonce, startedAt)
		if err != nil {
			return 0, fmt.Errorf("API returned %d; could not confirm the CD dispatch: %w", status, err)
		}
		if number != 0 {
			if _, err := fmt.Fprintf(clock.log, "cd-dispatch: confirmed pipeline %d after Woodpecker returned %d\n", number, status); err != nil {
				return 0, fmt.Errorf("log confirmed CD dispatch: %w", err)
			}
			return number, nil
		}
		remaining := deadline.Sub(clock.now())
		if remaining <= 0 {
			return 0, nil
		}
		wait := min(10*time.Second, remaining)
		if err := clock.sleep(ctx, wait); err != nil {
			return 0, err
		}
	}
}

// FindDispatch lists the newest manual main pipelines and returns the number
// of the one carrying nonce. A full page entirely inside the window fails
// closed, since the match could sit past the page.
func FindDispatch(ctx context.Context, client *Client, path, nonce string, startedAt int64) (int64, error) {
	query := url.Values{}
	query.Set("branch", "main")
	query.Set("event", "manual")
	query.Set("page", "1")
	query.Set("perPage", "50")
	raw, err := client.FetchJSON(ctx, http.MethodGet, path+"?"+query.Encode(), nil, 0)
	if err != nil {
		return 0, err
	}
	list, ok := raw.([]any)
	if !ok {
		return 0, errors.New("invalid pipeline list response")
	}
	oldest := int64(math.MaxInt64)
	for _, entry := range list {
		item, ok := entry.(map[string]any)
		if !ok {
			return 0, errors.New("invalid pipeline list item fields")
		}
		number, numOK := item["number"].(float64)
		created, createdOK := item["created"].(float64)
		branch, branchOK := item["branch"].(string)
		event, eventOK := item["event"].(string)
		if !numOK || number != float64(int64(number)) || number < 1 || !createdOK || !branchOK || !eventOK {
			return 0, errors.New("invalid pipeline list item fields")
		}
		if int64(created) < oldest {
			oldest = int64(created)
		}
		variables, err := PipelineVariables(item)
		if err != nil {
			return 0, err
		}
		if branch == "main" && event == "manual" && variables["PETREL_CD_DISPATCH_ID"] == nonce {
			return int64(number), nil
		}
	}
	if len(list) >= 50 && oldest >= startedAt-60 {
		return 0, errors.New("pipeline lookup reached its page limit inside the dispatch window")
	}
	return 0, nil
}

// PipelineVariables reads an item's optional variables: absent, or an object
// whose values are all strings. Any other shape fails closed, as in the TS.
func PipelineVariables(item map[string]any) (map[string]string, error) {
	raw, present := item["variables"]
	if !present {
		return map[string]string{}, nil
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("invalid pipeline variables")
	}
	variables := make(map[string]string, len(obj))
	for key, value := range obj {
		text, ok := value.(string)
		if !ok {
			return nil, errors.New("invalid pipeline variables")
		}
		variables[key] = text
	}
	return variables, nil
}

// NewDispatchID returns a random v4 UUID, the PETREL_CD_DISPATCH_ID a caller
// passes to DispatchCDTargets.
func NewDispatchID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("read dispatch nonce: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// createdPipelineNumber reads the number from a create reply, or 0 when the
// reply has none (a 204 has no body).
func createdPipelineNumber(value any) int64 {
	obj, ok := value.(map[string]any)
	if !ok {
		return 0
	}
	number, ok := obj["number"].(float64)
	if !ok || number != float64(int64(number)) || number < 1 {
		return 0
	}
	return int64(number)
}
