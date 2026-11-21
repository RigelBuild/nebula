package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/samber/oops"
)

const originRemote = "origin"

type repository struct {
	root, codename, stateDir string
	profile                  Profile
	keys                     issueKeys
	log                      io.Writer
	landedParents            map[string]string
	submittedBookmark        string
	env                      []string
	lookupLinearPriority     func(context.Context, string) (int, string, bool, error)
	// rootChecks runs the named root-checks in the head checkout. Nil uses the real gate; tests replace it.
	rootChecks func(context.Context, string, []string) (rootChecksResult, error)
	// pushedHeadWait spaces PR head re-reads after a push. Nil means waitFor; tests replace it.
	pushedHeadWait func(context.Context, time.Duration) error
}

// envLookup reads key from the repository overlay, falling back to the process environment.
func envLookup(overlay []string, key string) string {
	for _, entry := range slices.Backward(overlay) {
		if value, ok := strings.CutPrefix(entry, key+"="); ok {
			return value
		}
	}
	return os.Getenv(key)
}

type Change struct {
	Bookmark, Tip, Base string
	Issue               int
	Owned               bool
}

func childEnvironment(extra []string) []string {
	env := slices.DeleteFunc(os.Environ(), func(value string) bool {
		key, _, _ := strings.Cut(value, "=")
		if key == "GH_TOKEN" || key == "GITHUB_TOKEN" {
			return true
		}
		for _, override := range extra {
			other, _, _ := strings.Cut(override, "=")
			if key == other {
				return true
			}
		}
		return false
	})
	return append(env, extra...)
}

func (r *repository) command(ctx context.Context, env []string, input []byte, name string, args ...string) ([]byte, error) {
	cmd := r.execCommand(ctx, name, args...)
	cmd.Dir = r.root
	cmd.Env = r.childEnv(env)
	cmd.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if r.log != nil && name != "gh" {
		if _, writeErr := fmt.Fprintf(r.log, "> %s %q\n%s%s", name, args, stdout.Bytes(), stderr.Bytes()); writeErr != nil {
			return nil, oops.Wrapf(errors.Join(err, writeErr), "write command log")
		}
	}
	if err != nil {
		return stdout.Bytes(), toolFailure(name, oops.With("command", name, "argv", args, "stderr", stderr.String()).Wrapf(err, "%s failed: %s", name, strings.TrimSpace(stderr.String())))
	}
	return bytes.TrimSpace(stdout.Bytes()), nil
}

func (r *repository) jj(ctx context.Context, args ...string) (string, error) {
	args = append([]string{"--no-pager"}, args...)
	output, err := r.command(ctx, nil, nil, "jj", args...)
	return string(output), err
}

func (r *repository) fetchMain(ctx context.Context) error {
	if r.profile.Posture == forkTracking {
		if _, err := r.jj(ctx, "git", "fetch", "--remote", originRemote); err != nil {
			return err
		}
		return r.checkTrunk(ctx)
	}
	// Check origin HEAD before fetching, so a wrong default branch refuses with nothing moved.
	out, err := r.command(ctx, nil, nil, "git", "ls-remote", "--symref", originRemote, "HEAD")
	if err != nil {
		return refusal("preflight", "default-branch", "cannot determine origin's default branch", "check origin and retry")
	}
	if !strings.HasPrefix(string(out), "ref: refs/heads/"+r.profile.Trunk+"\tHEAD\n") {
		return refusal("preflight", "default-branch", "origin HEAD differs from the configured trunk", "check the repository profile")
	}
	_, err = r.jj(ctx, "git", "fetch", "--remote", originRemote)
	return err
}

