package main

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRetireDeletesOnlySelectedBookmark(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	b := f.new(t, "EX-1", "feat(rjc): retirement", "")
	jjTest(t, f.repo, "git", "push", "--bookmark", b)
	entry := LedgerEntry{Repo: f.repo.profile.Name, PR: 1, Bookmark: b, Codename: f.repo.codename}
	if err := (Ledger{root: f.repo.stateDir}).Save(entry); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{".published.md", ".candidate.md", ".metadata.json", ".parent.json", ".upstream.json"} {
		path, err := (fileBaseline{root: f.repo.stateDir, keys: f.repo.keys}).path(b, suffix)
		if err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, path, "state")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == prPath(f.repo.profile.Name, 1) {
			respondJSON(t, w, map[string]any{"state": "closed", "head": map[string]string{"ref": b}})
			return
		}
		w.WriteHeader(http.StatusNotFound)
		respondJSON(t, w, map[string]string{"message": "Branch not found"})
	}))
	defer server.Close()
	s, _ := service(f.ctx)
	s.forge = &forgeClient{client: server.Client(), baseURL: server.URL}
	var plan bytes.Buffer
	if err := f.repo.retire(f.ctx, retireOptions{bookmark: b}, &plan); err != nil {
		t.Fatalf("%v: %s", err, plan.String())
	}
	if _, found, err := (Ledger{root: f.repo.stateDir}).Find(entry.Repo, b); err != nil || found {
		t.Fatalf("ledger found=%v err=%v", found, err)
	}
	for _, suffix := range []string{".published.md", ".candidate.md", ".metadata.json", ".parent.json", ".upstream.json"} {
		path, err := (fileBaseline{root: f.repo.stateDir, keys: f.repo.keys}).path(b, suffix)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("state remains: %s: %v", path, err)
		}
	}
}

type retirePlanWriter func([]byte) (int, error)

func (f retirePlanWriter) Write(p []byte) (int, error) { return f(p) }

func retireFixture(t *testing.T, push bool) (*fixture, string, LedgerEntry) {
	t.Helper()
	f := fixtureRepo(t)
	b := f.new(t, "EX-1", "feat(rjc): retirement", "")
	if push {
		jjTest(t, f.repo, "git", "push", "--bookmark", b)
	}
	entry := LedgerEntry{Repo: f.repo.profile.Name, PR: 1, Bookmark: b, Codename: f.repo.codename}
	if err := (Ledger{root: f.repo.stateDir}).Save(entry); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == prPath(f.repo.profile.Name, 1) {
			respondJSON(t, w, map[string]any{"state": "closed", "head": map[string]string{"ref": b}})
			return
		}
		w.WriteHeader(http.StatusNotFound)
		respondJSON(t, w, map[string]string{"message": "Branch not found"})
	}))
	t.Cleanup(server.Close)
	s, _ := service(f.ctx)
	s.forge = &forgeClient{client: server.Client(), baseURL: server.URL}
	return f, b, entry
}

func TestRetireRaceDeletesOnlyValidated(t *testing.T) {
	f, b, _ := retireFixture(t, true)
	other := "test-lane/ex-2-other"
	jjTest(t, f.repo, "bookmark", "create", other, "-r", b)
	jjTest(t, f.repo, "git", "push", "--bookmark", other)
	workspace := filepath.Join(t.TempDir(), "other-workspace")
	jjTest(t, f.repo, "workspace", "add", workspace)
	sibling := *f.repo
	sibling.root = workspace
	called := false
	writer := retirePlanWriter(func(p []byte) (int, error) {
		called = true
		jjTest(t, &sibling, "bookmark", "delete", other)
		return len(p), nil
	})
	if err := f.repo.retire(f.ctx, retireOptions{bookmark: b}, writer); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("dry-run hook did not run")
	}
	remote := filepath.Join(os.Getenv("HOME"), "remote.git")
	out, err := f.repo.command(f.ctx, nil, nil, "git", "--git-dir", remote, "show-ref")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "refs/heads/"+b) || !strings.Contains(string(out), "refs/heads/"+other) {
		t.Fatalf("unexpected remote refs: %s", out)
	}
}

func TestRetireFailedPushKeepsState(t *testing.T) {
	f, b, entry := retireFixture(t, true)
	path, err := (fileBaseline{root: f.repo.stateDir, keys: f.repo.keys}).path(b, ".metadata.json")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, path, "state")
	writer := retirePlanWriter(func(p []byte) (int, error) {
		_, err := f.repo.command(f.ctx, nil, nil, "git", "--git-dir", filepath.Join(os.Getenv("HOME"), "remote.git"), "config", "receive.denyDeletes", "true")
		return len(p), err
	})
	err = f.repo.retire(f.ctx, retireOptions{bookmark: b}, writer)
	var failure *failure
	if !errors.As(err, &failure) || failure.code != 1 || failure.gate != "push-failed" {
		t.Fatalf("expected exit 1 push failure, got %v", err)
	}
	if _, found, err := (Ledger{root: f.repo.stateDir}).Find(entry.Repo, b); err != nil || !found {
		t.Fatalf("ledger found=%v err=%v", found, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state missing: %v", err)
	}
	if _, err := f.repo.one(f.ctx, quoteRev(b)); err != nil {
		t.Fatalf("bookmark not restored: %v", err)
	}
}

