// Package mint keeps the scoped Garage admin token in a Kubernetes Secret current.
package mint

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/RigelBuild/nebula/tools/garage-bucket-reconcile/internal/garage"
	"github.com/RigelBuild/nebula/tools/garage-bucket-reconcile/internal/jscompat"
)

// Scope is the exact admin scope the reconcile Job needs.
var Scope = []string{"ListBuckets", "CreateBucket", "GetKeyInfo", "UpdateBucket", "AllowBucketKey"}

// TTL and RenewBefore set the token lifetime and the remaining time that triggers a re-mint.
const (
	TTL         = 30 * 24 * time.Hour
	RenewBefore = 7 * 24 * time.Hour
)

// ErrSecretConflict reports that the Secret changed between read and replace.
var ErrSecretConflict = errors.New("Kubernetes Secret changed before token publication") //nolint:staticcheck // ST1005: user-facing message, text is fixed.

// Config describes the Secret to publish and the token to mint.
type Config struct {
	AdminURL    string
	MasterToken string
	Namespace   string
	SecretName  string
	TokenName   string
	Scope       []string
	TTL         time.Duration
	RenewBefore time.Duration
	// ReadyWait bounds the admin readiness poll. Zero means garage.DefaultReadyTimeout.
	ReadyWait time.Duration
}

// SecretClient reads and replaces one Kubernetes Secret.
type SecretClient interface {
	Read(ctx context.Context, namespace, name string) (map[string]any, error)
	Replace(ctx context.Context, namespace, name string, body map[string]any) error
}

// Deps holds the side effects the mint flow needs.
type Deps struct {
	Garage  garage.Deps
	Secrets SecretClient
}

// tokenInfo is the subset of GetCurrentAdminTokenInfo the mint flow reads.
type tokenInfo struct {
	id         *string
	expired    bool
	scope      []string
	expiration *time.Time
}

// minted is a token this run created: its ID for deletion and its secret value.
type minted struct {
	id          string
	secretToken string
}

// namedToken is one ListAdminTokens entry, enough to find same-name tokens.
type namedToken struct {
	id   *string
	name string
}

// apiResult is one admin response after the status and empty-body rules.
type apiResult struct {
	status int
	body   any
}

// Run keeps a current token in the Secret. It returns "kept" or "minted".
func Run(ctx context.Context, cfg Config, deps Deps) (string, error) {
	deps.Garage.Log("Waiting for Garage admin API readiness before token minting")
	if err := garage.WaitForAdmin(ctx, cfg.AdminURL, readyWait(cfg.ReadyWait), deps.Garage); err != nil {
		return "", err
	}
	current, err := deps.Secrets.Read(ctx, cfg.Namespace, cfg.SecretName)
	if err != nil {
		return "", err
	}
	if token := tokenFromSecret(current); token != "" {
		kept, err := tokenIsCurrent(ctx, cfg, deps, token)
		if err != nil {
			return "", err
		}
		if kept {
			deps.Garage.Log("Garage bucket reconcile token is current")
			return "kept", nil
		}
	}

	token, err := createToken(ctx, cfg, deps)
	if err != nil {
		return "", err
	}
	published, err := publishToken(ctx, cfg, deps, current, token)
	if err != nil {
		return "", err
	}
	if !published {
		return "minted", nil
	}
	if err := deleteOlderTokens(ctx, cfg, deps, token); err != nil {
		return "", err
	}
	deps.Garage.Log("Published a new Garage bucket reconcile token")
	return "minted", nil
}

// readyWait returns the configured readiness timeout, or garage.DefaultReadyTimeout when unset.
func readyWait(configured time.Duration) time.Duration {
	if configured == 0 {
		return garage.DefaultReadyTimeout
	}
	return configured
}

// tokenFromSecret returns the decoded token, or "" when the Secret holds none.
// Node's Buffer.from(value, "base64") skips invalid characters and also accepts unpadded and URL-safe input.
// StdEncoding rejects all three, so a malformed token returns "" and the run mints a new one. The API server
// writes canonical base64, so this difference cannot occur in practice.
func tokenFromSecret(secret map[string]any) string {
	data, ok := garage.Record(secret["data"])
	if !ok {
		return ""
	}
	encoded, ok := data["token"].(string)
	if !ok {
		return ""
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return ""
	}
	return string(decoded)
}

// isoMillis matches JavaScript toISOString: UTC with exactly three fractional digits.
const isoMillis = "2006-01-02T15:04:05.000Z07:00"

