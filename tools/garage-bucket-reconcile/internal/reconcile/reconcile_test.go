package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/RigelBuild/nebula/tools/garage-bucket-reconcile/internal/fakeadmin"
	"github.com/RigelBuild/nebula/tools/garage-bucket-reconcile/internal/garage"
)

const (
	adminURL  = "http://garage.example.com:3903"
	adminTok  = "secret-admin-token"
	accessKey = "S3-access-key"
	bucket    = "example-bucket"
	maxSize   = int64(5_368_709_120)
)

func testConfig() Config {
	return Config{
		AdminURL:    adminURL,
		AdminToken:  adminTok,
		AccessKeyID: accessKey,
		BucketName:  bucket,
		MaxSize:     maxSize,
	}
}

// bucketRecord is a ListBuckets or CreateBucket record for id, carrying the test bucket alias.
func bucketRecord(id string) map[string]any {
	return map[string]any{"id": id, "globalAliases": []any{bucket}}
}

func quotaResponse(size int64) map[string]any {
	return map[string]any{"quotas": map[string]any{"maxSize": size}}
}

func grantResponse() map[string]any {
	return map[string]any{
		"keys": []any{
			map[string]any{
				"accessKeyId": accessKey,
				"permissions": map[string]any{"read": true, "write": true, "owner": false},
			},
		},
	}
}

// completeReplies answers every endpoint the happy path uses. An unknown URL is a fake error, not a panic.
func completeReplies(req fakeadmin.Request) (fakeadmin.Reply, error) {
	if r, ok := health(req.URL); ok {
		return r, nil
	}
	switch req.URL {
	case adminURL + "/v2/ListBuckets":
		return fakeadmin.Reply{Body: []any{}}, nil
	case adminURL + "/v2/CreateBucket":
		return fakeadmin.Reply{Body: bucketRecord("bucket-123")}, nil
	case adminURL + "/v2/GetKeyInfo?id=" + accessKey:
		return fakeadmin.Reply{Body: map[string]any{"accessKeyId": accessKey}}, nil
	case adminURL + "/v2/UpdateBucket?id=bucket-123":
		return fakeadmin.Reply{Body: quotaResponse(maxSize)}, nil
	case adminURL + "/v2/AllowBucketKey":
		return fakeadmin.Reply{Body: grantResponse()}, nil
	}
	return fakeadmin.Reply{}, errors.New("unexpected request " + req.URL)
}

// health answers /health with 200 for the readiness probe.
func health(url string) (fakeadmin.Reply, bool) {
	if url == adminURL+"/health" {
		return fakeadmin.Reply{Body: "ready"}, true
	}
	return fakeadmin.Reply{}, false
}

// deps wires the fake Doer and clock into garage.Deps.
func deps(d *fakeadmin.Doer, clock *fakeadmin.Clock) garage.Deps {
	return garage.Deps{
		HTTP:  d.Client(),
		Sleep: clock.Sleep,
		Now:   clock.Now,
		Log:   func(string) {},
	}
}

// requestURLs returns the URLs of the admin API calls (those under /v2/), in order.
func apiRequests(reqs []fakeadmin.Request) []fakeadmin.Request {
	var out []fakeadmin.Request
	for _, r := range reqs {
		if strings.Contains(r.URL, "/v2/") {
			out = append(out, r)
		}
	}
	return out
}

func TestCreatesMissingBucketVerifiesQuotaThenGrantsKey(t *testing.T) {
	t.Parallel()
	d := fakeadmin.NewDoer(completeReplies)
	clock := fakeadmin.NewClock(time.Unix(0, 0))
	if err := Run(context.Background(), testConfig(), deps(d, clock)); err != nil {
		t.Fatalf("Run: %v", err)
	}

	api := apiRequests(d.Requests())
	want := [][2]string{
		{"GET", adminURL + "/v2/ListBuckets"},
		{"POST", adminURL + "/v2/CreateBucket"},
		{"GET", adminURL + "/v2/GetKeyInfo?id=" + accessKey},
		{"POST", adminURL + "/v2/UpdateBucket?id=bucket-123"},
		{"POST", adminURL + "/v2/AllowBucketKey"},
	}
	if len(api) != len(want) {
		t.Fatalf("api calls = %d, want %d", len(api), len(want))
	}
	for i, w := range want {
		if api[i].Method != w[0] || api[i].URL != w[1] {
			t.Errorf("call %d = %s %s, want %s %s", i, api[i].Method, api[i].URL, w[0], w[1])
		}
	}
	if !jsonEqual(api[1].Body, map[string]any{"globalAlias": bucket}) {
		t.Errorf("CreateBucket body = %v", api[1].Body)
	}
	if !jsonEqual(api[3].Body, map[string]any{"quotas": map[string]any{"maxSize": float64(maxSize), "maxObjects": nil}}) {
		t.Errorf("UpdateBucket body = %v", api[3].Body)
	}
	if !jsonEqual(api[4].Body, map[string]any{
		"bucketId":    "bucket-123",
		"accessKeyId": accessKey,
		"permissions": map[string]any{"read": true, "write": true, "owner": false},
	}) {
		t.Errorf("AllowBucketKey body = %v", api[4].Body)
	}
	for _, r := range api {
		if r.Authorization != "Bearer "+adminTok {
			t.Errorf("Authorization = %q for %s", r.Authorization, r.URL)
		}
	}
}

