package main

import (
	"bytes"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestRealVineRetargetDivergedMirror(t *testing.T) {
	f := fixtureRepo(t)
	profile, err := ProfileFor(loadExampleConfig(t), "https://github.com/example/tracked-fork")
	if err != nil {
		t.Fatal(err)
	}
	f.repo.profile = profile
	jjTest(t, f.repo, "bookmark", "create", "rigel-release", "-r", "main@origin")
	jjTest(t, f.repo, "git", "push", "--bookmark", "rigel-release")
	jjTest(t, f.repo, "new", "rigel-release@origin")
	f.commit(t, "release-only")
	jjTest(t, f.repo, "bookmark", "move", "rigel-release", "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", "rigel-release")
	jjTest(t, f.repo, "new", "main@origin")
	f.commit(t, "mirror-only")
	jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", "main")
	mirror := jjTest(t, f.repo, "log", "--no-graph", "-r", "main@origin", "-T", "commit_id")
	bookmark := f.new(t, "EX-1", "feat(rjc): use release trunk", "")
	f.commit(t, "release.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	if _, err := f.repo.jj(f.ctx, "git", "push", "--bookmark", bookmark); err != nil {
		t.Fatal(err)
	}
	head := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	base := jjTest(t, f.repo, "log", "--no-graph", "-r", "main@origin", "-T", "commit_id")
	f.prs[bookmark] = 1
	f.metadata[1] = Metadata{Title: "feat(rjc): use release trunk (EX-1)", Body: "## Summary\n\nExisting body\n", HeadRef: bookmark, HeadSHA: head, BaseRef: "main", BaseSHA: base, Draft: true}
	if err := (fileBaseline{root: f.repo.stateDir, keys: f.repo.keys}).Commit(bookmark, Published{Title: f.metadata[1].Title, Authored: f.metadata[1].Body}); err != nil {
		t.Fatal(err)
	}
	server := realVineServer(t, f, profile, bookmark)
	defer server.Close()
	configureRealVine(t, f, server)
	s, err := service(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.forge = &forgeClient{baseURL: server.URL, client: server.Client()}
	oldBase := jjTest(t, f.repo, "log", "--no-graph", "-r", "rigel-release@origin", "-T", "commit_id")
	store := localPrePush{root: f.repo.stateDir}
	priorLine := []Change{{Bookmark: bookmark, Tip: head, Base: "main@origin", Owned: true}}
	if err := f.repo.runHooks(f.ctx, priorLine, nil, localHookRunner{repo: f.repo}, store); err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(t.TempDir(), "hook-env.json")
	t.Setenv("RJC_TEST_HOOK_ENV", envPath)
	opt := submitOptions{bookmark: bookmark, title: "feat(rjc): use release trunk", bodyFile: f.bodyFile, retarget: true}
	f.pushError = errors.New("simulated push failure")
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err == nil || f.metadata[1].BaseRef != "main" {
		t.Fatalf("failed push did not restore base: %v, %+v", err, f.metadata[1])
	}
	head = prepareRetargetRetry(t, f, bookmark, oldBase, opt)
	if err := os.Remove(envPath); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(HookRecord{Repo: profile.Name, Head: head, Base: mirror, Result: hookPass}); err != nil {
		t.Fatal(err)
	}
	f.pushError = nil
	var log bytes.Buffer
	if err := f.repo.submit(f.ctx, opt, vine{repo: f.repo}, &log); err != nil {
		t.Fatalf("real jj-vine retarget: %v\n%s", err, log.String())
	}
	data, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("retarget did not rerun hooks: %v", err)
	}
	if got := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark+"@origin"), "-T", "commit_id"); got != head {
		t.Fatalf("retry did not push head: %s", got)
	}
	var hookEnv map[string]string
	if err := json.Unmarshal(data, &hookEnv); err != nil {
		t.Fatal(err)
	}
	if hookEnv["base"] != oldBase || hookEnv["head"] != head {
		t.Fatalf("retarget hook range: %v", hookEnv)
	}
	if got := f.metadata[1]; got.BaseRef != "rigel-release" || got.HeadSHA != head {
		t.Fatalf("PR = %+v", got)
	}
	if got := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark+"@origin"), "-T", "commit_id"); got != head {
		t.Fatalf("remote head moved: %s", got)
	}
	if got, err := f.repo.revisions(f.ctx, quoteRev(mirror)+" & ::"+quoteRev(head)); err != nil || len(got) != 0 {
		t.Fatalf("mirror in range: %v %v", got, err)
	}
}

