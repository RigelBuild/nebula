package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec" //nolint:depguard // The tool invokes gh directly with argv, without a shell.
	"strconv"
	"strings"

	"github.com/RigelBuild/nebula/tools/go-lib/httpretry"
	"github.com/spf13/cobra"
)

const (
	usageText       = "usage: get-pr-diff <pr> [owner/repo | --repo owner/repo] [--forge github|forgejo]"
	forgejoPageSize = 50
	maxForgejoPages = 1000
	pageParameter   = "page="
	limitParameter  = "&limit="
)

var errGHCommandFailed = errors.New("gh command failed")

type Forge string

const (
	ForgeGitHub  Forge = "github"
	ForgeForgejo Forge = "forgejo"
)

type parseResult struct {
	OK    bool
	PR    string
	Repo  string
	Forge Forge
	Msg   string
}

type forgejoRequest struct {
	URL     string
	Headers map[string]string
}

type deps struct {
	Argv    []string
	Env     func(string) string
	Out     io.Writer
	Err     io.Writer
	GH      func(context.Context, []string) (*string, error)
	HTTPGet func(context.Context, forgejoRequest) string
}

func run(ctx context.Context, getenv func(string) string, stdout, stderr io.Writer) int {
	command := newRootCommand(ctx, getenv, stdout, stderr)
	if err := command.Execute(); err != nil {
		statusError, ok := errors.AsType[exitCodeError](err)
		if ok {
			return statusError.code
		}
		slog.New(slog.NewTextHandler(stderr, nil)).Error("get-pr-diff failed", "error", err)
		return 1
	}
	return 0
}

func main() {
	os.Exit(run(context.Background(), os.Getenv, os.Stdout, os.Stderr))
}
func newRootCommand(ctx context.Context, getenv func(string) string, stdout, stderr io.Writer) *cobra.Command {
	command := &cobra.Command{
		Use:                "get-pr-diff <pr> [owner/repo | --repo owner/repo] [--forge github|forgejo]",
		Short:              "Fetch a pull request diff and touched-file list",
		Long:               "Fetches a pull request's unified diff and complete touched-file list from GitHub or Forgejo.\n\nEnv: FORGEJO_BASE_URL (required for Forgejo), FORGEJO_TOKEN (optional for public repos).\nExit 0 on success, 1 on fetch failure, 2 on usage error.",
		SilenceErrors:      true,
		SilenceUsage:       true,
		DisableFlagParsing: true,
		RunE: func(_ *cobra.Command, args []string) error {
			deps := deps{
				Argv:    args,
				Env:     getenv,
				Out:     stdout,
				Err:     stderr,
				GH:      realGH,
				HTTPGet: realHTTPGet,
			}
			return runOnce(ctx, deps)
		},
	}
	command.CompletionOptions.DisableDefaultCmd = true
	command.SetOut(stdout)
	command.SetErr(stderr)
	return command
}

func parseArgs(argv []string) parseResult {
	if len(argv) == 0 || argv[0] == "" || strings.HasPrefix(argv[0], "-") {
		return parseResult{}
	}
	result := parseResult{OK: true, PR: argv[0], Forge: ForgeGitHub}
	for index := 1; index < len(argv); {
		token := argv[index]
		switch {
		case token == "--repo":
			value := ""
			if index+1 < len(argv) {
				value = argv[index+1]
			}
			if value == "" || strings.HasPrefix(value, "-") {
				return parseResult{Msg: "get-pr-diff: --repo needs an owner/repo value"}
			}
			result.Repo = value
			index += 2
		case strings.HasPrefix(token, "--repo="):
			result.Repo = strings.TrimPrefix(token, "--repo=")
			if result.Repo == "" {
				return parseResult{Msg: "get-pr-diff: --repo needs an owner/repo value"}
			}
			index++
		case token == "--forge":
			value := ""
			if index+1 < len(argv) {
				value = argv[index+1]
			}
			if !isForge(value) {
				return parseResult{Msg: "get-pr-diff: --forge must be github or forgejo"}
			}
			result.Forge = Forge(value)
			index += 2
		case strings.HasPrefix(token, "--forge="):
			value := strings.TrimPrefix(token, "--forge=")
			if !isForge(value) {
				return parseResult{Msg: "get-pr-diff: --forge must be github or forgejo"}
			}
			result.Forge = Forge(value)
			index++
		case strings.HasPrefix(token, "-"):
			return parseResult{Msg: fmt.Sprintf("get-pr-diff: unknown option '%s'", token)}
		case result.Repo == "":
			result.Repo = token
			index++
		default:
			return parseResult{Msg: fmt.Sprintf("get-pr-diff: unexpected argument '%s'", token)}
		}
	}
	return result
}

func isForge(value string) bool {
	return value == string(ForgeGitHub) || value == string(ForgeForgejo)
}