// apiCall performs one admin request, returns an error for any non-2xx status, and redacts the master and token values from the error text.
func apiCall(ctx context.Context, cfg Config, deps Deps, endpoint, method, authToken string, body any, redact []string, allowUnauthorized bool) (apiResult, error) {
	status, text, err := garage.Send(ctx, deps.Garage, cfg.AdminURL, endpoint, method, authToken, body)
	if err != nil {
		return apiResult{}, err
	}
	ok := status >= http.StatusOK && status < http.StatusMultipleChoices
	authorizedRejection := allowUnauthorized && (status == http.StatusUnauthorized || status == http.StatusForbidden)
	if !ok && !authorizedRejection {
		safe := garage.ErrorBody(string(text), cfg.MasterToken)
		for _, token := range redact {
			safe = garage.ErrorBody(safe, token)
		}
		return apiResult{}, fmt.Errorf("%s returned HTTP %d: %s", endpoint, status, safe)
	}
	if !ok || len(text) == 0 {
		return apiResult{status: status}, nil
	}
	value, err := garage.DecodeJSON(endpoint, text)
	if err != nil {
		return apiResult{}, err
	}
	return apiResult{status: status, body: value}, nil
}

// parseTokenInfo validates a GetCurrentAdminTokenInfo or CreateAdminToken body.
func parseTokenInfo(value any, endpoint string) (tokenInfo, error) {
	record, ok := garage.Record(value)
	if !ok {
		return tokenInfo{}, fmt.Errorf("%s returned an invalid response", endpoint)
	}
	id, idOK := optionalString(record, "id")
	_, nameOK := record["name"].(string)
	expired, expiredOK := record["expired"].(bool)
	scope, scopeOK := garage.StringArray(record["scope"])
	expiration, expirationOK := optionalDate(record, "expiration")
	if !idOK || !nameOK || !expiredOK || !scopeOK || !expirationOK {
		return tokenInfo{}, fmt.Errorf("%s returned an invalid response", endpoint)
	}
	return tokenInfo{id: id, expired: expired, scope: scope, expiration: expiration}, nil
}

// optionalString accepts an absent, null, or string field and reports whether the field is valid.
func optionalString(record map[string]any, key string) (*string, bool) {
	raw, present := record[key]
	if !present || raw == nil {
		return nil, true
	}
	s, ok := raw.(string)
	if !ok {
		return nil, false
	}
	return &s, true
}

// optionalDate accepts an absent or null value, or an RFC 3339 string. time.Parse is stricter than JavaScript
// Date.parse, which also accepts other forms. Garage returns RFC 3339, so the stricter parse is not hit in practice.
func optionalDate(record map[string]any, key string) (*time.Time, bool) {
	raw, present := record[key]
	if !present || raw == nil {
		return nil, true
	}
	s, ok := raw.(string)
	if !ok {
		return nil, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return nil, false
	}
	return &t, true
}

// parseMintedToken validates a CreateAdminToken response and returns the new token.
func parseMintedToken(value any) (minted, error) {
	info, err := parseTokenInfo(value, "CreateAdminToken")
	if err != nil {
		return minted{}, err
	}
	record, _ := garage.Record(value)
	secretToken, ok := record["secretToken"].(string)
	if !ok || secretToken == "" {
		return minted{}, errors.New("CreateAdminToken returned an invalid response")
	}
	if info.id == nil {
		return minted{}, errors.New("CreateAdminToken did not return a deletable token ID")
	}
	return minted{id: *info.id, secretToken: secretToken}, nil
}

// parseAdminTokens validates a ListAdminTokens body and returns named tokens.
func parseAdminTokens(value any) ([]namedToken, error) {
	items, ok := value.([]any)
	if !ok {
		return nil, errors.New("ListAdminTokens returned an invalid response")
	}
	out := make([]namedToken, 0, len(items))
	for _, item := range items {
		record, ok := garage.Record(item)
		if !ok {
			return nil, errors.New("ListAdminTokens returned an invalid response")
		}
		id, idOK := optionalString(record, "id")
		name, nameOK := record["name"].(string)
		if !idOK || !nameOK {
			return nil, errors.New("ListAdminTokens returned an invalid response")
		}
		out = append(out, namedToken{id: id, name: name})
	}
	return out, nil
}

// hasWantedScope reports whether scope has exactly the wanted entries, with no extras.
func hasWantedScope(scope, wanted []string) bool {
	if len(scope) != len(wanted) {
		return false
	}
	for _, entry := range wanted {
		if !slices.Contains(scope, entry) {
			return false
		}
	}
	return true
}

