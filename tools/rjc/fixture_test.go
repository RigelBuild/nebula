package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const mainOrigin = "main@origin"

type fixture struct {
	repo                *repository
	ctx                 context.Context
	prs                 map[string]int
	metadata            map[int]Metadata
	merged              map[string]bool
	mergedHeads         map[string]string
	commitPRs           map[string][]string
	commitPRCalls       map[string]int
	commitPRStatus      int
	commitPRMalformed   bool
	commitPRMessage     string
	repoStatus          int
	repoReads           int
	timeline            []map[string]any
	pushes, reviewCalls []string
	noHooks             []bool
	drafts              []bool
	bodyFile            string
	afterPush           func()
	pushError           error
	labelPosts          int
	labelPRs            []int
	labelPayloads       [][]string
	labelStatus         int
	reviewStatus        int
	pullCreates         int
	comments            []map[string]any
	holdStatus          int
	readyCalls          []int
	readyStatus         int
}

func fixtureRepo(t *testing.T) *fixture {
	t.Helper()
	return makeFixtureRepo(t, false)
}

func makeFixtureRepo(t *testing.T, parallel bool) *fixture {
	t.Helper()
	home := t.TempDir()
	runtimeDir := t.TempDir()
	if !parallel {
		t.Setenv("HOME", home)
		t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	}
	config := filepath.Join(home, "jj.toml")
	writeTestFile(t, config, "[user]\nname = 'example'\nemail = 'agent@example.com'\n")
	if !parallel {
		t.Setenv("JJ_CONFIG", config)
	}
	installHookFixture(t, home, parallel)
	root := filepath.Join(home, "agents", "workspaces", "test-lane", "app")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	profile, err := ProfileFor(loadExampleConfig(t), "https://github.com/example/app.git")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := newIssueKeys(profile.IssuePrefix)
	if err != nil {
		t.Fatal(err)
	}
	r := &repository{root: root, codename: "test-lane", stateDir: filepath.Join(home, "state", "rjc"), profile: profile, keys: keys, log: io.Discard, pushedHeadWait: noPushedHeadWait}
	if parallel {
		r.env = []string{"HOME=" + home, "JJ_CONFIG=" + config, "XDG_RUNTIME_DIR=" + runtimeDir, "PATH=" + filepath.Join(home, "bin") + string(os.PathListSeparator) + os.Getenv("PATH"), "RJC_TEST_HELPER=1"}
	}
	jjTest(t, r, "git", "init", "--colocate")
	writeTestFile(t, filepath.Join(root, "README.md"), "initial\n")
	jjTest(t, r, "describe", "-m", "chore: initial repository")
	jjTest(t, r, "bookmark", "create", "main")
	bare := filepath.Join(home, "remote.git")
	if _, err := r.command(t.Context(), nil, nil, "git", "init", "--bare", bare); err != nil {
		t.Fatal(err)
	}
	if _, err := r.command(t.Context(), nil, nil, "git", "--git-dir", bare, "symbolic-ref", "HEAD", "refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	jjTest(t, r, "git", "remote", "add", "origin", bare)
	jjTest(t, r, "git", "push", "--bookmark", "main")
	jjTest(t, r, "new", "main")
	f := &fixture{repo: r, prs: map[string]int{}, metadata: map[int]Metadata{}, timeline: []map[string]any{}, commitPRCalls: map[string]int{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) { f.serve(t, w, request) }))
	t.Cleanup(server.Close)
	f.ctx = withServices(t.Context(), &services{jj: r, engine: f, forge: &forgeClient{client: server.Client(), baseURL: server.URL}})
	f.bodyFile = filepath.Join(home, "body.md")
	writeTestFile(t, f.bodyFile, "## What does this do\n\nThis fixture submits a feature through the real jj state checks.\n\n## Why do we need it\n\nThe fake remote keeps integration tests offline while exercising submit behavior.\n\n## Summary\n\n"+strings.Repeat("safe submission words ", 15)+"\n\n## Verification\n\nTemporary repository checks exercise real jj state.\n\n## Risks\n\nNone: this test has no live remote.\n\n## Spec impact\n\nSpec-impact: none\n")
	return f
}