func TestRealVineRecordedMergeSelection(t *testing.T) {
	t.Parallel()
	f := trackingFixture(t, "main", false)
	bookmark, err := f.repo.upstreamSync(f.ctx, upstreamSyncOptions{issue: "EX-1"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	head := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	base := jjTest(t, f.repo, "log", "--no-graph", "-r", "rigel-release@origin", "-T", "commit_id")
	jjTest(t, f.repo, "git", "push", "--bookmark", bookmark)
	f.prs[bookmark] = 1
	f.metadata[1] = Metadata{Title: "chore(fork): upstream merge (EX-1)", Body: "## Summary\n\nExisting body\n", HeadRef: bookmark, HeadSHA: head, BaseRef: "rigel-release", BaseSHA: base, Draft: true}
	server := realVineServer(t, f, f.repo.profile, bookmark)
	defer server.Close()
	configureRealVine(t, f, server)
	line, err := f.repo.line(f.ctx, bookmark, lineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	if err := (vine{repo: f.repo, line: line}).Submit(f.ctx, bookmark, f.bodyFile, true, false, &log); err != nil {
		t.Fatalf("jj-vine merge selection: %v\n%s", err, log.String())
	}
	var submitted []string
	for line := range strings.SplitSeq(log.String(), "\n") {
		if names, ok := strings.CutPrefix(line, "Submitting bookmarks: "); ok {
			submitted = append(submitted, strings.Fields(names)...)
		}
	}
	if len(submitted) != 1 || submitted[0] != bookmark {
		t.Fatalf("jj-vine submitted %v, want only %s; log: %s", submitted, bookmark, log.String())
	}
}

func TestRealVineKeepsForeignParentBaseOnCreateAndResubmit(t *testing.T) {
	f, foreign, parentTip, child := parentFixture(t)
	// Model a parent that is only published remotely, as in the reported foreign-PR stack.
	jjTest(t, f.repo, "bookmark", "delete", foreign)
	mainTip := jjTest(t, f.repo, "log", "--no-graph", "-r", "main@origin", "-T", "commit_id")
	f.prs[foreign] = 1
	f.metadata[1] = Metadata{
		Title: "feat(rjc): parent work to land (EX-1)", Body: "## Summary\n\nParent PR body\n",
		HeadRef: foreign, HeadSHA: parentTip, BaseRef: "main", BaseSHA: mainTip, Draft: true,
	}
	server := realVineServer(t, f, f.repo.profile, child)
	defer server.Close()
	configureTrackingVine(t, f, server)
	opt := submitOptions{bookmark: child, title: "feat(rjc): land the child independently", bodyFile: f.bodyFile}
	created := 0
	for _, attempt := range []string{"create", "resubmit"} {
		var log bytes.Buffer
		err := f.repo.submit(f.ctx, opt, vine{repo: f.repo}, &log)
		if err != nil {
			t.Fatalf("%s through jj-vine: %v; PR=%+v\n%s", attempt, err, f.metadata[f.prs[child]], log.String())
		}
		pr := f.prs[child]
		if pr == 0 || f.metadata[pr].BaseRef != foreign {
			t.Fatalf("%s PR base = %q, want foreign parent %q (PR %d)", attempt, f.metadata[pr].BaseRef, foreign, pr)
		}
		if attempt == "create" {
			created = pr
		} else if pr != created || f.pullCreates != 1 {
			t.Fatalf("resubmit created another PR: initial=%d current=%d create calls=%d", created, pr, f.pullCreates)
		}
	}
}

func realVineServer(t *testing.T, f *fixture, profile Profile, bookmark string) *httptest.Server {
	t.Helper()
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/graphql" {
			serveVineGraphQL(t, f, profile, bookmark, w, req)
			return
		}
		if req.URL.Path == "/repos/"+profile.Name+"/pulls" && req.Method == http.MethodPost {
			serveVineCreatePull(t, f, w, req)
			return
		}
		if strings.HasSuffix(req.URL.Path, "/requested_reviewers") || strings.HasSuffix(req.URL.Path, "/comments") {
			f.serve(t, w, req)
			return
		}
		if strings.HasPrefix(req.URL.Path, "/repos/"+profile.Name+"/pulls/") {
			serveVinePull(t, f, w, req)
			return
		}
		f.serve(t, w, req)
	}))
}

func serveVineCreatePull(t *testing.T, f *fixture, w http.ResponseWriter, req *http.Request) {
	t.Helper()
	f.pullCreates++
	var request struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		Draft bool   `json:"draft"`
	}
	if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
		t.Error(err)
		return
	}
	if f.prs[request.Head] != 0 {
		t.Errorf("jj-vine attempted to create a duplicate PR for %s", request.Head)
		w.WriteHeader(http.StatusConflict)
		return
	}
	headSHA, err := f.repo.one(req.Context(), quoteRev(request.Head+"@origin"))
	if err != nil {
		t.Error(err)
		return
	}
	baseSHA, err := f.repo.one(req.Context(), quoteRev(request.Base+"@origin"))
	if err != nil {
		t.Error(err)
		return
	}
	number := 1
	for _, current := range f.prs {
		if current >= number {
			number = current + 1
		}
	}
	f.prs[request.Head] = number
	f.metadata[number] = Metadata{Title: request.Title, Body: request.Body, HeadRef: request.Head, HeadSHA: headSHA, BaseRef: request.Base, BaseSHA: baseSHA, Draft: request.Draft}
	w.WriteHeader(http.StatusCreated)
	respondJSON(t, w, decorateVinePR(number, f.metadata[number]))
}

