package mint

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/RigelBuild/nebula/tools/garage-bucket-reconcile/internal/fakeadmin"
)

// failingSecret fails every Replace with a non-conflict error, as an API outage would.
type failingSecret struct{ inner *fakeSecret }

func (f failingSecret) Read(ctx context.Context, ns, name string) (map[string]any, error) {
	return f.inner.Read(ctx, ns, name)
}

func (failingSecret) Replace(context.Context, string, string, map[string]any) error {
	return errors.New("secret api unavailable")
}

// TestNonConflictReplaceFailureDeletesThisRunsToken checks that a Replace error other than conflict
// deletes the token this run just minted, so it does not stay valid for 30 days, and returns both errors.
func TestNonConflictReplaceFailureDeletesThisRunsToken(t *testing.T) {
	t.Parallel()
	secrets := failingSecret{inner: newFakeSecret(emptySecret())}
	d := fakeadmin.NewDoer(func(req fakeadmin.Request) (fakeadmin.Reply, error) {
		if r, ok := healthReady(req); ok {
			return r, nil
		}
		if r, ok, err := createdTokenReply(req, "orphan-id", "orphan-secret"); ok {
			return r, err
		}
		if apiEndpoint(req) == "DeleteAdminToken" {
			return fakeadmin.Reply{}, nil
		}
		return fakeadmin.Reply{}, errors.New("unexpected request " + req.URL)
	})
	_, err := Run(context.Background(), testConfig(), testDeps(d, fixedClock(), secrets))
	if err == nil || !strings.Contains(err.Error(), "secret api unavailable") {
		t.Fatalf("Run err = %v, want the Replace error", err)
	}
	var deleted []string
	for _, r := range d.Requests() {
		if apiEndpoint(r) == "DeleteAdminToken" {
			deleted = append(deleted, deleteID(r))
		}
	}
	if !slices.Equal(deleted, []string{"orphan-id"}) {
		t.Errorf("deleted = %v, want [orphan-id]", deleted)
	}
}

// writeThenFailSecret stores each Replace, then returns an error, as a 504 after the API committed would.
type writeThenFailSecret struct{ inner *fakeSecret }

func (w writeThenFailSecret) Read(ctx context.Context, ns, name string) (map[string]any, error) {
	return w.inner.Read(ctx, ns, name)
}

func (w writeThenFailSecret) Replace(ctx context.Context, ns, name string, body map[string]any) error {
	if err := w.inner.Replace(ctx, ns, name, body); err != nil {
		return err
	}
	return errors.New("gateway timeout after commit")
}

// TestReplaceErrorAfterCommitKeepsTokenAndSweepsOlderOnes checks that a Replace error does not prove the PUT
// failed. The Secret already holds the new token, so the run must keep it and sweep the older same-name tokens.
func TestReplaceErrorAfterCommitKeepsTokenAndSweepsOlderOnes(t *testing.T) {
	t.Parallel()
	secrets := writeThenFailSecret{inner: newFakeSecret(emptySecret())}
	listed := []map[string]any{tokenRecord(new("old-token-id"), Scope, nil, tokenName)}
	d := fakeadmin.NewDoer(func(req fakeadmin.Request) (fakeadmin.Reply, error) {
		if r, ok := healthReady(req); ok {
			return r, nil
		}
		if r, ok, err := createdTokenReply(req, "new-id", "new-id.new-secret"); ok {
			return r, err
		}
		switch apiEndpoint(req) {
		case "ListAdminTokens":
			return fakeadmin.Reply{Body: listed}, nil
		case "DeleteAdminToken":
			return fakeadmin.Reply{}, nil
		}
		return fakeadmin.Reply{}, errors.New("unexpected request " + req.URL)
	})
	result, err := Run(context.Background(), testConfig(), testDeps(d, fixedClock(), secrets))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result != "minted" {
		t.Errorf("result = %q, want minted", result)
	}
	var deleted []string
	for _, r := range d.Requests() {
		if apiEndpoint(r) == "DeleteAdminToken" {
			deleted = append(deleted, deleteID(r))
		}
	}
	if !slices.Equal(deleted, []string{"old-token-id"}) {
		t.Errorf("deleted = %v, want only [old-token-id]; the new token must be kept", deleted)
	}
}

// TestFailedDeleteAfterReplaceErrorJoinsBothAndRedactsSecret makes the cleanup delete fail with a body that
// carries the new secret. Both errors must be joined, and the secret must appear only as [redacted].
func TestFailedDeleteAfterReplaceErrorJoinsBothAndRedactsSecret(t *testing.T) {
	t.Parallel()
	const minted = "orphan-id.orphan-secret-value"
	secrets := failingSecret{inner: newFakeSecret(emptySecret())}
	d := fakeadmin.NewDoer(func(req fakeadmin.Request) (fakeadmin.Reply, error) {
		if r, ok := healthReady(req); ok {
			return r, nil
		}
		if r, ok, err := createdTokenReply(req, "orphan-id", minted); ok {
			return r, err
		}
		if apiEndpoint(req) == "DeleteAdminToken" {
			return fakeadmin.Reply{Status: http.StatusInternalServerError, Body: "delete failed for " + minted}, nil
		}
		return fakeadmin.Reply{}, errors.New("unexpected request " + req.URL)
	})
	_, err := Run(context.Background(), testConfig(), testDeps(d, fixedClock(), secrets))
	if err == nil {
		t.Fatal("Run succeeded, want joined error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "secret api unavailable") || !strings.Contains(msg, "DeleteAdminToken?id=orphan-id returned HTTP 500") {
		t.Errorf("err = %q, want both the Replace and the delete error", msg)
	}
	if strings.Contains(msg, minted) || strings.Contains(msg, "orphan-secret-value") {
		t.Errorf("error exposed the new secret: %q", msg)
	}
	if !strings.Contains(msg, "[redacted]") {
		t.Errorf("err = %q, want the secret shown as [redacted]", msg)
	}
}