func TestUsesExistingBucketWithoutCreatingIt(t *testing.T) {
	t.Parallel()
	d := fakeadmin.NewDoer(func(req fakeadmin.Request) (fakeadmin.Reply, error) {
		if r, ok := health(req.URL); ok {
			return r, nil
		}
		switch req.URL {
		case adminURL + "/v2/ListBuckets":
			return fakeadmin.Reply{Body: []any{bucketRecord("existing-bucket")}}, nil
		case adminURL + "/v2/GetKeyInfo?id=" + accessKey:
			return fakeadmin.Reply{Body: map[string]any{"accessKeyId": accessKey}}, nil
		case adminURL + "/v2/UpdateBucket?id=existing-bucket":
			return fakeadmin.Reply{Body: quotaResponse(maxSize)}, nil
		case adminURL + "/v2/AllowBucketKey":
			return fakeadmin.Reply{Body: grantResponse()}, nil
		}
		return fakeadmin.Reply{}, errors.New("unexpected request " + req.URL)
	})
	clock := fakeadmin.NewClock(time.Unix(0, 0))
	if err := Run(context.Background(), testConfig(), deps(d, clock)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	reqs := d.Requests()
	for _, r := range reqs {
		if strings.HasSuffix(r.URL, "/CreateBucket") {
			t.Error("CreateBucket was called for an existing bucket")
		}
	}
	found := false
	for _, r := range reqs {
		if strings.HasSuffix(r.URL, "id=existing-bucket") {
			found = true
		}
	}
	if !found {
		t.Error("no request targeted id=existing-bucket")
	}
}

func TestDoesNotGrantKeyWhenQuotaNotConfirmed(t *testing.T) {
	t.Parallel()
	d := fakeadmin.NewDoer(func(req fakeadmin.Request) (fakeadmin.Reply, error) {
		if r, ok := health(req.URL); ok {
			return r, nil
		}
		switch req.URL {
		case adminURL + "/v2/ListBuckets":
			return fakeadmin.Reply{Body: []any{bucketRecord("bucket-123")}}, nil
		case adminURL + "/v2/GetKeyInfo?id=" + accessKey:
			return fakeadmin.Reply{Body: map[string]any{"accessKeyId": accessKey}}, nil
		case adminURL + "/v2/UpdateBucket?id=bucket-123":
			return fakeadmin.Reply{Body: quotaResponse(maxSize - 1)}, nil
		}
		return fakeadmin.Reply{}, errors.New("unexpected request " + req.URL)
	})
	clock := fakeadmin.NewClock(time.Unix(0, 0))
	err := Run(context.Background(), testConfig(), deps(d, clock))
	if err == nil || !strings.Contains(err.Error(), "UpdateBucket response did not confirm") {
		t.Fatalf("err = %v, want UpdateBucket response did not confirm", err)
	}
	for _, r := range d.Requests() {
		if strings.HasSuffix(r.URL, "/AllowBucketKey") {
			t.Error("AllowBucketKey was called after an unconfirmed quota")
		}
	}
}

func TestRejectsGrantWithOwnerPermission(t *testing.T) {
	t.Parallel()
	d := fakeadmin.NewDoer(func(req fakeadmin.Request) (fakeadmin.Reply, error) {
		if req.URL == adminURL+"/v2/AllowBucketKey" {
			return fakeadmin.Reply{Body: map[string]any{
				"keys": []any{map[string]any{
					"accessKeyId": accessKey,
					"permissions": map[string]any{"read": true, "write": true, "owner": true},
				}},
			}}, nil
		}
		return completeReplies(req)
	})
	clock := fakeadmin.NewClock(time.Unix(0, 0))
	err := Run(context.Background(), testConfig(), deps(d, clock))
	if err == nil || !strings.Contains(err.Error(), "AllowBucketKey response did not confirm") {
		t.Fatalf("err = %v, want AllowBucketKey response did not confirm", err)
	}
	reqs := d.Requests()
	if last := reqs[len(reqs)-1].URL; last != adminURL+"/v2/AllowBucketKey" {
		t.Errorf("last request = %s, want AllowBucketKey", last)
	}
}

func TestRejectsMismatchedAccessKeyBeforeChangingBucket(t *testing.T) {
	t.Parallel()
	d := fakeadmin.NewDoer(func(req fakeadmin.Request) (fakeadmin.Reply, error) {
		if r, ok := health(req.URL); ok {
			return r, nil
		}
		switch req.URL {
		case adminURL + "/v2/ListBuckets":
			return fakeadmin.Reply{Body: []any{bucketRecord("bucket-123")}}, nil
		case adminURL + "/v2/GetKeyInfo?id=" + accessKey:
			return fakeadmin.Reply{Body: map[string]any{"accessKeyId": "some-other-key"}}, nil
		}
		return fakeadmin.Reply{}, errors.New("unexpected request " + req.URL)
	})
	clock := fakeadmin.NewClock(time.Unix(0, 0))
	err := Run(context.Background(), testConfig(), deps(d, clock))
	if err == nil || !strings.Contains(err.Error(), "GetKeyInfo response accessKeyId did not match") {
		t.Fatalf("err = %v, want GetKeyInfo response accessKeyId did not match", err)
	}
	for _, r := range d.Requests() {
		if strings.Contains(r.URL, "UpdateBucket") {
			t.Error("UpdateBucket was called after a mismatched access key")
		}
	}
}

func TestReportsFailedCreateBucketWithoutExposingToken(t *testing.T) {
	t.Parallel()
	d := fakeadmin.NewDoer(func(req fakeadmin.Request) (fakeadmin.Reply, error) {
		if r, ok := health(req.URL); ok {
			return r, nil
		}
		switch req.URL {
		case adminURL + "/v2/ListBuckets":
			return fakeadmin.Reply{Body: []any{}}, nil
		case adminURL + "/v2/CreateBucket":
			return fakeadmin.Reply{Status: 503, Body: "failure " + adminTok}, nil
		}
		return fakeadmin.Reply{}, errors.New("unexpected request " + req.URL)
	})
	clock := fakeadmin.NewClock(time.Unix(0, 0))
	err := Run(context.Background(), testConfig(), deps(d, clock))
	pattern := regexp.MustCompile(`CreateBucket returned HTTP 503.*\[redacted\]`)
	if err == nil || !pattern.MatchString(err.Error()) {
		t.Fatalf("err = %v, want CreateBucket HTTP 503 with [redacted]", err)
	}
	for _, r := range d.Requests() {
		if strings.Contains(r.URL, "secret-admin-token") {
			t.Errorf("token leaked into request URL %s", r.URL)
		}
	}
}

func TestFailsAtReadinessDeadlineWithoutListingBuckets(t *testing.T) {
	t.Parallel()
	d := fakeadmin.NewDoer(func(fakeadmin.Request) (fakeadmin.Reply, error) {
		return fakeadmin.Reply{Status: 503}, nil
	})
	clock := fakeadmin.NewClock(time.Unix(0, 0))
	cfg := testConfig()
	cfg.ReadyWait = 5 * time.Second
	err := Run(context.Background(), cfg, deps(d, clock))
	if err == nil || !strings.Contains(err.Error(), "Garage admin API was not ready within 5000ms") {
		t.Fatalf("err = %v, want readiness deadline", err)
	}
	sleeps := clock.Sleeps()
	want := []time.Duration{2 * time.Second, 2 * time.Second, 1 * time.Second}
	if !durationsEqual(sleeps, want) {
		t.Errorf("sleeps = %v, want %v", sleeps, want)
	}
	for _, r := range d.Requests() {
		if strings.Contains(r.URL, "/v2/ListBuckets") {
			t.Error("ListBuckets was called before readiness")
		}
	}
}

func TestWaitsThroughHealth503AndContinuesAfter200(t *testing.T) {
	t.Parallel()
	healthCalls := 0
	d := fakeadmin.NewDoer(func(req fakeadmin.Request) (fakeadmin.Reply, error) {
		if req.URL == adminURL+"/health" {
			healthCalls++
			if healthCalls == 1 {
				return fakeadmin.Reply{Status: 503}, nil
			}
			return fakeadmin.Reply{Status: 200}, nil
		}
		return completeReplies(req)
	})
	clock := fakeadmin.NewClock(time.Unix(0, 0))
	if err := Run(context.Background(), testConfig(), deps(d, clock)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if healthCalls != 2 {
		t.Errorf("healthCalls = %d, want 2", healthCalls)
	}
	found := false
	for _, r := range d.Requests() {
		if strings.HasSuffix(r.URL, "/AllowBucketKey") {
			found = true
		}
	}
	if !found {
		t.Error("AllowBucketKey was not called after readiness")
	}
}

// jsonEqual compares two decoded JSON values, treating numbers as float64.
func jsonEqual(got, want any) bool {
	return fmtJSON(got) == fmtJSON(want)
}

// durationsEqual reports whether a and b hold the same durations in order.
func durationsEqual(a, b []time.Duration) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// fmtJSON canonicalises a value through encoding/json so map ordering and numeric types compare equal.
func fmtJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return "marshal error: " + err.Error()
	}
	return string(raw)
}
