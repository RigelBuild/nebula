package mint

import (
	"context"
	"errors"
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
