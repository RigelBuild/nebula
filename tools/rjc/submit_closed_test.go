package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSubmitRefusesClosedLedgerPRBeforeMutation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		discover bool
		at       bool
		merged   bool
	}{
		{name: "explicit bookmark merged", merged: true},
		{name: "discovered target merged", discover: true, merged: true},
		{name: "at revision merged", at: true, merged: true},
		{name: "explicit bookmark closed unmerged"},
		{name: "discovered target closed unmerged", discover: true},
		{name: "at revision closed unmerged", at: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := parallelFixtureRepo(t)
			title := "feat(rjc): refuse a closed PR"
			bookmark := f.new(t, "EX-1", title, "")
			f.commit(t, "closed-pr.txt")
			opt := submitOptions{bookmark: bookmark, title: title, bodyFile: f.bodyFile}
			if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
				t.Fatal(err)
			}
			f.commit(t, "after-close.txt")
			before := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
			setSubmitPRState(t, f, bookmark, "closed", tt.merged)
			beforeOp := jjTest(t, f.repo, "op", "log", "-n", "1", "-T", "id")
			if tt.discover {
				opt.bookmark = ""
			}
			if tt.at {
				opt.at = "@-"
			}

			err := f.repo.submit(f.ctx, opt, f, io.Discard)
			assertRefusalGate(t, err, "pr-closed")
			detail, ok := errors.AsType[*failure](err)
			if !ok {
				t.Fatalf("submit error is not a refusal: %v", err)
			}
			if tt.merged && (!strings.Contains(detail.reason, "merged") || !strings.Contains(detail.fix, "rjc retire "+bookmark) || !strings.Contains(detail.fix, "rjc new --at <tip>")) {
				t.Fatalf("missing merged PR recovery instructions: %v", err)
			}
			if !tt.merged && (!strings.Contains(detail.reason, "closed without merging") || !strings.Contains(detail.fix, "reopen")) {
				t.Fatalf("missing closed-unmerged PR recovery instructions: %v", err)
			}
			if after := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id"); after != before {
				t.Fatalf("bookmark target changed: got %s, want %s", after, before)
			}
			if afterOp := jjTest(t, f.repo, "op", "log", "-n", "1", "-T", "id"); afterOp != beforeOp {
				t.Fatalf("jj operation log advanced: got %s, want %s", afterOp, beforeOp)
			}
			if len(f.pushes) != 1 {
				t.Fatalf("submit reached push after refusal: %v", f.pushes)
			}
		})
	}
}

func TestSubmitAllowsOpenLedgerPR(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	title := "feat(rjc): submit an open PR"
	bookmark := f.new(t, "EX-1", title, "")
	f.commit(t, "open-pr.txt")
	opt := submitOptions{bookmark: bookmark, title: title, bodyFile: f.bodyFile}
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	f.commit(t, "open-pr-update.txt")
	setSubmitPRState(t, f, bookmark, "open", false)
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatalf("open ledger PR refused: %v", err)
	}
	if len(f.pushes) != 2 {
		t.Fatalf("submit pushes = %v, want two submissions", f.pushes)
	}
}

func TestSubmitAllowsOpenPRWhenLedgerPointsToClosedPR(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	title := "feat(rjc): submit with a stale closed ledger PR"
	bookmark := f.new(t, "EX-1", title, "")
	f.commit(t, "stale-ledger.txt")
	opt := submitOptions{bookmark: bookmark, title: title, bodyFile: f.bodyFile}
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	closedPR := f.prs[bookmark]
	if err := (Ledger{root: f.repo.stateDir}).Save(LedgerEntry{Repo: f.repo.profile.Name, PR: closedPR, Bookmark: bookmark, Codename: f.repo.codename}); err != nil {
		t.Fatal(err)
	}
	f.commit(t, "new-tip.txt")
	openPR := closedPR + 1
	openMetadata := f.metadata[closedPR]
	openMetadata.HeadSHA = jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark+"@origin"), "-T", "commit_id")
	f.metadata[openPR] = openMetadata
	f.prs[bookmark] = openPR
	setSubmitOpenPRWithClosedLedger(t, f, bookmark, closedPR, openPR)

	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatalf("submit with an open PR and stale closed ledger entry: %v", err)
	}
	if len(f.pushes) != 2 || f.pushes[1] != bookmark {
		t.Fatalf("pushes = %v, want [%s %s]", f.pushes, bookmark, bookmark)
	}
}