func extractTouchedFiles(filesJSON any) []string {
	items, ok := filesJSON.([]any)
	if !ok {
		return []string{}
	}
	out := []string{}
	for _, item := range items {
		record, ok := item.(map[string]any)
		if !ok {
			continue
		}
		path, ok := record["path"].(string)
		if !ok {
			path, ok = record["filename"].(string)
		}
		if ok && path != "" {
			out = append(out, path)
		}
	}
	return out
}

func parseFileLines(text string) []string {
	out := []string{}
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func buildGitHubDiffArgs(pr, repo string) []string {
	args := []string{"pr", "diff", pr}
	if repo != "" {
		args = append(args, "--repo", repo)
	}
	return args
}

func buildGitHubFilesArgs(pr, repo string) []string {
	if repo == "" {
		repo = "{owner}/{repo}"
	}
	return []string{"api", "repos/" + repo + "/pulls/" + pr + "/files", "--paginate", "--jq", ".[].filename"}
}

func encodeURIComponent(value string) string {
	encoded := url.QueryEscape(value)
	encoded = strings.ReplaceAll(encoded, "+", "%20")
	for _, character := range []struct{ escaped, literal string }{
		{"%21", "!"}, {"%2A", "*"}, {"%27", "'"}, {"%28", "("}, {"%29", ")"},
	} {
		encoded = strings.ReplaceAll(encoded, character.escaped, character.literal)
		encoded = strings.ReplaceAll(encoded, strings.ToLower(character.escaped), character.literal)
	}
	return encoded
}

func forgejoRepoPath(repo string) string {
	segments := strings.Split(repo, "/")
	for index, segment := range segments {
		segments[index] = encodeURIComponent(segment)
	}
	return strings.Join(segments, "/")
}

func buildForgejoDiffRequest(base, repo, pr, token string) forgejoRequest {
	base = strings.TrimRight(base, "/")
	return forgejoRequest{
		URL:     base + "/api/v1/repos/" + forgejoRepoPath(repo) + "/pulls/" + encodeURIComponent(pr) + ".diff",
		Headers: authorizationHeaders(token),
	}
}

func buildForgejoFilesRequest(base, repo, pr, token string, page, limit int) forgejoRequest {
	base = strings.TrimRight(base, "/")
	query := pageParameter + strconv.Itoa(page) + limitParameter + strconv.Itoa(limit)
	return forgejoRequest{
		URL:     fmt.Sprintf("%s/api/v1/repos/%s/pulls/%s/files?%s", base, forgejoRepoPath(repo), encodeURIComponent(pr), query),
		Headers: authorizationHeaders(token),
	}
}

func authorizationHeaders(token string) map[string]string {
	headers := map[string]string{}
	if token != "" {
		headers["Authorization"] = "token " + token
	}
	return headers
}

func safeJSON(text string) any {
	var parsed any
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		return nil
	}
	return parsed
}

func fetchForgejoFiles(ctx context.Context, deps deps, base, repo, pr, token string) ([]string, bool) {
	files := []string{}
	for page := 1; page <= maxForgejoPages; page++ {
		body := deps.HTTPGet(ctx, buildForgejoFilesRequest(base, repo, pr, token, page, forgejoPageSize))
		parsed := safeJSON(body)
		items, ok := parsed.([]any)
		if !ok {
			return nil, false
		}
		if len(items) == 0 {
			return files, true
		}
		files = append(files, extractTouchedFiles(items)...)
	}
	return nil, false
}

func runOnce(ctx context.Context, deps deps) error {
	parsed := parseArgs(deps.Argv)
	if !parsed.OK {
		message := parsed.Msg
		if message == "" {
			message = usageText
		}
		if _, err := fmt.Fprintln(deps.Err, message); err != nil {
			return fmt.Errorf("writing error output: %w", err)
		}
		return exitCodeError{code: 2}
	}

	var diff string
	var touchedFiles []string
	var complete bool
	var err error
	if parsed.Forge == ForgeGitHub {
		diff, touchedFiles, complete, err = runGitHubOnce(ctx, deps, parsed)
	} else {
		diff, touchedFiles, complete, err = runForgejoOnce(ctx, deps, parsed)
	}
	if err != nil {
		return err
	}
	if strings.TrimSpace(diff) == "" {
		return emitFailure(deps.Err, fmt.Sprintf("get-pr-diff: no diff resolved for PR %s%s", parsed.PR, repoDetail(parsed.Repo)))
	}
	if !complete {
		return emitFailure(deps.Err, fmt.Sprintf("get-pr-diff: incomplete file list for PR %s%s — the file-list fetch failed; refusing to emit a partial list", parsed.PR, repoDetail(parsed.Repo)))
	}

	touchedFiles = unionFiles(touchedFiles, touchedFilesFromDiff(diff))
	payload, err := json.Marshal(struct {
		Diff         string   `json:"diff"`
		TouchedFiles []string `json:"touchedFiles"`
	}{Diff: diff, TouchedFiles: touchedFiles})
	if err != nil {
		return fmt.Errorf("encoding result: %w", err)
	}
	if _, err := fmt.Fprintln(deps.Out, string(payload)); err != nil {
		return fmt.Errorf("writing result: %w", err)
	}
	return nil
}