// cloneFixtureRepo returns a second clone of f's origin, with the same config
// and environment, so a test can publish a rewrite the first clone then fetches.
func cloneFixtureRepo(t *testing.T, base *repository, name string) *repository {
	t.Helper()
	root := filepath.Join(filepath.Dir(base.root), name)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	clone := &repository{root: root, codename: base.codename, stateDir: base.stateDir, profile: base.profile, keys: base.keys, log: io.Discard, pushedHeadWait: noPushedHeadWait, env: base.env}
	jjTest(t, clone, "git", "init", "--colocate")
	jjTest(t, clone, "git", "remote", "add", "origin", base.remoteURL(t))
	jjTest(t, clone, "git", "fetch", "--remote", "origin")
	jjTest(t, clone, "new", mainOrigin)
	return clone
}

// remoteURL returns the fixture origin's bare repository path.
func (r *repository) remoteURL(t *testing.T) string {
	t.Helper()
	return strings.TrimSpace(jjTest(t, r, "git", "remote", "list")[len("origin "):])
}

// noPushedHeadWait skips the post-push head backoff, so fixture pushes settle at once.
func noPushedHeadWait(ctx context.Context, _ time.Duration) error { return ctx.Err() }

func respondJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Error(err)
	}
}
func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
func jjTest(t *testing.T, r *repository, args ...string) string {
	t.Helper()
	out, err := r.jj(t.Context(), args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func (f *fixture) Submit(ctx context.Context, bookmark, bodyPath string, noHooks, draft bool, _ io.Writer) error {
	f.pushes = append(f.pushes, bookmark)
	f.noHooks = append(f.noHooks, noHooks)
	f.drafts = append(f.drafts, draft)
	if f.pushError != nil {
		return f.pushError
	}
	if _, err := f.repo.jj(ctx, "git", "push", "--bookmark", bookmark); err != nil {
		return err
	}
	tip, err := f.repo.one(ctx, quoteRev(bookmark))
	if err != nil {
		return err
	}
	rangeRev := quoteRev(f.repo.profile.TrunkRemote()) + ".." + quoteRev(bookmark)
	if f.repo.profile.Posture == forkTracking {
		rangeRev += " ~ ::" + quoteRev(f.repo.profile.Mirror+"@origin")
	}
	ancestors, err := f.repo.stackBookmarks(ctx, rangeRev)
	if err != nil {
		return err
	}
	parent := f.repo.profile.TrunkRemote()
	for _, name := range ancestors {
		if strings.TrimSuffix(name, "@origin") != bookmark {
			parent = name
		}
	}
	base, err := f.repo.one(ctx, quoteRev(parent))
	if err != nil {
		return err
	}
	number := f.prs[bookmark]
	current := f.metadata[number]
	if number == 0 {
		body, err := os.ReadFile(bodyPath)
		if err != nil {
			return err
		}
		number = len(f.prs) + 1
		f.prs[bookmark] = number
		current = Metadata{Title: "root title", Body: string(body), Draft: draft}
	}
	if f.repo.profile.Posture == forkNone || current.BaseRef == "" {
		current.BaseRef = strings.TrimSuffix(parent, "@origin")
		current.BaseSHA = base
	}
	current.HeadRef = bookmark
	current.HeadSHA = tip
	f.metadata[number] = current
	if f.afterPush != nil {
		f.afterPush()
	}
	return nil
}
func (f *fixture) serve(t *testing.T, w http.ResponseWriter, request *http.Request) {
	t.Helper()
	switch {
	case request.URL.Path == "/user":
		respondJSON(t, w, map[string]string{"login": "example-agent"})
	case request.URL.Path == "/graphql":
		f.serveGraphQL(t, w, request)
	case request.URL.Path == "/repos/"+f.repo.profile.Name:
		f.repoReads++
		if f.repoStatus != 0 {
			w.WriteHeader(f.repoStatus)
			respondJSON(t, w, map[string]string{"message": "Not Found"})
			return
		}
		respondJSON(t, w, map[string]string{"full_name": f.repo.profile.Name})
	case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/comments"):
		pr := pathIssueNumber(request.URL.Path)
		comments := []map[string]any{}
		for _, c := range f.comments {
			// A comment a test appends by hand has no "pr" and shows on every PR.
			if n, ok := c["pr"].(int); !ok || n == pr {
				comments = append(comments, c)
			}
		}
		respondJSON(t, w, comments)
	case strings.HasSuffix(request.URL.Path, "/requested_reviewers"), strings.HasSuffix(request.URL.Path, "/comments"):
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
		}
		f.reviewCalls = append(f.reviewCalls, request.URL.Path+" "+string(body))
		var posted struct {
			Body string `json:"body"`
		}
		if err := json.Unmarshal(body, &posted); err != nil {
			t.Error(err)
		}
		if strings.HasPrefix(posted.Body, "Hold:") {
			if f.holdStatus != 0 {
				w.WriteHeader(f.holdStatus)
				respondJSON(t, w, map[string]string{"message": "fixture hold failure"})
				return
			}
			f.comments = append(f.comments, map[string]any{"id": len(f.comments) + 1, "pr": pathIssueNumber(request.URL.Path), "body": posted.Body, "user": map[string]string{"login": "example-agent"}})
			w.WriteHeader(http.StatusCreated)
			return
		}
		if f.reviewStatus != 0 {
			w.WriteHeader(f.reviewStatus)
			respondJSON(t, w, map[string]string{"message": "fixture review failure"})
			return
		}
		if strings.HasSuffix(request.URL.Path, "/comments") {
			f.comments = append(f.comments, map[string]any{"id": len(f.comments) + 1, "pr": pathIssueNumber(request.URL.Path), "body": posted.Body, "user": map[string]string{"login": "example-agent"}})
		}
		w.WriteHeader(http.StatusCreated)
	case strings.Contains(request.URL.Path, "/issues/") && strings.HasSuffix(request.URL.Path, "/labels"):
		f.serveLabels(t, w, request)
	case strings.Contains(request.URL.Path, "/commits/") && strings.HasSuffix(request.URL.Path, "/pulls"):
		f.serveCommitPRs(t, w, request)
	case strings.HasSuffix(request.URL.Path, "/pulls"):
		_, bookmark, _ := strings.Cut(request.URL.Query().Get("head"), ":")
		entries := []map[string]any{}
		closed := request.URL.Query().Get("state") == "closed"
		if number := f.prs[bookmark]; number != 0 && closed == f.merged[bookmark] {
			entry := map[string]any{"number": number, "head": map[string]string{"ref": bookmark, "sha": f.mergedHeads[bookmark]}}
			if closed {
				entry["merged_at"] = "2026-11-01T00:00:00Z"
			}
			entries = append(entries, entry)
		}
		respondJSON(t, w, entries)
	case strings.HasSuffix(request.URL.Path, "/timeline"):
		respondJSON(t, w, f.timeline)
	default:
		f.serveMetadata(t, w, request)
	}
}

// serveGraphQL answers markReady: the node lookup returns PR_<n>, and the mutation clears that PR's draft flag.
func (f *fixture) serveGraphQL(t *testing.T, w http.ResponseWriter, request *http.Request) {
	t.Helper()
	var payload struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
		t.Error(err)
		return
	}
	if number, ok := payload.Variables["number"].(float64); ok {
		respondJSON(t, w, map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]string{"id": "PR_" + strconv.Itoa(int(number))}}}})
		return
	}
	id, _ := payload.Variables["id"].(string)
	number, err := strconv.Atoi(strings.TrimPrefix(id, "PR_"))
	if err != nil {
		t.Errorf("unexpected graphql request: %s", payload.Query)
		return
	}
	if f.readyStatus != 0 {
		w.WriteHeader(f.readyStatus)
		respondJSON(t, w, map[string]string{"message": "fixture ready failure"})
		return
	}
	f.readyCalls = append(f.readyCalls, number)
	current := f.metadata[number]
	current.Draft = false
	f.metadata[number] = current
	respondJSON(t, w, map[string]any{"data": map[string]any{"markPullRequestReadyForReview": map[string]any{"pullRequest": map[string]bool{"isDraft": false}}}})
}