// tokenIsCurrent reports whether the stored token still works with the right scope and enough lifetime.
// Garage 401 or 403 means the token is rejected, so it is not current.
func tokenIsCurrent(ctx context.Context, cfg Config, deps Deps, token string) (bool, error) {
	result, err := apiCall(ctx, cfg, deps, "GetCurrentAdminTokenInfo", http.MethodGet, token, nil, []string{token}, true)
	if err != nil {
		return false, err
	}
	if result.status == http.StatusUnauthorized || result.status == http.StatusForbidden {
		return false, nil
	}
	info, err := parseTokenInfo(result.body, "GetCurrentAdminTokenInfo")
	if err != nil {
		return false, err
	}
	return !info.expired &&
		hasWantedScope(info.scope, cfg.Scope) &&
		info.expiration != nil &&
		info.expiration.After(deps.Garage.Now().Add(renewBefore(cfg))), nil
}

// renewBefore returns the configured renewal window, or RenewBefore when unset.
func renewBefore(cfg Config) time.Duration {
	if cfg.RenewBefore == 0 {
		return RenewBefore
	}
	return cfg.RenewBefore
}

// ttl returns the configured token lifetime, or TTL when unset.
func ttl(cfg Config) time.Duration {
	if cfg.TTL == 0 {
		return TTL
	}
	return cfg.TTL
}

// createToken mints a token with the master token and returns its ID and secret value.
func createToken(ctx context.Context, cfg Config, deps Deps) (minted, error) {
	result, err := apiCall(ctx, cfg, deps, "CreateAdminToken", http.MethodPost, cfg.MasterToken, map[string]any{
		"name":       cfg.TokenName,
		"scope":      cfg.Scope,
		"expiration": deps.Garage.Now().Add(ttl(cfg)).UTC().Format(isoMillis),
	}, nil, false)
	if err != nil {
		return minted{}, err
	}
	return parseMintedToken(result.body)
}

// publishToken writes the new token into the Secret. On a 409 it deletes this run's token and reports false.
func publishToken(ctx context.Context, cfg Config, deps Deps, current map[string]any, token minted) (bool, error) {
	metadata, ok := garage.Record(current["metadata"])
	if !ok {
		return false, errors.New("Kubernetes Secret response did not include metadata") //nolint:staticcheck // ST1005: user-facing message, text is fixed.
	}
	if _, ok := metadata["resourceVersion"].(string); !ok {
		return false, errors.New("Kubernetes Secret response did not include resourceVersion") //nolint:staticcheck // ST1005: user-facing message, text is fixed.
	}
	data, ok := garage.Record(current["data"])
	if !ok {
		data = map[string]any{}
	}
	// Copy each level so the replacement shares no maps with current.
	replacement := maps.Clone(current)
	replacement["metadata"] = maps.Clone(metadata)
	copiedData := maps.Clone(data)
	copiedData["token"] = base64.StdEncoding.EncodeToString([]byte(token.secretToken))
	replacement["data"] = copiedData

	err := deps.Secrets.Replace(ctx, cfg.Namespace, cfg.SecretName, replacement)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, ErrSecretConflict) {
		return false, err
	}
	if _, err := apiCall(ctx, cfg, deps, "DeleteAdminToken?id="+jscompat.EncodeURIComponent(token.id), http.MethodPost, cfg.MasterToken, nil, []string{token.secretToken}, false); err != nil {
		return false, err
	}
	deps.Garage.Log("Another mint run published first; removed this run's token")
	return false, nil
}

// deleteOlderTokens removes every other token with this run's name.
func deleteOlderTokens(ctx context.Context, cfg Config, deps Deps, token minted) error {
	listed, err := apiCall(ctx, cfg, deps, "ListAdminTokens", http.MethodGet, cfg.MasterToken, nil, []string{token.secretToken}, false)
	if err != nil {
		return err
	}
	tokens, err := parseAdminTokens(listed.body)
	if err != nil {
		return err
	}
	for _, candidate := range tokens {
		if candidate.name != cfg.TokenName || candidate.id == nil || *candidate.id == token.id {
			continue
		}
		if _, err := apiCall(ctx, cfg, deps, "DeleteAdminToken?id="+jscompat.EncodeURIComponent(*candidate.id), http.MethodPost, cfg.MasterToken, nil, []string{token.secretToken}, false); err != nil {
			return err
		}
	}
	return nil
}
