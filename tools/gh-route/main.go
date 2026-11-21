package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec" //nolint:depguard // The tool invokes gh directly with argv, without a shell.
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	apiCommand            = "api"
	graphqlCommand        = "graphql"
	headSHACommand        = "head-sha"
	reviewsCommand        = "reviews"
	commentsCommand       = "comments"
	reviewCommentsCommand = "review-comments"
	checkRunsCommand      = "check-runs"
	prListCommand         = "pr-list"
)

func nonnegativeResetWait(reset, now int64) int64 {
	duration := reset - now + 1
	if duration < 1 {
		return 1
	}
	return duration
}

type ghResult struct {
	Stdout   string
	ExitCode int
}

type routeDeps struct {
	Env        func(string) (string, bool)
	GH         func(context.Context, []string) ghResult
	Log        func(string) error
	Err        func(string) error
	Now        func() int64
	Sleep      func(context.Context, time.Duration) error
	ReadCache  func() (string, bool)
	WriteCache func(string) error
}

func main() {
	os.Exit(run(context.Background(), os.Getenv, os.Stdout, os.Stderr))
}

func run(ctx context.Context, getenv func(string) string, stdout, stderr io.Writer) int {
	command := newRootCommand(ctx, getenv, stdout, stderr)
	if err := command.Execute(); err != nil {
		statusError, ok := errors.AsType[commandExitError](err)
		if ok {
			return statusError.code
		}
		slog.New(slog.NewTextHandler(stderr, nil)).Error("gh-route failed", "error", err)
		return 1
	}
	return 0
}
func newRootCommand(ctx context.Context, getenv func(string) string, stdout, stderr io.Writer) *cobra.Command {
	command := &cobra.Command{
		Use:                "gh-route <cmd> [args]",
		Short:              "Route GitHub reads through the API bucket with more headroom",
		Long:               helpText,
		SilenceErrors:      true,
		SilenceUsage:       true,
		DisableFlagParsing: true,
		RunE: func(_ *cobra.Command, args []string) error {
			dependencies := realDependencies(getenv, stdout, stderr)
			code, err := runMain(ctx, dependencies, args)
			if err != nil {
				return err
			}
			if code != 0 {
				return commandExitError{code: code}
			}
			return nil
		},
	}
	command.CompletionOptions.DisableDefaultCmd = true
	command.SetOut(stdout)
	command.SetErr(stderr)
	return command
}

type commandExitError struct {
	code int
}

func (e commandExitError) Error() string {
	return fmt.Sprintf("command exited with status %d", e.code)
}

func realDependencies(getenv func(string) string, stdout, stderr io.Writer) routeDeps {
	cachePath := getenv("GH_ROUTE_CACHE")
	if cachePath == "" {
		tmpDir := getenv("TMPDIR")
		if tmpDir == "" {
			tmpDir = "/tmp"
		}
		cachePath = filepath.Join(tmpDir, "gh-route-rl."+strconv.Itoa(os.Getuid())+".json")
	}
	return routeDeps{
		Env: func(key string) (string, bool) {
			value := getenv(key)
			return value, value != ""
		},
		GH: func(ctx context.Context, args []string) ghResult {
			command := exec.CommandContext(ctx, "gh", args...) //nolint:gosec // The executable is fixed; argv values are passed without a shell.
			output, err := command.Output()
			if err == nil {
				return ghResult{Stdout: string(output)}
			}
			if exitError, ok := errors.AsType[*exec.ExitError](err); ok {
				return ghResult{Stdout: string(exitError.Stderr), ExitCode: exitError.ExitCode()}
			}
			return ghResult{ExitCode: 1}
		},
		Log: func(message string) error {
			_, err := fmt.Fprintln(stdout, message)
			return err
		},
		Err: func(message string) error {
			_, err := fmt.Fprintln(stderr, message)
			return err
		},
		Now: func() int64 { return time.Now().Unix() },
		Sleep: func(ctx context.Context, duration time.Duration) error {
			timer := time.NewTimer(duration)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		},
		ReadCache: func() (string, bool) {
			content, err := os.ReadFile(cachePath) //nolint:gosec // GH_ROUTE_CACHE names the intended local file.
			if err != nil {
				return "", false
			}
			return string(content), len(content) > 0
		},
		WriteCache: func(content string) error {
			if err := os.WriteFile(cachePath, []byte(content), 0o600); err != nil {
				slog.Warn("writing rate-limit cache", "error", err, "path", cachePath)
			}
			return nil
		},
	}
}