func (f *fixture) serveCommitPRs(t *testing.T, w http.ResponseWriter, request *http.Request) {
	t.Helper()
	sha := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/repos/"+f.repo.profile.Name+"/commits/"), "/pulls")
	f.commitPRCalls[sha]++
	if request.Method != http.MethodGet || request.URL.Query().Get("per_page") != "100" {
		t.Errorf("unexpected commit association request: %s %s", request.Method, request.URL)
	}
	if f.commitPRStatus != 0 {
		w.WriteHeader(f.commitPRStatus)
		message := strings.ReplaceAll(f.commitPRMessage, "<sha>", sha)
		respondJSON(t, w, map[string]string{"message": message, "status": strconv.Itoa(f.commitPRStatus)})
		return
	}
	if f.commitPRMalformed {
		if _, err := io.WriteString(w, "{"); err != nil {
			t.Error(err)
		}
		return
	}
	entries := make([]map[string]any, 0, len(f.commitPRs[sha]))
	for _, head := range f.commitPRs[sha] {
		entries = append(entries, map[string]any{"head": map[string]string{"ref": head}})
	}
	respondJSON(t, w, entries)
}
func (f *fixture) serveMetadata(t *testing.T, w http.ResponseWriter, request *http.Request) {
	t.Helper()
	var number int
	for _, n := range f.prs {
		if strings.HasSuffix(request.URL.Path, "/pulls/"+strconv.Itoa(n)) {
			number = n
		}
	}
	current, ok := f.metadata[number]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if request.Method == http.MethodPatch {
		var payload map[string]string
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if title, ok := payload["title"]; ok {
			current.Title = title
		}
		if body, ok := payload["body"]; ok {
			current.Body = body
		}
		if base, ok := payload["base"]; ok {
			current.BaseRef = base
			sha, err := f.repo.one(request.Context(), quoteRev(base+"@origin"))
			if err != nil {
				t.Error(err)
				return
			}
			current.BaseSHA = sha
		}
		f.metadata[number] = current
	}
	respondJSON(t, w, githubMetadata(current))
}

