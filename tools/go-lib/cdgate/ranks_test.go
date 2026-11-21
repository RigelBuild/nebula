package cdgate

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const (
	shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	shaC = "cccccccccccccccccccccccccccccccccccccccc"
)

func TestFirstParentRanksOrdersHeadFirst(t *testing.T) {
	ranks, err := FirstParentRanks(shaA + "\n" + shaB + "\n" + shaC + "\n")
	if err != nil {
		t.Fatalf("FirstParentRanks: %v", err)
	}
	if ranks[shaA] != 0 || ranks[shaB] != 1 || ranks[shaC] != 2 {
		t.Fatalf("ranks = %v, want head 0, then 1, then 2", ranks)
	}
}

func TestFirstParentRanksRejectsEmptyHistory(t *testing.T) {
	if _, err := FirstParentRanks("  \n"); err == nil || err.Error() != "origin/main history is empty" {
		t.Fatalf("err = %v, want the empty-history error", err)
	}
}

func TestFirstParentRanksRejectsMalformedSHA(t *testing.T) {
	for _, bad := range []string{"not-a-sha", "abc123", strings.Repeat("g", 40)} {
		if _, err := FirstParentRanks(bad); err == nil || err.Error() != "invalid origin/main history" {
			t.Fatalf("FirstParentRanks(%q) err = %v, want the invalid-history error", bad, err)
		}
	}
}

func TestFirstParentRanksRejectsDuplicateSHA(t *testing.T) {
	if _, err := FirstParentRanks(shaA + "\n" + shaA + "\n"); err == nil || err.Error() != "invalid origin/main history" {
		t.Fatalf("err = %v, want the invalid-history error for a repeated commit", err)
	}
}

// scriptedGit answers each git call in order. Unexpected calls fail the test.
type scriptedGit struct {
	t     *testing.T
	calls []string
	out   map[string]string
	err   map[string]error
}

func (g *scriptedGit) run(_ context.Context, args ...string) (string, error) {
	key := strings.Join(args, " ")
	g.calls = append(g.calls, key)
	if err, ok := g.err[key]; ok {
		return "", err
	}
	out, ok := g.out[key]
	if !ok {
		g.t.Fatalf("unexpected git call %q", key)
	}
	return out, nil
}

func TestMainRanksFullCloneFetchesWithoutUnshallow(t *testing.T) {
	g := &scriptedGit{t: t, out: map[string]string{
		"rev-parse --is-shallow-repository":    "false\n",
		"fetch --filter=blob:none origin main": "",
		"rev-list --first-parent origin/main":  shaA + "\n" + shaB + "\n",
	}}
	ranks, err := MainRanks(context.Background(), g.run)
	if err != nil {
		t.Fatalf("MainRanks: %v", err)
	}
	if ranks[shaA] != 0 || ranks[shaB] != 1 {
		t.Fatalf("ranks = %v", ranks)
	}
	for _, c := range g.calls {
		if strings.Contains(c, "--unshallow") {
			t.Fatalf("a full clone must not unshallow: calls = %v", g.calls)
		}
	}
}

func TestMainRanksShallowCloneUnshallowsWithBlobFilter(t *testing.T) {
	g := &scriptedGit{t: t, out: map[string]string{
		"rev-parse --is-shallow-repository":                "true\n",
		"fetch --unshallow --filter=blob:none origin main": "",
		"rev-list --first-parent origin/main":              shaA + "\n",
	}}
	if _, err := MainRanks(context.Background(), g.run); err != nil {
		t.Fatalf("MainRanks: %v", err)
	}
	found := false
	for _, c := range g.calls {
		if c == "fetch --unshallow --filter=blob:none origin main" {
			found = true
		}
	}
	if !found {
		t.Fatalf("a shallow clone must unshallow with blob:none: calls = %v", g.calls)
	}
}

func TestMainRanksRejectsUnknownShallowStatus(t *testing.T) {
	g := &scriptedGit{t: t, out: map[string]string{
		"rev-parse --is-shallow-repository": "maybe\n",
	}}
	if _, err := MainRanks(context.Background(), g.run); err == nil || err.Error() != "invalid shallow repository status" {
		t.Fatalf("err = %v, want the invalid-status error", err)
	}
}

func TestMainRanksPropagatesGitFailure(t *testing.T) {
	boom := errors.New("git: no remote")
	g := &scriptedGit{t: t,
		out: map[string]string{"rev-parse --is-shallow-repository": "false\n"},
		err: map[string]error{"fetch --filter=blob:none origin main": boom},
	}
	if _, err := MainRanks(context.Background(), g.run); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the git failure surfaced", err)
	}
}