func TestSubmitDoesNotRefuseClosedParentPR(t *testing.T) {
	f := parallelFixtureRepo(t)
	parentTitle := "feat(rjc): parent work to land"
	parent := f.new(t, "EX-1", parentTitle, "")
	f.commit(t, "parent.txt")
	jjTest(t, f.repo, "bookmark", "move", parent, "--to", "@-")
	parentTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(parent), "-T", "commit_id")
	parentOpt := submitOptions{bookmark: parent, title: parentTitle, bodyFile: f.bodyFile}
	if err := f.repo.submit(f.ctx, parentOpt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	childTitle := "feat(rjc): land the child independently"
	child := f.new(t, "EX-2", childTitle, parent)
	f.commit(t, "child.txt")
	jjTest(t, f.repo, "bookmark", "move", child, "--to", "@-")
	f.merged = map[string]bool{parent: true}
	f.mergedHeads = map[string]string{parent: parentTip}
	landAndRestackChild(t, f, parent, parentTip, child, false)
	f.pushes = nil
	setSubmitPRState(t, f, parent, "closed", true)

	if err := f.repo.submit(f.ctx, submitOptions{bookmark: child, title: childTitle, bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatalf("submit child with closed landed parent: %v", err)
	}
	if len(f.pushes) != 1 || f.pushes[0] != child {
		t.Fatalf("pushes = %v, want [%s]", f.pushes, child)
	}
}

func setSubmitOpenPRWithClosedLedger(t *testing.T, f *fixture, bookmark string, closedPR, openPR int) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Path == prPath(f.repo.profile.Name, closedPR) {
			pr := githubMetadata(f.metadata[closedPR])
			pr["state"] = "closed"
			pr["merged_at"] = "2026-11-01T00:00:00Z"
			respondJSON(t, w, pr)
			return
		}
		owner, _, _ := strings.Cut(f.repo.profile.Name, "/")
		if request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/pulls") && request.URL.Query().Get("head") == owner+":"+bookmark {
			switch request.URL.Query().Get("state") {
			case "open":
				respondJSON(t, w, []map[string]any{{"number": openPR, "head": map[string]string{"ref": bookmark}}})
				return
			case "closed":
				respondJSON(t, w, []map[string]any{{"number": closedPR, "head": map[string]string{"ref": bookmark, "sha": f.metadata[closedPR].HeadSHA}, "merged_at": "2026-11-01T00:00:00Z"}})
				return
			}
		}
		f.serve(t, w, request)
	}))
	t.Cleanup(server.Close)
	s, err := service(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.forge = &forgeClient{client: server.Client(), baseURL: server.URL}
}

func setSubmitPRState(t *testing.T, f *fixture, bookmark, state string, merged bool) {
	t.Helper()
	number := f.prs[bookmark]
	metadata := f.metadata[number]
	metadata.HeadRef = bookmark
	f.metadata[number] = metadata
	if f.merged == nil {
		f.merged = map[string]bool{}
	}
	f.merged[bookmark] = merged
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Path == prPath(f.repo.profile.Name, number) {
			pr := githubMetadata(f.metadata[number])
			pr["state"] = state
			if state == "closed" && merged {
				pr["merged_at"] = "2026-11-01T00:00:00Z"
			}
			respondJSON(t, w, pr)
			return
		}
		owner, _, _ := strings.Cut(f.repo.profile.Name, "/")
		if request.Method != http.MethodGet || !strings.HasSuffix(request.URL.Path, "/pulls") || request.URL.Query().Get("head") != owner+":"+bookmark {
			f.serve(t, w, request)
			return
		}
		switch request.URL.Query().Get("state") {
		case "open":
			pulls := []map[string]any{}
			if state == "open" {
				pulls = append(pulls, map[string]any{"number": number, "head": map[string]string{"ref": bookmark}})
			}
			respondJSON(t, w, pulls)
		case "closed":
			pulls := []map[string]any{}
			if state == "closed" {
				sha := metadata.HeadSHA
				if mergedHead := f.mergedHeads[bookmark]; mergedHead != "" {
					sha = mergedHead
				}
				closed := map[string]any{"number": number, "head": map[string]string{"ref": bookmark, "sha": sha}}
				if merged {
					closed["merged_at"] = "2026-11-01T00:00:00Z"
				}
				pulls = append(pulls, closed)
			}
			respondJSON(t, w, pulls)
		default:
			f.serve(t, w, request)
		}
	}))
	t.Cleanup(server.Close)
	s, err := service(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.forge = &forgeClient{client: server.Client(), baseURL: server.URL}
}