func TestRetireForgeFailureKeepsState(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{{"branch-exists", http.StatusOK}, {"server-error", http.StatusInternalServerError}} {
		t.Run(tc.name, func(t *testing.T) {
			f, b, entry := retireFixture(t, false)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path == prPath(f.repo.profile.Name, 1) {
					respondJSON(t, w, map[string]any{"state": "closed", "head": map[string]string{"ref": b}})
					return
				}
				w.WriteHeader(tc.status)
				respondJSON(t, w, map[string]string{"message": "branch response"})
			}))
			defer server.Close()
			s, _ := service(f.ctx)
			s.forge = &forgeClient{client: server.Client(), baseURL: server.URL}
			err := f.repo.retire(f.ctx, retireOptions{bookmark: b}, nil)
			if err == nil {
				t.Fatal("expected refusal")
			}
			if _, err := f.repo.one(f.ctx, quoteRev(b)); err != nil {
				t.Fatalf("bookmark missing: %v", err)
			}
			if _, found, err := (Ledger{root: f.repo.stateDir}).Find(entry.Repo, b); err != nil || !found {
				t.Fatalf("ledger found=%v err=%v", found, err)
			}
		})
	}
}
func TestRetirePlainTextBranchNotFoundKeepsState(t *testing.T) {
	f, b, entry := retireFixture(t, false)
	path, err := (fileBaseline{root: f.repo.stateDir, keys: f.repo.keys}).path(b, ".metadata.json")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, path, "state")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == prPath(f.repo.profile.Name, 1) {
			respondJSON(t, w, map[string]any{"state": "closed", "head": map[string]string{"ref": b}})
			return
		}
		w.WriteHeader(http.StatusNotFound)
		if _, err := w.Write([]byte("Not Found")); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	s, _ := service(f.ctx)
	s.forge = &forgeClient{client: server.Client(), baseURL: server.URL}

	err = f.repo.retire(f.ctx, retireOptions{bookmark: b}, nil)
	if err == nil || !strings.Contains(err.Error(), "branch-unconfirmed") {
		t.Fatalf("expected branch-unconfirmed refusal, got %v", err)
	}
	if _, err := f.repo.one(f.ctx, quoteRev(b)); err != nil {
		t.Fatalf("bookmark missing: %v", err)
	}
	if _, found, err := (Ledger{root: f.repo.stateDir}).Find(entry.Repo, b); err != nil || !found {
		t.Fatalf("ledger found=%v err=%v", found, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("metadata state missing: %v", err)
	}
}

func TestRetireBranchGoneSkipsPush(t *testing.T) {
	f, b, entry := retireFixture(t, false)
	path, err := (fileBaseline{root: f.repo.stateDir, keys: f.repo.keys}).path(b, ".metadata.json")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, path, "state")
	called := false
	writer := retirePlanWriter(func(p []byte) (int, error) { called = true; return len(p), nil })
	if err := f.repo.retire(f.ctx, retireOptions{bookmark: b}, writer); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("unexpected push dry run")
	}
	if _, found, err := (Ledger{root: f.repo.stateDir}).Find(entry.Repo, b); err != nil || found {
		t.Fatalf("ledger found=%v err=%v", found, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("state remains: %v", err)
	}
}

func TestRetireNoPRRefusesWhenPRExists(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	b := f.new(t, "EX-1", "feat(rjc): retirement", "")
	f.prs[b] = 1
	err := f.repo.retire(f.ctx, retireOptions{bookmark: b, noPR: true}, nil)
	if err == nil || !strings.Contains(err.Error(), "pr-exists") {
		t.Fatalf("expected pr-exists refusal, got %v", err)
	}
}

func TestRetireRefusesOpenPR(t *testing.T) {
	f, b, _ := retireFixture(t, false)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		respondJSON(t, w, map[string]any{"state": "open", "head": map[string]string{"ref": b}})
	}))
	defer server.Close()
	s, _ := service(f.ctx)
	s.forge = &forgeClient{client: server.Client(), baseURL: server.URL}
	err := f.repo.retire(f.ctx, retireOptions{bookmark: b}, nil)
	if err == nil || !strings.Contains(err.Error(), "pr-open") {
		t.Fatalf("expected pr-open refusal, got %v", err)
	}
}
