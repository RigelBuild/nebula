package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func requireExitCode(t *testing.T, err error) {
	t.Helper()
	var status exitCodeError
	if !errors.As(err, &status) || status.code != 1 {
		t.Fatalf("error = %v, want exit code 1", err)
	}
}

func TestParseArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want parseResult
	}{
		{"'305' alone → ok, empty repo, github routing", []string{"305"}, parseResult{OK: true, PR: "305", Forge: ForgeGitHub}},
		{"'305 owner/repo' (positional repo) → ok", []string{"305", "owner/repo"}, parseResult{OK: true, PR: "305", Repo: "owner/repo", Forge: ForgeGitHub}},
		{"'305 --repo owner/repo' (space form) → ok", []string{"305", "--repo", "owner/repo"}, parseResult{OK: true, PR: "305", Repo: "owner/repo", Forge: ForgeGitHub}},
		{"'305 --repo=owner/repo' (equals form) → ok", []string{"305", "--repo=owner/repo"}, parseResult{OK: true, PR: "305", Repo: "owner/repo", Forge: ForgeGitHub}},
		{"'305 --forge forgejo' (space form) → routes forgejo", []string{"305", "--forge", "forgejo"}, parseResult{OK: true, PR: "305", Forge: ForgeForgejo}},
		{"'305 --forge=forgejo' (equals form) → routes forgejo", []string{"305", "--forge=forgejo"}, parseResult{OK: true, PR: "305", Forge: ForgeForgejo}},
		{"empty argv (missing pr) → not ok, no message", nil, parseResult{}},
		{"leading-dash where pr goes → not ok, no message", []string{"-5"}, parseResult{}},
		{"'305 --repo' (no value) → needs value", []string{"305", "--repo"}, parseResult{Msg: "get-pr-diff: --repo needs an owner/repo value"}},
		{"'305 --repo --forge' (dash value) → needs value", []string{"305", "--repo", "--forge"}, parseResult{Msg: "get-pr-diff: --repo needs an owner/repo value"}},
		{"'305 --repo=' (empty equals) → needs value", []string{"305", "--repo="}, parseResult{Msg: "get-pr-diff: --repo needs an owner/repo value"}},
		{"'305 --forge bogus' → forge must be github or forgejo", []string{"305", "--forge", "bogus"}, parseResult{Msg: "get-pr-diff: --forge must be github or forgejo"}},
		{"'305 --forge=bogus' → forge must be github or forgejo", []string{"305", "--forge=bogus"}, parseResult{Msg: "get-pr-diff: --forge must be github or forgejo"}},
		{"'305 --bogus' → unknown option names the flag", []string{"305", "--bogus"}, parseResult{Msg: "get-pr-diff: unknown option '--bogus'"}},
		{"'305 owner/repo extra' (second positional) → unexpected argument", []string{"305", "owner/repo", "extra"}, parseResult{Msg: "get-pr-diff: unexpected argument 'extra'"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := parseArgs(test.args); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("parseArgs(%q) = %#v, want %#v", test.args, got, test.want)
			}
		})
	}
}

func TestExtractTouchedFiles(t *testing.T) {
	tests := []struct {
		name  string
		input any
		want  []string
	}{
		{"gh shape: array of {path} → paths", []any{map[string]any{"path": "src/a.ts"}, map[string]any{"path": "src/b.ts"}}, []string{"src/a.ts", "src/b.ts"}},
		{"forgejo shape: array of {filename} → filenames", []any{map[string]any{"filename": "x.go"}}, []string{"x.go"}},
		{"mixed path/filename entries → both fields read", []any{map[string]any{"path": "a.ts"}, map[string]any{"filename": "b.go"}}, []string{"a.ts", "b.go"}},
		{"path wins over filename when both present", []any{map[string]any{"path": "a.ts", "filename": "b.ts"}}, []string{"a.ts"}},
		{"entry missing both path and filename → skipped", []any{map[string]any{"path": "keep.ts"}, map[string]any{"other": "x"}, map[string]any{}}, []string{"keep.ts"}},
		{"empty-string path → skipped", []any{map[string]any{"path": ""}, map[string]any{"path": "real.ts"}}, []string{"real.ts"}},
		{"non-array input → []", map[string]any{"path": "a.ts"}, []string{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := extractTouchedFiles(test.input); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("extractTouchedFiles() = %#v, want %#v", got, test.want)
			}
		})
	}
	if got := extractTouchedFiles(nil); len(got) != 0 {
		t.Fatalf("extractTouchedFiles(nil) = %#v", got)
	}
	if got := extractTouchedFiles("nope"); len(got) != 0 {
		t.Fatalf("extractTouchedFiles(string) = %#v", got)
	}
}