func runGitHubOnce(ctx context.Context, deps deps, parsed parseResult) (string, []string, bool, error) {
	diff := ""
	githubDiff, err := deps.GH(ctx, buildGitHubDiffArgs(parsed.PR, parsed.Repo))
	if err != nil {
		return "", nil, false, emitFailure(deps.Err, fmt.Sprintf("get-pr-diff: no diff resolved for PR %s%s", parsed.PR, repoDetail(parsed.Repo)))
	}
	if githubDiff != nil {
		diff = *githubDiff
	}
	filesRaw, err := deps.GH(ctx, buildGitHubFilesArgs(parsed.PR, parsed.Repo))
	if err != nil {
		return "", nil, false, emitFailure(deps.Err, fmt.Sprintf("get-pr-diff: incomplete file list for PR %s%s; the file-list fetch failed; refusing to emit a partial list", parsed.PR, repoDetail(parsed.Repo)))
	}
	if filesRaw == nil {
		return diff, nil, false, nil
	}
	return diff, parseFileLines(*filesRaw), true, nil
}

func runForgejoOnce(ctx context.Context, deps deps, parsed parseResult) (string, []string, bool, error) {
	base := deps.Env("FORGEJO_BASE_URL")
	if base == "" {
		return "", nil, false, emitFailure(deps.Err, "get-pr-diff: --forge forgejo needs FORGEJO_BASE_URL")
	}
	if parsed.Repo == "" {
		return "", nil, false, emitFailure(deps.Err, "get-pr-diff: --forge forgejo needs an owner/repo (positional or --repo)")
	}
	token := deps.Env("FORGEJO_TOKEN")
	diff := deps.HTTPGet(ctx, buildForgejoDiffRequest(base, parsed.Repo, parsed.PR, token))
	files, complete := fetchForgejoFiles(ctx, deps, base, parsed.Repo, parsed.PR, token)
	return diff, files, complete, nil
}

func repoDetail(repo string) string {
	if repo == "" {
		return ""
	}
	return " in " + repo
}

type exitCodeError struct {
	code int
}

func (e exitCodeError) Error() string {
	return fmt.Sprintf("command exited with status %d", e.code)
}

func emitFailure(destination io.Writer, message string) error {
	if _, err := fmt.Fprintln(destination, message); err != nil {
		return fmt.Errorf("writing error output: %w", err)
	}
	return exitCodeError{code: 1}
}

func unionFiles(groups ...[]string) []string {
	seen := make(map[string]struct{})
	out := []string{}
	for _, group := range groups {
		for _, file := range group {
			if _, ok := seen[file]; ok {
				continue
			}
			seen[file] = struct{}{}
			out = append(out, file)
		}
	}
	return out
}

func touchedFilesFromDiff(diff string) []string {
	out := []string{}
	oldPath := ""
	for line := range strings.SplitSeq(diff, "\n") {
		if target, ok := strings.CutPrefix(line, "--- "); ok {
			target = strings.TrimSpace(target)
			if target == "/dev/null" {
				oldPath = ""
			} else {
				oldPath, _ = strings.CutPrefix(target, "a/")
			}
			continue
		}
		target, ok := strings.CutPrefix(line, "+++ ")
		if !ok {
			continue
		}
		target = strings.TrimSpace(target)
		if target == "/dev/null" {
			if oldPath != "" {
				out = append(out, oldPath)
			}
			continue
		}
		path, _ := strings.CutPrefix(target, "b/")
		out = append(out, path)
	}
	return unionFiles(out)
}

func realGH(ctx context.Context, args []string) (*string, error) {
	command := exec.CommandContext(ctx, "gh", args...) //nolint:gosec // The executable is fixed; argv values are passed without a shell.
	output, err := command.Output()
	if err != nil {
		if _, ok := errors.AsType[*exec.ExitError](err); ok {
			return nil, errGHCommandFailed
		}
		return nil, fmt.Errorf("executing gh: %w", err)
	}
	result := string(output)
	return &result, nil

}

func realHTTPGet(ctx context.Context, request forgejoRequest) string {
	header := make(http.Header, len(request.Headers))
	for key, value := range request.Headers {
		header.Set(key, value)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, request.URL, nil)
	if err != nil {
		slog.Warn("creating Forgejo request", "error", err)
		return ""
	}
	httpRequest.Header = header
	response, err := httpretry.NewClient(http.DefaultClient).Do(httpRequest)
	if err != nil {
		slog.Warn("fetching Forgejo response", "error", err)
		return ""
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			slog.Warn("closing Forgejo response body", "error", err)
		}
	}()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return ""
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		slog.Warn("reading Forgejo response body", "error", err)
		return ""
	}
	return string(body)
}