func serveVineGraphQL(t *testing.T, f *fixture, profile Profile, bookmark string, w http.ResponseWriter, req *http.Request) {
	t.Helper()
	var request struct {
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
		t.Error(err)
		return
	}
	nodes := []any{}
	name, ok := request.Variables["headRefName"].(string)
	if !ok {
		name = bookmark
	}
	if number := f.prs[name]; number != 0 {
		m := f.metadata[number]
		url := "https://example.test/pr/" + strconv.Itoa(number)
		pr := decorateVinePR(number, m)
		pr["databaseId"] = number
		pr["isDraft"] = m.Draft
		pr["headRepository"] = map[string]string{"nameWithOwner": profile.Name}
		pr["baseRepository"] = map[string]string{"nameWithOwner": profile.Name}
		pr["headRefName"], pr["headRefOid"] = m.HeadRef, m.HeadSHA
		pr["baseRefName"], pr["baseRefOid"] = m.BaseRef, m.BaseSHA
		pr["state"], pr["url"], pr["merged"], pr["createdAt"] = "OPEN", url, false, "2026-01-01T00:00:00Z"
		pr["assignees"] = map[string]any{"nodes": []any{}}
		pr["reviewRequests"] = map[string]any{"nodes": []any{}}
		nodes = append(nodes, pr)
	}
	respondJSON(t, w, map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequests": map[string]any{"nodes": nodes}}}})
}

func serveVinePull(t *testing.T, f *fixture, w http.ResponseWriter, req *http.Request) {
	t.Helper()
	_, idText, ok := strings.Cut(req.URL.Path, "/pulls/")
	if !ok {
		t.Errorf("unexpected pull request path %q", req.URL.Path)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	id, err := strconv.Atoi(idText)
	if err != nil {
		t.Errorf("invalid pull request number %q", idText)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	m, exists := f.metadata[id]
	if !exists {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if req.Method == http.MethodGet {
		if sha, err := f.repo.one(req.Context(), quoteRev(m.HeadRef+"@origin")); err == nil {
			m.HeadSHA = sha
			f.metadata[id] = m
		}
	}
	if req.Method == http.MethodPatch {
		var patch map[string]string
		if err := json.NewDecoder(req.Body).Decode(&patch); err != nil {
			t.Error(err)
			return
		}
		if value, ok := patch["base"]; ok {
			m.BaseRef = value
			sha, err := f.repo.one(req.Context(), quoteRev(value+"@origin"))
			if err != nil {
				t.Error(err)
				return
			}
			m.BaseSHA = sha
		}
		if value, ok := patch["body"]; ok {
			m.Body = value
		}
		if value, ok := patch["title"]; ok {
			m.Title = value
		}
		f.metadata[id] = m
	}
	respondJSON(t, w, decorateVinePR(id, f.metadata[id]))
}

// decorateVinePR adds the PR envelope fields GitHub returns beside a metadata body.
func decorateVinePR(number int, m Metadata) map[string]any {
	out := githubMetadata(m)
	out["number"], out["id"], out["state"] = number, number, "open"
	out["html_url"], out["created_at"] = "https://example.test/pr/"+strconv.Itoa(number), "2026-01-01T00:00:00Z"
	out["user"] = map[string]any{"id": 1, "login": "test"}
	out["assignees"], out["requested_reviewers"] = []any{}, []any{}
	return out
}

// configureRealVine points jj-vine at the TLS fixture server with the test CA. It skips the test when jj-vine is not
// on PATH.
func configureRealVine(t *testing.T, f *fixture, server *httptest.Server) {
	t.Helper()
	if _, err := exec.LookPath("jj-vine"); err != nil {
		t.Skip("jj-vine is not on PATH")
	}
	cert := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"jj-vine.forge": "github", "jj-vine.github.host": server.URL, "jj-vine.github.project": f.repo.profile.Name, "jj-vine.github.token": "fixture", "jj-vine.caBundle": cert, "jj-vine.tlsAcceptNonCompliantCerts": "true", "jj-vine.github.linkStack": "false", "jj-vine.fetch": "false"} {
		jjTest(t, f.repo, "config", "set", "--workspace", key, value)
	}
}

func prepareRetargetRetry(t *testing.T, f *fixture, bookmark, base string, opt submitOptions) string {
	t.Helper()
	f.pushError = nil
	m := f.metadata[1]
	m.BaseRef, m.BaseSHA = "rigel-release", base
	f.metadata[1] = m
	err := f.repo.submit(f.ctx, opt, f, io.Discard)
	assertRefusalGate(t, err, "base")
	if !strings.Contains(err.Error(), "already targets trunk") || len(f.pushes) != 1 {
		t.Fatalf("no-op retarget: %v, pushes %v", err, f.pushes)
	}
	f.commit(t, "pending-retry.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	return jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
}