func (f *fixture) serveLabels(t *testing.T, w http.ResponseWriter, request *http.Request) {
	t.Helper()
	if request.Method != http.MethodPost {
		t.Errorf("unexpected label request: %s %s", request.Method, request.URL)
	}
	f.labelPosts++
	parts := strings.Split(strings.Trim(request.URL.Path, "/"), "/")
	number, err := strconv.Atoi(parts[len(parts)-2])
	if err != nil {
		t.Fatal(err)
	}
	f.labelPRs = append(f.labelPRs, number)
	if f.labelStatus != 0 {
		w.WriteHeader(f.labelStatus)
		respondJSON(t, w, map[string]string{"message": "fixture label failure"})
		return
	}
	var payload struct {
		Labels []string `json:"labels"`
	}
	if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	f.labelPayloads = append(f.labelPayloads, payload.Labels)
	current := f.metadata[number]
	current.Labels = append(current.Labels, payload.Labels...)
	f.metadata[number] = current
	respondJSON(t, w, []map[string]string{})
}
func (f *fixture) new(t *testing.T, issue, title, on string) string {
	t.Helper()
	bookmark, err := f.repo.newChange(f.ctx, newOptions{issue: issue, title: title, on: on})
	if err != nil {
		t.Fatal(err)
	}
	return bookmark
}
func (f *fixture) commit(t *testing.T, path string) {
	t.Helper()
	f.commitFile(t, path, "committed\n")
}

func (f *fixture) commitFile(t *testing.T, path, body string) {
	t.Helper()
	writeTestFile(t, filepath.Join(f.repo.root, path), body)
	jjTest(t, f.repo, "commit", "-m", "fix: committed test work")
}

// pathIssueNumber returns the number in an /issues/<n>/... path, or 0.
func pathIssueNumber(path string) int {
	_, rest, ok := strings.Cut(path, "/issues/")
	if !ok {
		return 0
	}
	digits, _, _ := strings.Cut(rest, "/")
	n, _ := strconv.Atoi(digits)
	return n
}

// prCommentBodies returns the bodies of comments posted on pr, oldest first.
func (f *fixture) prCommentBodies(pr int) []string {
	var bodies []string
	for _, c := range f.comments {
		if n, _ := c["pr"].(int); n == pr {
			body, _ := c["body"].(string)
			bodies = append(bodies, body)
		}
	}
	return bodies
}