func rateLimit(ctx context.Context, deps routeDeps, config Config, forceRefresh bool) RateLimit {
	now := deps.Now()
	cached, cacheExists := deps.ReadCache()
	cachedAt := int64(0)
	if cacheExists && !forceRefresh {
		var parsed RateLimit
		if err := json.Unmarshal([]byte(cached), &parsed); err == nil {
			cachedAt = parsed.FetchedAt
		}
	}
	if forceRefresh || now-cachedAt >= config.CacheTTL {
		refreshRateLimit(ctx, deps, now)
	}
	finalRaw, ok := deps.ReadCache()
	if !ok {
		return RateLimit{}
	}
	var result RateLimit
	if err := json.Unmarshal([]byte(finalRaw), &result); err != nil {
		return RateLimit{}
	}
	return result
}

func refreshRateLimit(ctx context.Context, deps routeDeps, now int64) {
	result := deps.GH(ctx, []string{apiCommand, "rate_limit"})
	if result.ExitCode != 0 || strings.TrimSpace(result.Stdout) == "" {
		return
	}
	parsed, err := parseRateLimit(result.Stdout)
	if err != nil {
		return
	}
	parsed.FetchedAt = now
	encoded, err := json.Marshal(parsed)
	if err != nil {
		return
	}
	if err := deps.WriteCache(string(encoded)); err != nil {
		slog.Warn("updating rate-limit cache", "error", err)
	}
}

func awaitHeadroom(ctx context.Context, deps routeDeps, config Config) error {
	var waited int64
	force := false
	for {
		snapshot := rateLimit(ctx, deps, config, force)
		var restRemaining, graphqlRemaining int64
		var resets []int64
		if snapshot.Resources != nil {
			restRemaining = value(bucketRemaining(snapshot.Resources.Core))
			graphqlRemaining = value(bucketRemaining(snapshot.Resources.GraphQL))
			for _, bucket := range []*Bucket{snapshot.Resources.Core, snapshot.Resources.GraphQL} {
				if bucket != nil && bucket.Reset != nil && *bucket.Reset > deps.Now() {
					resets = append(resets, *bucket.Reset)
				}
			}
		}
		if restRemaining > config.Floor || graphqlRemaining > config.Floor {
			return nil
		}
		now := deps.Now()
		resetMin := now + 60
		if len(resets) > 0 {
			resetMin = resets[0]
			for _, reset := range resets[1:] {
				if reset < resetMin {
					resetMin = reset
				}
			}
		}
		sleepSeconds := nonnegativeResetWait(resetMin, now)
		sleepSeconds = min(sleepSeconds, config.MaxWait)
		if waited+sleepSeconds > config.MaxWait {
			return nil
		}
		if err := deps.Err(fmt.Sprintf("gh-route: both buckets < %d (rest=%d gql=%d); waiting %ds for reset", config.Floor, restRemaining, graphqlRemaining, sleepSeconds)); err != nil {
			return fmt.Errorf("writing wait warning: %w", err)
		}
		if err := deps.Sleep(ctx, time.Duration(sleepSeconds)*time.Second); err != nil {
			return fmt.Errorf("waiting for rate-limit reset: %w", err)
		}
		waited += sleepSeconds
		force = true
	}
}

func runGraphQL(ctx context.Context, deps routeDeps, label, query string, fields []string) any {
	args := make([]string, 0, 4+len(fields))
	args = append(args, apiCommand, graphqlCommand, "-f", "query="+query)
	args = append(args, fields...)
	result := deps.GH(ctx, args)
	parsed := safeJSON(result.Stdout)
	if parsed == nil {
		parsed = map[string]any{}
	}
	if hasNextPage(parsed) {
		if err := deps.Err(fmt.Sprintf("gh-route: %s truncated at the 100-item GraphQL page cap; the same read via REST paginates fully — re-run when REST has headroom, or use the MCP for the complete list.", label)); err != nil {
			slog.Warn("writing GraphQL truncation warning", "error", err)
		}
	}
	return parsed
}

func safeJSON(text string) any {
	var parsed any
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		return nil
	}
	return parsed
}

func restPages(ctx context.Context, deps routeDeps, path string) []any {
	result := deps.GH(ctx, []string{apiCommand, "--paginate", path})
	return parseJSONStream(result.Stdout)
}