func (r *repository) checkTrunk(ctx context.Context) error {
	if r.profile.Posture != forkTracking {
		return nil
	}
	revs, err := r.revisions(ctx, "remote_bookmarks(exact:"+quoteRev(r.profile.Trunk)+", remote=exact:\"origin\")")
	if err != nil {
		return err
	}
	if len(revs) != 1 {
		return refusal("preflight", "fork-profile-incomplete", "configured fork trunk is missing", "fetch "+r.profile.TrunkRemote())
	}
	return nil
}
func quoteRev(value string) string {
	if bookmark, remote, ok := strings.Cut(value, "@"); ok {
		return quoteRev(bookmark) + "@" + quoteRev(remote)
	}
	return `"` + strings.ReplaceAll(strings.ReplaceAll(value, `\`, `\\`), `"`, `\"`) + `"`
}
func (r *repository) revisions(ctx context.Context, rev string) ([]string, error) {
	output, err := r.jj(ctx, "log", "--no-graph", "-r", rev, "-T", `commit_id ++ "\n"`)
	if err != nil {
		return nil, err
	}
	if output == "" {
		return []string{}, nil
	}
	return strings.Split(output, "\n"), nil
}

// changedPaths lists the paths that differ from lower to upper. Renames split into delete and add, so a moved source still counts.
func (r *repository) changedPaths(ctx context.Context, lower, upper string) ([]string, error) {
	out, err := r.command(ctx, nil, nil, "git", "diff", "--name-only", "--no-renames", "-z", lower, upper)
	if err != nil {
		return nil, err
	}
	var paths []string
	for path := range strings.SplitSeq(string(out), "\x00") {
		if path != "" {
			paths = append(paths, path)
		}
	}
	return paths, nil
}
func (r *repository) one(ctx context.Context, rev string) (string, error) {
	revisions, err := r.revisions(ctx, rev)
	if err != nil {
		return "", err
	}
	if len(revisions) != 1 {
		return "", refusal("working-copy", "ambiguous-revision", "expected exactly one revision for "+rev, "select a single linear revision")
	}
	return revisions[0], nil
}
func (r *repository) clean(ctx context.Context) error {
	empty, err := r.jj(ctx, "log", "--no-graph", "-r", "@", "-T", "empty")
	if err != nil {
		return err
	}
	if empty != "true" {
		return refusal("working-copy", "uncommitted-edits", "@ contains edits; save them before submitting", `jj commit -m "<message>"`)
	}
	return nil
}

func (r *repository) freshJJ(ctx context.Context, args ...string) (string, error) {
	value, err := r.jj(ctx, args...)
	if err == nil || !strings.Contains(err.Error(), "stale") {
		return value, err
	}
	if _, err := r.jj(ctx, "workspace", "update-stale"); err != nil {
		return "", err
	}
	return r.jj(ctx, args...)
}

func preflight(ctx context.Context, cfg Config, codename string, log io.Writer) (*repository, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, oops.Wrapf(err, "find home directory")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, oops.Wrapf(err, "find working directory")
	}
	cwd, err = filepath.EvalSymlinks(cwd)
	if err != nil {
		return nil, oops.Wrapf(err, "resolve workspace directory")
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		return nil, oops.Wrapf(err, "resolve home directory")
	}
	r := &repository{root: cwd, log: log}
	root, err := r.jj(ctx, "--at-op=@", "root")
	if err != nil {
		return nil, err
	}
	r.root = root
	agentRoot := filepath.Join(home, "agents", "workspaces") + string(filepath.Separator)
	nemoRoot := filepath.Join(home, cfg.NemoRepoDir) + string(filepath.Separator)
	email := cfg.AgentEmail
	switch {
	case strings.HasPrefix(root, agentRoot):
		inferred, _, _ := strings.Cut(strings.TrimPrefix(root, agentRoot), string(filepath.Separator))
		if codename == "" {
			codename = os.Getenv("RJC_CODENAME")
		}
		if codename == "" {
			codename = inferred
		}
	case strings.HasPrefix(root, nemoRoot):
		email = cfg.NemoEmail
		if codename == "" {
			codename = os.Getenv("RJC_CODENAME")
		}
		if codename == "" {
			codename = "nemo"
		}
	default:
		return nil, refusal("preflight", "write-boundary", "clone is outside an agent workspace", "use a clone under ~/agents/workspaces/<codename>/ or "+cfg.NemoRepoDir)
	}
	r.codename = codename
	configured, err := r.jj(ctx, "--at-op=@", "config", "get", "user.email")
	if err != nil {
		return nil, err
	}
	if configured != email {
		return nil, refusal("preflight", "identity", "jj user.email must be "+email, "jj config set --workspace user.email "+email)
	}
	origin, err := r.jj(ctx, "--ignore-working-copy", "--at-op=@", "git", "remote", "list")
	if err != nil {
		return nil, err
	}
	originURL := ""
	for line := range strings.SplitSeq(origin, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == originRemote {
			originURL = fields[1]
		}
	}
	r.profile, err = ProfileFor(cfg, originURL)
	if err != nil {
		return nil, refusal("preflight", "repository", err.Error(), "use an agent clone of a supported GitHub repository listed in the profile file")
	}
	r.keys, err = newIssueKeys(r.profile.IssuePrefix)
	if err != nil {
		return nil, refusal("preflight", "repository", err.Error(), "fix issuePrefix in the profile file")
	}
	if !r.profile.PR {
		return nil, refusal("preflight", "repository", "repository does not accept PRs (mirror target)", "use a repository that accepts PRs")
	}
	// Reading @ forces freshness checks even when a stale workspace still answers root.
	if _, err := r.freshJJ(ctx, "log", "--no-graph", "-r", "@", "-T", "commit_id"); err != nil {
		return nil, err
	}
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		state = filepath.Join(home, ".local", "state")
	}
	r.stateDir = filepath.Join(state, "rjc")
	return r, nil
}
