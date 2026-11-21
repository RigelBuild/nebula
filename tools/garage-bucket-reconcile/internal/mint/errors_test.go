package mint

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/RigelBuild/nebula/tools/garage-bucket-reconcile/internal/fakeadmin"
)

// TestAdminErrorBodyDoesNotLeakTokenPrefixAcrossTruncation makes ListAdminTokens fail with a body that carries
// the minted secret after 190 filler characters. The secret must be redacted before the body is cut to 200 units.
func TestAdminErrorBodyDoesNotLeakTokenPrefixAcrossTruncation(t *testing.T) {
	t.Parallel()
	const minted = "new-id.new-secret-value-0123456789"
	body := strings.Repeat("x", 190) + minted
	secrets := newFakeSecret(emptySecret())
	d := fakeadmin.NewDoer(func(req fakeadmin.Request) (fakeadmin.Reply, error) {
		if r, ok := healthReady(req); ok {
			return r, nil
		}
		if r, ok, err := createdTokenReply(req, "new-id", minted); ok {
			return r, err
		}
		if apiEndpoint(req) == "ListAdminTokens" {
			return fakeadmin.Reply{Status: http.StatusInternalServerError, Body: body}, nil
		}
		return fakeadmin.Reply{}, errors.New("unexpected request " + req.URL)
	})
	_, err := Run(context.Background(), testConfig(), testDeps(d, fixedClock(), secrets))
	if err == nil {
		t.Fatal("Run succeeded, want ListAdminTokens error")
	}
	for n := 4; n <= len(minted); n++ {
		if strings.Contains(err.Error(), minted[:n]) {
			t.Fatalf("error contains a %d-character prefix of the token: %q", n, err.Error())
		}
	}
}