func runMain(ctx context.Context, deps routeDeps, argv []string) (int, error) {
	config := readConfig(deps.Env)
	command := ""
	args := []string{}
	if len(argv) > 0 {
		command = argv[0]
		args = argv[1:]
	}
	switch command {
	case "pick":
		return logResult(deps, pick(rateLimit(ctx, deps, config, false), config.Floor))
	case "remaining":
		if len(args) == 0 {
			return 1, writeError(deps, "usage: gh-route remaining <rest|graphql>")
		}
		return logResult(deps, strconv.FormatInt(remaining(rateLimit(ctx, deps, config, false), args[0]), 10))
	case headSHACommand, reviewsCommand, commentsCommand, reviewCommentsCommand, checkRunsCommand:
		if len(args) == 0 {
			return 1, writeError(deps, "usage: gh-route "+command+" <arg> [owner/repo]")
		}
		return routeRequest(ctx, deps, config, command, args[0], optionalArg(args, 1))
	case prListCommand:
		return routeRequest(ctx, deps, config, command, "", optionalArg(args, 0))
	case "", "-h", "--help":
		if err := deps.Log(helpText); err != nil {
			return 1, fmt.Errorf("writing help: %w", err)
		}
		if command == "" {
			return 2, nil
		}
		return 0, nil
	default:
		return 2, writeError(deps, fmt.Sprintf("gh-route: unknown command '%s' (try: pick remaining head-sha reviews comments review-comments check-runs pr-list)", command))
	}
}

func logResult(deps routeDeps, output string) (int, error) {
	if err := deps.Log(output); err != nil {
		return 1, fmt.Errorf("writing command output: %w", err)
	}
	return 0, nil
}

func writeError(deps routeDeps, message string) error {
	if err := deps.Err(message); err != nil {
		return fmt.Errorf("writing error output: %w", err)
	}
	return nil
}

func optionalArg(args []string, index int) string {
	if index >= len(args) {
		return ""
	}
	return args[index]
}

func routeRequest(ctx context.Context, deps routeDeps, config Config, command, arg, repoArg string) (int, error) {
	repo := repoArg
	if repo == "" {
		result := deps.GH(ctx, []string{"repo", "view", "--json", "nameWithOwner", "-q", ".nameWithOwner"})
		repo = strings.TrimSpace(result.Stdout)
	}
	split := splitRepo(repo)
	if err := awaitHeadroom(ctx, deps, config); err != nil {
		return 1, err
	}
	route := pick(rateLimit(ctx, deps, config, false), config.Floor)
	if route == graphqlCommand {
		query, fields := graphqlRequest(command, split, arg)
		response := runGraphQL(ctx, deps, command, query, fields)
		return logNormalized(deps, command, response)
	}
	return runREST(ctx, deps, command, arg, repo)
}

func graphqlRequest(command string, repo Repo, arg string) (string, []string) {
	query := ""
	fields := []string{"-F", "o=" + repo.Owner, "-F", "n=" + repo.Name}
	switch command {
	case headSHACommand:
		query = "query($o:String!,$n:String!,$p:Int!){repository(owner:$o,name:$n){pullRequest(number:$p){headRefOid}}}"
		fields = append(fields, "-F", "p="+arg)
	case reviewsCommand:
		query = "query($o:String!,$n:String!,$p:Int!){repository(owner:$o,name:$n){pullRequest(number:$p){reviews(first:100){pageInfo{hasNextPage} nodes{author{login __typename} state submittedAt commit{oid} body}}}}}"
		fields = append(fields, "-F", "p="+arg)
	case commentsCommand:
		query = "query($o:String!,$n:String!,$p:Int!){repository(owner:$o,name:$n){pullRequest(number:$p){comments(first:100){pageInfo{hasNextPage} nodes{author{login __typename} body createdAt updatedAt}}}}}"
		fields = append(fields, "-F", "p="+arg)
	case reviewCommentsCommand:
		query = "query($o:String!,$n:String!,$p:Int!){repository(owner:$o,name:$n){pullRequest(number:$p){reviewThreads(first:100){pageInfo{hasNextPage} nodes{comments(first:100){pageInfo{hasNextPage} nodes{author{login __typename} body path line commit{oid} originalCommit{oid}}}}}}}}"
		fields = append(fields, "-F", "p="+arg)
	case checkRunsCommand:
		query = "query($o:String!,$n:String!,$r:String!){repository(owner:$o,name:$n){object(expression:$r){... on Commit{checkSuites(first:20){pageInfo{hasNextPage} nodes{checkRuns(first:100){pageInfo{hasNextPage} nodes{name status conclusion}}}}}}}}"
		fields = append(fields, "-F", "r="+arg)
	case prListCommand:
		query = "query($o:String!,$n:String!){repository(owner:$o,name:$n){pullRequests(states:OPEN,first:100){pageInfo{hasNextPage} nodes{number title state headRefOid author{login __typename}}}}}"
	}
	return query, fields
}

