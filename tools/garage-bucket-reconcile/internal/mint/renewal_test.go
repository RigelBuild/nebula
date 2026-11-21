package mint

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/RigelBuild/nebula/tools/garage-bucket-reconcile/internal/fakeadmin"
)

func TestKeepsValidTokenWithExactScopeAndFarExpiry(t *testing.T) {
	t.Parallel()
	secrets := newFakeSecret(secret("existing-token-id.existing-secret", "17"))
	d := fakeadmin.NewDoer(func(req fakeadmin.Request) (fakeadmin.Reply, error) {
		if r, ok := healthReady(req); ok {
			return r, nil
		}
		if apiEndpoint(req) == "GetCurrentAdminTokenInfo" {
			if req.Authorization != "Bearer existing-token-id.existing-secret" {
				return fakeadmin.Reply{}, errors.New("wrong authorization " + req.Authorization)
			}
			reversed := slices.Clone(Scope)
			slices.Reverse(reversed)
			return fakeadmin.Reply{Body: tokenRecord(new("existing-token-id"), reversed, new(isoAfter(TTL)), tokenName)}, nil
		}
		return fakeadmin.Reply{}, errors.New("unexpected request " + req.URL)
	})
	result, err := Run(context.Background(), testConfig(), testDeps(d, fixedClock(), secrets))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result != "kept" {
		t.Errorf("result = %q, want kept", result)
	}
	if got := endpointsOf(d.Requests()); !slices.Equal(got, []string{"health", "GetCurrentAdminTokenInfo"}) {
		t.Errorf("endpoints = %v", got)
	}
	if n := len(secrets.Writes()); n != 0 {
		t.Errorf("writes = %d, want 0", n)
	}
}

// renewalResponder answers a stored token whose info has the given scope and expiration. Everything else
// the re-mint path needs (health, CreateAdminToken, ListAdminTokens) is answered in order.
func renewalResponder(scope []string, expiration string) func(fakeadmin.Request) (fakeadmin.Reply, error) {
	return func(req fakeadmin.Request) (fakeadmin.Reply, error) {
		if r, ok := healthReady(req); ok {
			return r, nil
		}
		if apiEndpoint(req) == "GetCurrentAdminTokenInfo" {
			return fakeadmin.Reply{Body: tokenRecord(new("old-token-id"), scope, new(expiration), tokenName)}, nil
		}
		if r, ok, err := createdTokenReply(req, "replacement-id", "replacement-id.replacement-secret"); ok {
			return r, err
		}
		if apiEndpoint(req) == "ListAdminTokens" {
			return fakeadmin.Reply{Body: []any{}}, nil
		}
		return fakeadmin.Reply{}, errors.New("unexpected request " + req.URL)
	}
}

// renewalCases are the renewal cases: scope drift, and expiry inside the renewal window.
var renewalCases = []struct {
	label      string
	expiration string
	scope      []string
}{
	{"scope drift", isoAfter(TTL), []string{"ListBuckets"}},
	{"expiry inside the renewal window", isoAfter(RenewBefore), Scope},
}

func TestRemintsWhenStoredTokenHasScopeDriftOrRenewalWindowExpiry(t *testing.T) {
	t.Parallel()
	for _, c := range renewalCases {
		t.Run(c.label, func(t *testing.T) {
			t.Parallel()
			secrets := newFakeSecret(secret("old-token-id.old-token-secret", "17"))
			d := fakeadmin.NewDoer(renewalResponder(c.scope, c.expiration))
			if _, err := Run(context.Background(), testConfig(), testDeps(d, fixedClock(), secrets)); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !containsEndpoint(d.Requests(), "CreateAdminToken") {
				t.Error("CreateAdminToken was not called")
			}
			if n := len(secrets.Writes()); n != 1 {
				t.Errorf("writes = %d, want 1", n)
			}
		})
	}
}

// reject403Responder answers the stored token's info with HTTP 403, as Garage does for a revoked token.
func reject403Responder(req fakeadmin.Request) (fakeadmin.Reply, error) {
	if r, ok := healthReady(req); ok {
		return r, nil
	}
	if apiEndpoint(req) == "GetCurrentAdminTokenInfo" {
		return fakeadmin.Reply{Status: 403, Body: "denied"}, nil
	}
	if r, ok, err := createdTokenReply(req, "replacement-id", "replacement-id.replacement-secret"); ok {
		return r, err
	}
	if apiEndpoint(req) == "ListAdminTokens" {
		return fakeadmin.Reply{Body: []any{}}, nil
	}
	return fakeadmin.Reply{}, errors.New("unexpected request " + req.URL)
}

func TestRemintsAfterGarageRejectsStoredTokenWith403(t *testing.T) {
	t.Parallel()
	secrets := newFakeSecret(secret("rejected-id.rejected-secret", "17"))
	d := fakeadmin.NewDoer(reject403Responder)
	result, err := Run(context.Background(), testConfig(), testDeps(d, fixedClock(), secrets))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result != "minted" {
		t.Errorf("result = %q, want minted", result)
	}
	if !containsEndpoint(d.Requests(), "CreateAdminToken") {
		t.Error("CreateAdminToken was not called")
	}
	if n := len(secrets.Writes()); n != 1 {
		t.Errorf("writes = %d, want 1", n)
	}
}

// inspectionFailure is one way token inspection can fail.
type inspectionFailure struct {
	label        string
	reply        fakeadmin.Reply
	transportErr bool
}

// inspectionFailures are the failure cases: HTTP 500, and a network error during token inspection.
var inspectionFailures = []inspectionFailure{
	{"HTTP 500", fakeadmin.Reply{Status: 500, Body: "unavailable"}, false},
	{"network errors", fakeadmin.Reply{}, true},
}

// inspectionFailResponder answers token inspection with the failure under test and rejects every other call.
// A transport error is returned as an error from the Doer, not as a reply.
func inspectionFailResponder(f inspectionFailure) func(fakeadmin.Request) (fakeadmin.Reply, error) {
	return func(req fakeadmin.Request) (fakeadmin.Reply, error) {
		if r, ok := healthReady(req); ok {
			return r, nil
		}
		if apiEndpoint(req) != "GetCurrentAdminTokenInfo" {
			return fakeadmin.Reply{}, errors.New("unexpected request " + req.URL)
		}
		if f.transportErr {
			return fakeadmin.Reply{}, errors.New("connection reset")
		}
		return f.reply, nil
	}
}

func TestDoesNotMintOrPublishWhenTokenInspectionFails(t *testing.T) {
	t.Parallel()
	for _, f := range inspectionFailures {
		t.Run(f.label, func(t *testing.T) {
			t.Parallel()
			secrets := newFakeSecret(secret("old-token-id.old-token-secret", "17"))
			d := fakeadmin.NewDoer(inspectionFailResponder(f))
			if _, err := Run(context.Background(), testConfig(), testDeps(d, fixedClock(), secrets)); err == nil {
				t.Fatal("Run succeeded, want error")
			}
			if containsEndpoint(d.Requests(), "CreateAdminToken") {
				t.Error("CreateAdminToken was called")
			}
			if containsEndpoint(d.Requests(), "DeleteAdminToken") {
				t.Error("DeleteAdminToken was called")
			}
			if n := len(secrets.Writes()); n != 0 {
				t.Errorf("writes = %d, want 0", n)
			}
		})
	}
}