func TestParseFileLines(t *testing.T) {
	tests := []struct {
		name, input string
		want        []string
	}{
		{"newline-separated paths (trailing newline) → trimmed list", "a.ts\nb.ts\n", []string{"a.ts", "b.ts"}},
		{"empty string → []", "", []string{}},
		{"surrounding whitespace on each line → trimmed", "  a.ts  \n\tb.ts\t", []string{"a.ts", "b.ts"}},
		{"blank interior lines → dropped", "a.ts\n\n   \nb.ts\n", []string{"a.ts", "b.ts"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := parseFileLines(test.input); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("parseFileLines() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestGitHubRequestArgs(t *testing.T) {
	tests := []struct {
		name string
		got  []string
		want []string
	}{
		{"diff args without repo → no --repo appended", buildGitHubDiffArgs("305", ""), []string{"pr", "diff", "305"}},
		{"diff args with repo → --repo appended", buildGitHubDiffArgs("305", "owner/repo"), []string{"pr", "diff", "305", "--repo", "owner/repo"}},
		{"files args without repo → {owner}/{repo} placeholder, --paginate", buildGitHubFilesArgs("305", ""), []string{"api", "repos/{owner}/{repo}/pulls/305/files", "--paginate", "--jq", ".[].filename"}},
		{"files args with repo → repo filled into the api path", buildGitHubFilesArgs("305", "owner/repo"), []string{"api", "repos/owner/repo/pulls/305/files", "--paginate", "--jq", ".[].filename"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if !reflect.DeepEqual(test.got, test.want) {
				t.Fatalf("got %#v, want %#v", test.got, test.want)
			}
		})
	}
}

func TestForgejoRequestBuilders(t *testing.T) {
	tests := []struct {
		name, got, want string
	}{
		{"diff req: trailing slash stripped, .diff suffix, correct path", buildForgejoDiffRequest("https://forge.example.com/", "owner/repo", "305", "tok").URL, "https://forge.example.com/api/v1/repos/owner/repo/pulls/305.diff"},
		{"files req: /files suffix (no .diff), page+limit query, correct path", buildForgejoFilesRequest("https://forge.example.com", "owner/repo", "305", "tok", 1, 50).URL, "https://forge.example.com/api/v1/repos/owner/repo/pulls/305/files?page=1&limit=50"},
		{"repo/pr segments percent-encoded, / separators + path shape intact", buildForgejoDiffRequest("https://forge.example.com", "my org/re po", "30 5", "tok").URL, "https://forge.example.com/api/v1/repos/my%20org/re%20po/pulls/30%205.diff"},
		{"non-ASCII repo segment percent-encoded in the files req", buildForgejoFilesRequest("https://forge.example.com", "café/dépôt", "7", "tok", 1, 50).URL, "https://forge.example.com/api/v1/repos/caf%C3%A9/d%C3%A9p%C3%B4t/pulls/7/files?page=1&limit=50"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.got != test.want {
				t.Fatalf("got %q, want %q", test.got, test.want)
			}
		})
	}
	t.Run("token present → Authorization: token <tok> header", func(t *testing.T) {
		if got := buildForgejoDiffRequest("https://f", "o/r", "1", "sekret").Headers["Authorization"]; got != "token sekret" {
			t.Fatalf("authorization = %q", got)
		}
	})
	t.Run("empty token → no Authorization header (empty headers)", func(t *testing.T) {
		if got := buildForgejoDiffRequest("https://f", "o/r", "1", "").Headers; len(got) != 0 {
			t.Fatalf("headers = %#v", got)
		}
	})
}

func TestTouchedFilesFromDiff(t *testing.T) {
	tests := []struct {
		name, diff string
		want       []string
	}{
		{"multi-file diff → all +++ b/ paths with b/ stripped", "diff --git a/src/foo.ts b/src/foo.ts\n--- a/src/foo.ts\n+++ b/src/foo.ts\n@@ -1 +1 @@\n-old\n+new\ndiff --git a/bar.md b/bar.md\n--- a/bar.md\n+++ b/bar.md\n@@ -0,0 +1 @@\n+added", []string{"src/foo.ts", "bar.md"}},
		{"delete-only diff (+++ /dev/null) → old path, deleted file included", "diff --git a/gone.ts b/gone.ts\ndeleted file mode 100644\n--- a/gone.ts\n+++ /dev/null\n@@ -1 +0,0 @@\n-bye", []string{"gone.ts"}},
		{"mixed modify + delete → both the kept and the deleted path", "diff --git a/kept.ts b/kept.ts\n--- a/kept.ts\n+++ b/kept.ts\n@@ -1 +1 @@\n-a\n+b\ndiff --git a/gone.ts b/gone.ts\ndeleted file mode 100644\n--- a/gone.ts\n+++ /dev/null\n@@ -1 +0,0 @@\n-bye", []string{"kept.ts", "gone.ts"}},
		{"same path appearing twice → deduped to one entry", "diff --git a/dup.ts b/dup.ts\n--- a/dup.ts\n+++ b/dup.ts\n@@ -1 +1 @@\n-a\n+b\ndiff --git a/dup.ts b/dup.ts\n--- a/dup.ts\n+++ b/dup.ts\n@@ -5 +5 @@\n-c\n+d", []string{"dup.ts"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := touchedFilesFromDiff(test.diff); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("touchedFilesFromDiff() = %#v, want %#v", got, test.want)
			}
		})
	}
}

type captured struct {
	out       strings.Builder
	err       strings.Builder
	ghCalls   [][]string
	httpCalls []forgejoRequest
	gh        func([]string) *string
	httpGet   func(forgejoRequest) string
	env       map[string]string
}

func newTestDeps(args []string) (deps, *captured) {
	state := &captured{env: map[string]string{}}
	deps := deps{
		Argv: args,
		Env:  func(name string) string { return state.env[name] },
		Out:  &state.out,
		Err:  &state.err,
		GH: func(_ context.Context, args []string) (*string, error) {
			state.ghCalls = append(state.ghCalls, append([]string{}, args...))
			if state.gh != nil {
				return state.gh(args), nil
			}
			result := ""
			return &result, nil
		},
		HTTPGet: func(_ context.Context, request forgejoRequest) string {
			state.httpCalls = append(state.httpCalls, request)
			if state.httpGet != nil {
				return state.httpGet(request)
			}
			return ""
		},
	}
	return deps, state
}

const testGitHubDiff = "diff --git a/src/foo.ts b/src/foo.ts\n--- a/src/foo.ts\n+++ b/src/foo.ts\n@@ -1 +1 @@\n-old\n+new"

func TestRunOnceGitHub(t *testing.T) {
	t.Run("happy path → gh gets built argv, JSON out, exit 0", func(t *testing.T) {
		deps, state := newTestDeps([]string{"305", "owner/repo"})
		state.gh = func(args []string) *string {
			value := testGitHubDiff
			if args[0] == "api" {
				value = "src/foo.ts\n"
			}
			return &value
		}
		if err := runOnce(t.Context(), deps); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(state.ghCalls, [][]string{{"pr", "diff", "305", "--repo", "owner/repo"}, {"api", "repos/owner/repo/pulls/305/files", "--paginate", "--jq", ".[].filename"}}) {
			t.Fatalf("gh calls = %#v", state.ghCalls)
		}
		var payload struct {
			Diff         string   `json:"diff"`
			TouchedFiles []string `json:"touchedFiles"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(state.out.String())), &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Diff != testGitHubDiff || !reflect.DeepEqual(payload.TouchedFiles, []string{"src/foo.ts"}) {
			t.Fatalf("payload = %#v", payload)
		}
	})
	t.Run("empty diff → err + exit 1, no JSON emitted", func(t *testing.T) {
		deps, state := newTestDeps([]string{"305", "owner/repo"})
		state.gh = func([]string) *string { value := ""; return &value }
		var status exitCodeError
		if err := runOnce(t.Context(), deps); !errors.As(err, &status) || status.code != 1 {
			t.Fatalf("runOnce error = %v", err)
		}
		if !strings.Contains(state.err.String(), "no diff resolved") || state.out.Len() != 0 {
			t.Fatalf("stdout %q, stderr %q", state.out.String(), state.err.String())
		}
	})
	t.Run("files endpoint empty → touchedFiles derived from the diff", func(t *testing.T) {
		deps, state := newTestDeps([]string{"305", "owner/repo"})
		state.gh = func(args []string) *string {
			value := testGitHubDiff
			if args[0] == "api" {
				value = ""
			}
			return &value
		}
		if err := runOnce(t.Context(), deps); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(state.out.String(), `"touchedFiles":["src/foo.ts"]`) {
			t.Fatalf("output %s", state.out.String())
		}
	})
	t.Run("files endpoint capped (partial) → union recovers files only in the diff", func(t *testing.T) {
		diff := strings.ReplaceAll(testGitHubDiff, "src/foo.ts", "late.ts")
		deps, state := newTestDeps([]string{"305", "owner/repo"})
		state.gh = func(args []string) *string {
			value := diff
			if args[0] == "api" {
				value = "src/foo.ts\n"
			}
			return &value
		}
		if err := runOnce(t.Context(), deps); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(state.out.String(), `"touchedFiles":["src/foo.ts","late.ts"]`) {
			t.Fatalf("output %s", state.out.String())
		}
	})
}
func TestRunOnceGitHubEdgeCases(t *testing.T) {
	t.Run("bad args (unknown option) → exit 2, err names it, gh never called", func(t *testing.T) {
		deps, state := newTestDeps([]string{"305", "--bogus"})
		var status exitCodeError
		if err := runOnce(t.Context(), deps); !errors.As(err, &status) || status.code != 2 {
			t.Fatalf("error = %v", err)
		}
		if !strings.Contains(state.err.String(), "unknown option") || len(state.ghCalls) != 0 {
			t.Fatalf("stderr %q, calls %#v", state.err.String(), state.ghCalls)
		}
	})
	t.Run("bad args (dash pr, no msg) → exit 2, default usage banner", func(t *testing.T) {
		deps, state := newTestDeps([]string{"-5"})
		var status exitCodeError
		if err := runOnce(t.Context(), deps); !errors.As(err, &status) || status.code != 2 || !strings.Contains(state.err.String(), "usage") {
			t.Fatalf("error=%v stderr=%q", err, state.err.String())
		}
	})
	t.Run("pagination completeness: every gh-drained file survives, even one absent from the diff", func(t *testing.T) {
		deps, state := newTestDeps([]string{"305", "owner/repo"})
		state.gh = func(args []string) *string {
			value := testGitHubDiff
			if args[0] == "api" {
				names := make([]string, 0, 150)
				for index := range 150 {
					names = append(names, fmt.Sprintf("pkg/f%d.ts", index))
				}
				value = strings.Join(names, "\n") + "\n"
			}
			return &value
		}
		if err := runOnce(t.Context(), deps); err != nil {
			t.Fatal(err)
		}
		for index := range 150 {
			if !strings.Contains(state.out.String(), fmt.Sprintf(`"pkg/f%d.ts"`, index)) {
				t.Fatalf("missing f%d", index)
			}
		}
		if !strings.Contains(state.out.String(), `"src/foo.ts"`) {
			t.Fatal("diff-derived path missing")
		}
	})
	t.Run("files call fails (null) → fail closed, exit 1, no JSON emitted", func(t *testing.T) {
		deps, state := newTestDeps([]string{"305", "owner/repo"})
		state.gh = func(args []string) *string {
			if args[0] == "api" {
				return nil
			}
			value := testGitHubDiff
			return &value
		}
		var status exitCodeError
		if err := runOnce(t.Context(), deps); !errors.As(err, &status) || status.code != 1 || state.out.Len() != 0 || !strings.Contains(state.err.String(), "incomplete file list") {
			t.Fatalf("error=%v output=%q stderr=%q", err, state.out.String(), state.err.String())
		}
	})
	t.Run("diff call fails (null) → no diff resolved, exit 1, no JSON", func(t *testing.T) {
		deps, state := newTestDeps([]string{"305", "owner/repo"})
		state.gh = func([]string) *string { return nil }
		var status exitCodeError
		if err := runOnce(t.Context(), deps); !errors.As(err, &status) || status.code != 1 || state.out.Len() != 0 || !strings.Contains(state.err.String(), "no diff resolved") {
			t.Fatalf("error=%v output=%q stderr=%q", err, state.out.String(), state.err.String())
		}
	})
}

func TestRunOnceForgejo(t *testing.T) {
	t.Run("no FORGEJO_BASE_URL → exit 1, err says so, no http call", func(t *testing.T) {
		deps, state := newTestDeps([]string{"305", "owner/repo", "--forge", "forgejo"})
		requireExitCode(t, runOnce(t.Context(), deps))
		if !strings.Contains(state.err.String(), "FORGEJO_BASE_URL") || len(state.httpCalls) != 0 {
			t.Fatalf("stderr=%q requests=%d", state.err.String(), len(state.httpCalls))
		}
	})
	t.Run("base URL but no repo → exit 1, err demands owner/repo", func(t *testing.T) {
		deps, state := newTestDeps([]string{"305", "--forge", "forgejo"})
		state.env["FORGEJO_BASE_URL"] = "https://forge.example.com"
		requireExitCode(t, runOnce(t.Context(), deps))
		if !strings.Contains(state.err.String(), "owner/repo") || len(state.httpCalls) != 0 {
			t.Fatalf("stderr=%q requests=%d", state.err.String(), len(state.httpCalls))
		}
	})
	t.Run("happy path → drains files, built requests fetched, JSON out, exit 0", func(t *testing.T) {
		deps, state := newTestDeps([]string{"305", "owner/repo", "--forge=forgejo"})
		state.env["FORGEJO_BASE_URL"] = "https://forge.example.com/"
		state.env["FORGEJO_TOKEN"] = "tok"
		state.httpGet = func(request forgejoRequest) string {
			if strings.HasSuffix(request.URL, ".diff") {
				return forgeDiff
			}
			if strings.Contains(request.URL, "page=1") {
				return `[{"filename":"a.ts"}]`
			}
			return "[]"
		}
		if err := runOnce(t.Context(), deps); err != nil {
			t.Fatal(err)
		}
		assertForgejoHappyPath(t, state)
	})
}

const forgeDiff = "diff --git a/a.ts b/a.ts\n--- a/a.ts\n+++ b/a.ts\n@@ -1 +1 @@\n-x\n+y"

func assertForgejoHappyPath(t *testing.T, state *captured) {
	t.Helper()
	if len(state.httpCalls) != 3 || state.httpCalls[0].URL != "https://forge.example.com/api/v1/repos/owner/repo/pulls/305.diff" || state.httpCalls[0].Headers["Authorization"] != "token tok" || state.httpCalls[1].URL != "https://forge.example.com/api/v1/repos/owner/repo/pulls/305/files?page=1&limit=50" || state.httpCalls[1].Headers["Authorization"] != "token tok" || state.httpCalls[2].URL != "https://forge.example.com/api/v1/repos/owner/repo/pulls/305/files?page=2&limit=50" {
		t.Fatalf("requests %#v", state.httpCalls)
	}
	if !strings.Contains(state.out.String(), `"diff":"diff --git`) || !strings.Contains(state.out.String(), `"touchedFiles":["a.ts"]`) {
		t.Fatalf("output %s", state.out.String())
	}
}

func TestForgejoMultiPageDrain(t *testing.T) {
	deps, state := forgejoTestDeps(t)
	state.httpGet = func(request forgejoRequest) string {
		page := forgejoPage(request.URL)
		if page == 0 {
			return forgeDiff
		}
		if page == 1 {
			items := make([]string, 50)
			for index := range items {
				items[index] = fmt.Sprintf(`{"filename":"f%d.ts"}`, index+1)
			}
			return "[" + strings.Join(items, ",") + "]"
		}
		if page == 2 {
			return `[{"filename":"f51.ts"},{"filename":"logo.png"}]`
		}
		return "[]"
	}
	if err := runOnce(t.Context(), deps); err != nil {
		t.Fatal(err)
	}
	for index := 1; index <= 51; index++ {
		if !strings.Contains(state.out.String(), fmt.Sprintf("f%d.ts", index)) {
			t.Fatalf("missing f%d", index)
		}
	}
	if !strings.Contains(state.out.String(), "logo.png") || len(state.httpCalls) != 4 {
		t.Fatalf("requests=%#v output=%s", state.httpCalls, state.out.String())
	}
}

func TestForgejoEmptyFirstPage(t *testing.T) {
	deps, state := forgejoTestDeps(t)
	if err := runOnce(t.Context(), deps); err != nil {
		t.Fatal(err)
	}
	if len(state.httpCalls) != 2 || !strings.Contains(state.out.String(), `"touchedFiles":["a.ts"]`) {
		t.Fatalf("requests=%d output=%s", len(state.httpCalls), state.out.String())
	}
}

func TestForgejoPageTwoFailure(t *testing.T) {
	deps, state := forgejoTestDeps(t)
	state.httpGet = func(request forgejoRequest) string {
		if forgejoPage(request.URL) == 0 {
			return forgeDiff
		}
		if forgejoPage(request.URL) == 1 {
			files := make([]string, 50)
			for index := range files {
				files[index] = fmt.Sprintf(`{"filename":"f%d.ts"}`, index)
			}
			return "[" + strings.Join(files, ",") + "]"
		}
		return ""
	}
	requireExitCode(t, runOnce(t.Context(), deps))
	if state.out.Len() != 0 {
		t.Fatalf("unexpected output %q", state.out.String())
	}
}

func TestForgejoInvalidFilesPages(t *testing.T) {
	for _, test := range []struct{ name, body string }{
		{"malformed (unparseable) files page → fail closed, exit 1, no JSON", "{not json"},
		{"non-array JSON files page → fail closed, exit 1, no JSON", "{}"},
	} {
		t.Run(test.name, func(t *testing.T) {
			deps, state := forgejoTestDeps(t)
			state.httpGet = func(request forgejoRequest) string {
				if forgejoPage(request.URL) == 0 {
					return forgeDiff
				}
				return test.body
			}
			requireExitCode(t, runOnce(t.Context(), deps))
			if state.out.Len() != 0 {
				t.Fatalf("unexpected output %q", state.out.String())
			}
		})
	}
}

func TestForgejoPaginationBound(t *testing.T) {
	deps, state := forgejoTestDeps(t)
	state.httpGet = func(request forgejoRequest) string {
		if forgejoPage(request.URL) == 0 {
			return forgeDiff
		}
		return `[{"filename":"x.ts"}]`
	}
	requireExitCode(t, runOnce(t.Context(), deps))
	if state.out.Len() != 0 || len(state.httpCalls) != maxForgejoPages+1 {
		t.Fatalf("requests=%d output=%q", len(state.httpCalls), state.out.String())
	}
}

func forgejoTestDeps(t *testing.T) (deps, *captured) {
	t.Helper()
	deps, state := newTestDeps([]string{"305", "owner/repo", "--forge", "forgejo"})
	state.env["FORGEJO_BASE_URL"] = "https://forge.example.com"
	state.httpGet = func(request forgejoRequest) string {
		if forgejoPage(request.URL) == 0 {
			return forgeDiff
		}
		return "[]"
	}
	return deps, state
}

func forgejoPage(requestURL string) int {
	query, err := url.Parse(requestURL)
	if err != nil {
		return 0
	}
	page, _ := strconv.Atoi(query.Query().Get("page"))
	return page
}

func TestRunAndOutputFailures(t *testing.T) {
	deps, _ := newTestDeps([]string{"1"})
	deps.Out = failingWriter{}
	deps.GH = func(_ context.Context, args []string) (*string, error) {
		value := testGitHubDiff
		if args[0] == "api" {
			value = ""
		}
		return &value, nil
	}
	if err := runOnce(t.Context(), deps); err == nil || !strings.Contains(err.Error(), "writing result") {
		t.Fatalf("error = %v", err)
	}
}

func TestRootCommandHelpDocumentsEnvAndExitCodes(t *testing.T) {
	command := newRootCommand(t.Context(), func(string) string { return "" }, io.Discard, io.Discard)
	for _, phrase := range []string{"FORGEJO_BASE_URL", "FORGEJO_TOKEN", "Exit 0", "1 on fetch failure", "2 on usage error"} {
		if !strings.Contains(command.Long, phrase) {
			t.Errorf("help text missing %q", phrase)
		}
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