func logNormalized(deps routeDeps, command string, response any) (int, error) {
	if command == headSHACommand {
		return logResult(deps, headSHAFromGraphQL(response))
	}
	var output any
	switch command {
	case reviewsCommand:
		output = reviewsFromGraphQL(response)
	case commentsCommand:
		output = commentsFromGraphQL(response)
	case reviewCommentsCommand:
		output = reviewCommentsFromGraphQL(response)
	case checkRunsCommand:
		output = checkRunsFromGraphQL(response)
	case prListCommand:
		output = pullRequestsFromGraphQL(response)
	}
	return marshalAndLog(deps, output)
}

func runREST(ctx context.Context, deps routeDeps, command, arg, repo string) (int, error) {
	var output any
	switch command {
	case headSHACommand:
		result := deps.GH(ctx, []string{apiCommand, "repos/" + repo + "/pulls/" + arg})
		return logResult(deps, headSHAFromREST(safeJSON(result.Stdout)))
	case reviewsCommand:
		output = reviewsFromREST(restPages(ctx, deps, "repos/"+repo+"/pulls/"+arg+"/reviews"))
	case commentsCommand:
		output = commentsFromREST(restPages(ctx, deps, "repos/"+repo+"/issues/"+arg+"/comments"))
	case reviewCommentsCommand:
		output = reviewCommentsFromREST(restPages(ctx, deps, "repos/"+repo+"/pulls/"+arg+"/comments"))
	case checkRunsCommand:
		output = checkRunsFromREST(restPages(ctx, deps, "repos/"+repo+"/commits/"+arg+"/check-runs"))
	case prListCommand:
		output = pullRequestsFromREST(restPages(ctx, deps, "repos/"+repo+"/pulls?state=open"))
	}
	return marshalAndLog(deps, output)
}

func marshalAndLog(deps routeDeps, output any) (int, error) {
	encoded, err := json.Marshal(output)
	if err != nil {
		return 1, fmt.Errorf("encoding normalized result: %w", err)
	}
	if err := deps.Log(string(encoded)); err != nil {
		return 1, fmt.Errorf("writing normalized result: %w", err)
	}
	return 0, nil
}

const helpText = "gh-route <cmd> [args] — route a GitHub read to the API bucket (REST `core` vs\nGraphQL) with more headroom, so a fleet of agents sharing one token drains both\n5000/hr buckets evenly instead of exhausting GraphQL while REST sits idle.\n\nEvery read emits the GitHub REST JSON shape regardless of which bucket served\nit, so callers are bucket-agnostic (a drop-in for `gh api repos/O/R/...`).\n`pick` exposes the routing decision on its own, for an agent choosing between\nthis tool (REST) and the github MCP (GraphQL) for an interactive read.\n\nCommands:\n  pick                              → `rest` | `graphql` (higher fractional headroom)\n  remaining <rest|graphql>          → that bucket's remaining request count\n  head-sha        <pr>  [owner/repo] → PR head SHA (bare string)\n  reviews         <pr>  [owner/repo] → REST-shaped review array\n  comments        <pr>  [owner/repo] → REST-shaped issue-comment array\n  review-comments <pr>  [owner/repo] → REST-shaped inline review-comment array\n  check-runs      <ref> [owner/repo] → REST-shaped {check_runs:[...]} for a ref\n  pr-list               [owner/repo] → REST-shaped open-PR array\n\nRouting: reads live remaining/limit for both buckets (`gh api rate_limit`,\nwhich counts against neither bucket, cached GH_ROUTE_CACHE_TTL=15s in a per-uid\ntmp file shared across agents) and sends each read to the bucket with the higher\nremaining/limit fraction — so it flips either way and never merely relocates the\nwall onto REST. If both buckets sit below GH_ROUTE_FLOOR, it waits for the\nnearest reset (capped at GH_ROUTE_MAX_WAIT) rather than spinning against an\nexhausted API, per GitHub's rate-limit guidance.\n\nShape note: emits the fields consumers use (login / commit_id / state / body /\ntimestamps / path / line / conclusion), normalized across both APIs. Inline\nthread state (`is_resolved` / `is_outdated`) and thread node IDs live only on\nGraphQL and are NOT part of this shape — resolve/reply stay on the github MCP.\nThe GraphQL branch caps list fields at 100 (unpaginated); the REST branch\n`--paginate`s, so prefer REST for a PR with >100 reviews/comments.\n\nEnv: GH_ROUTE_FLOOR (200), GH_ROUTE_CACHE_TTL (15), GH_ROUTE_MAX_WAIT (300),\n     GH_ROUTE_CACHE (per-uid tmp path). gh resolves from the dev set.\nExit codes: 0 on success/help, 1 on request or I/O failure, 2 on usage error."
