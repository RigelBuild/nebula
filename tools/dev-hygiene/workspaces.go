package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// unlandedRevset selects workspace commits outside trunk, excluding the empty undescribed working copy.
const unlandedRevset = `((::@) ~ (::trunk()) ~ root()) ~ (@ & empty() & description(exact:""))`

func unlandedRevsetFor(trunk string) string {
	return strings.Replace(unlandedRevset, "trunk()", trunk, 1)
}

func workspaceTrunk(ctx context.Context, dir string) (string, error) {
	var failures []error
	for _, ref := range []string{"trunk()", "main@origin"} {
		result, err := jjQuery(ctx, dir, "log", "--no-graph", "-r", ref+" ~ root()", "-T", `commit_id ++ "\n"`)
		if err != nil {
			failures = append(failures, fmt.Errorf("resolve %s: %w", ref, err))
			continue
		}
		ids := nonEmptyLines(result.stdout)
		if len(ids) == 1 {
			return ids[0], nil
		}
		failures = append(failures, fmt.Errorf("resolve %s: expected one commit, got %d", ref, len(ids)))
	}
	return "", fmt.Errorf("cannot resolve trunk: %w", errors.Join(failures...))
}

type patchResult struct {
	identity [32]byte
	valid    bool
}

type patchCache map[string]patchResult

// findUnlandedChanges compares each non-trunk commit with recent trunk patches after the merge base.
func findUnlandedChanges(ctx context.Context, dir string, cache patchCache) ([]string, error) {
	trunk, err := workspaceTrunk(ctx, dir)
	if err != nil {
		return nil, err
	}
	candidates, err := jjQuery(ctx, dir, "log", "--no-graph", "-r", unlandedRevsetFor(trunk),
		"-T", `commit_id ++ "\t" ++ change_id.short() ++ " " ++ description.first_line() ++ "\n"`)
	if err != nil {
		return nil, fmt.Errorf("list candidate commits: %w", err)
	}
	lines := nonEmptyLines(candidates.stdout)
	if len(lines) == 0 {
		return nil, nil
	}
	revset := fmt.Sprintf("ancestors(%s, 500) ~ ::fork_point(@ | %s) ~ root()", trunk, trunk)
	commits, err := jjQuery(ctx, dir, "log", "--no-graph", "-r", revset, "-T", `commit_id ++ "\n"`)
	if err != nil {
		return nil, fmt.Errorf("list trunk commits: %w", err)
	}
	landed := make(map[[32]byte]struct{})
	for _, commit := range nonEmptyLines(commits.stdout) {
		patch, ok := cache[commit]
		if !ok {
			identity, valid, err := patchIdentity(ctx, dir, commit)
			if err != nil {
				return nil, fmt.Errorf("identify trunk patch %s: %w", commit, err)
			}
			patch = patchResult{identity: identity, valid: valid}
			cache[commit] = patch
		}
		if patch.valid {
			landed[patch.identity] = struct{}{}
		}
	}
	unlanded := make([]string, 0, len(lines))
	for _, candidate := range lines {
		commit, detail, ok := strings.Cut(candidate, "\t")
		if !ok || commit == "" || detail == "" {
			return nil, fmt.Errorf("parse candidate commit %q", candidate)
		}
		identity, valid, err := patchIdentity(ctx, dir, commit)
		if err != nil {
			return nil, fmt.Errorf("identify candidate patch %s: %w", commit, err)
		}
		if _, found := landed[identity]; !valid || !found {
			unlanded = append(unlanded, detail)
		}
	}
	return unlanded, nil
}

// patchIdentity ignores blob IDs but keeps hunk coordinates to avoid equating distinct edits.
func patchIdentity(ctx context.Context, dir, commit string) ([32]byte, bool, error) {
	diff, err := jjQuery(ctx, dir, "diff", "--git", "-r", commit)
	if err != nil {
		return [32]byte{}, false, err
	}
	types, err := jjQuery(ctx, dir, "diff", "--types", "-r", commit)
	if err != nil {
		return [32]byte{}, false, err
	}
	for _, line := range nonEmptyLines(types.stdout) {
		fields := strings.Fields(line)
		if len(fields) == 0 || len(fields[0]) != 2 || strings.Contains(fields[0], "G") {
			return [32]byte{}, false, nil
		}
	}
	var normalized bytes.Buffer
	for _, line := range strings.SplitAfter(diff.stdout, "\n") {
		if strings.HasPrefix(line, "Binary files ") || strings.HasPrefix(line, "GIT binary patch") {
			return [32]byte{}, false, nil
		}
		if strings.HasPrefix(line, "index ") {
			continue
		}
		if _, err := normalized.WriteString(line); err != nil {
			return [32]byte{}, false, fmt.Errorf("normalize diff: %w", err)
		}
	}
	if normalized.Len() == 0 {
		return [32]byte{}, false, nil
	}
	return sha256.Sum256(normalized.Bytes()), true, nil
}

// buildOutputDirs are build trees. A top-level one is excluded outright; a nested one only when no tracked file lives under it.
var buildOutputDirs = []string{"node_modules", "target", "dist"}

// regenerableNames are tool state recreated on demand (devenv, direnv, node_modules at any depth). Matched by base name.
// Files such as .env are deliberately absent: they can hold user work and must not be treated as regenerable.
var regenerableNames = []string{"node_modules", ".devenv", ".direnv"}

// regenerablePaths are top-level tool caches matched by exact relative path (moon's task cache).
var regenerablePaths = []string{".moon/cache"}

const (
	jjDir = ".jj"
	// trashPrefix names a workspace moved aside by an interrupted deletion; the next run reports it without removing it.
	trashPrefix = ".dev-hygiene-trash-"
	// workspaceMarker sits in a workspace being deleted and records its jj workspace name.
	workspaceMarker = ".dev-hygiene-name"
	// snapshotRefused is jj's stderr warning for a new file over snapshot.max-new-file-size. The exit code is 0.
	snapshotRefused = "Refused to snapshot"
)

// kept is a workspace left in place that needs a human decision. line is its DM text; key names it in the reported set.
type kept struct {
	root string
	line string
	key  string
}

// skipTotal sums the workspaces kept in place for one skip reason, and their directory bytes.
type skipTotal struct {
	count int
	bytes int64
}

// skipSummary counts the workspaces kept in place without a verdict, by fixed reason. Raw jj stderr never names a
// reason, so one bucket holds every workspace that failed the same way.
type skipSummary map[string]skipTotal

// add records one skipped workspace of the given reason and directory size.
func (s skipSummary) add(reason string, bytes int64) {
	t := s[reason]
	t.count++
	t.bytes += bytes
	s[reason] = t
}

// Skip reasons. Each is a fixed category, so the summary has one line per reason.
const (
	skipOrphanedStore = "orphaned-store"
	skipDirMissing    = "directory missing"
	skipJJInspect     = "jj inspect failed"
)

// errForgotten means the workspace's store no longer lists it. jj cannot inspect it, but that is not a failure.
var errForgotten = errors.New("store no longer lists workspace")

// cleanWorkspaces classifies every secondary jj workspace under each of cfg.roots and acts on the verdicts.
// live lists the working directories of running processes, read once per run.
func cleanWorkspaces(ctx context.Context, cfg config, live []string, out *printer) ([]kept, error) {
	var dirs, trash []string
	var errs []error
	for _, root := range cfg.roots {
		d, t, skipped, err := workspaceRoots(root)
		// A skipped dir or missing root still fails the run, so a broken host is noticed; the sweep goes on.
		errs = append(errs, skipped...)
		if err != nil && cfg.defaultRoot && errors.Is(err, fs.ErrNotExist) {
			// Only an explicit -root must exist. A host with no default workspaces dir has nothing to sweep.
			out.printf("workspaces root %s: missing, skipped\n", root)
			continue
		}
		// One unreadable root must not stop the sweep of the others.
		if err != nil {
			errs = append(errs, err)
			continue
		}
		dirs = append(dirs, d...)
		trash = append(trash, t...)
	}
	var keptWorkspaces []kept
	skips := make(skipSummary)
	reports := make(skipSummary)
	patches := make(patchCache)
	for _, dir := range trash {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, errors.Join(append(errs, ctxErr)...)
		}
		if !cfg.apply {
			out.printf("leftover %s: would report and keep (dry run)\n", dir)
			continue
		}
		// A leftover trash dir from an interrupted deletion is reported but never removed.
		out.printf("workspace %s: leftover trash, report only\n", dir)
		trashBytes, sizeErr := dirSize(dir)
		reports.add("frozen", trashBytes)
		keptWorkspaces = append(keptWorkspaces, kept{root: dir, line: dir + ": leftover trash (report only)", key: reportKeyOf(dir, nil)})
		if sizeErr != nil {
			errs = append(errs, sizeErr)
		}
	}
	now := time.Now()
	for _, dir := range dirs {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return keptWorkspaces, errors.Join(append(errs, ctxErr)...)
		}
		k, keep, err := sweepWorkspace(ctx, cfg, dir, live, now, skips, reports, out, patches)
		if err != nil {
			errs = append(errs, err)
		}
		if keep {
			keptWorkspaces = append(keptWorkspaces, k)
		}
	}
	for _, reason := range slices.Sorted(maps.Keys(skips)) {
		t := skips[reason]
		out.printf("skip %s: %d workspace(s), %.2f GiB, kept in place\n", reason, t.count, float64(t.bytes)/(1<<30))
	}
	// Forgotten and reclaimable workspaces got a verdict, so they are reports, not skips.
	for _, v := range slices.Sorted(maps.Keys(reports)) {
		t := reports[v]
		out.printf("report %s: %d workspace(s), %.2f GiB, kept in place\n", v, t.count, float64(t.bytes)/(1<<30))
	}
	return keptWorkspaces, errors.Join(errs...)
}

// sweepWorkspace decides one workspace. Cheap, jj-free checks run first; snapshotting jj queries run last,
// and only for an idle workspace with no process inside it. A workspace is kept, never deleted, when its store is
// gone (orphaned-store: no jj call runs on it), when its directory is gone, or when jj could not inspect it. Each
// of those skips is counted in skips under a fixed reason.
func sweepWorkspace(ctx context.Context, cfg config, dir string, live []string, now time.Time, skips, reports skipSummary, out *printer, patches patchCache) (kept, bool, error) {
	resolved, err := filepath.EvalSymlinks(dir)
	if errors.Is(err, fs.ErrNotExist) {
		// The directory went away after the scan. Keep it and report it; the run goes on.
		const reason = "directory missing; jj may still list it"
		out.printf("workspace %s: skipped: %s\n", dir, reason)
		skips.add(skipDirMissing, 0)
		return kept{root: dir, line: dir + ": not inspected (" + reason + ")", key: reportKeyOf(dir, nil)}, true, nil
	}
	if err != nil {
		return kept{}, false, fmt.Errorf("resolve %s: %w", dir, err)
	}
	secondary, err := secondaryWorkspace(resolved)
	if err != nil {
		return kept{}, false, err
	}
	if !secondary {
		// A primary checkout owns the shared store. Forgetting it would be destructive.
		out.printf("workspace %s: primary checkout, never touched\n", resolved)
		return kept{}, false, nil
	}
	store, err := pointerTarget(resolved)
	if err != nil {
		return kept{}, false, err
	}
	missing := ""
	if _, err := os.Stat(store); errors.Is(err, fs.ErrNotExist) {
		missing = store
	}
	if missing != "" {
		// jj cannot judge a workspace whose store is gone, so no jj call runs on it. A tree scan reads its clock.
		// A failed scan leaves the clock unknown: the workspace is kept and skipped, never dropped.
		scan, scanErr := scanTree(resolved, false)
		ws := workspace{Root: resolved, LiveCwd: hasLiveCwd(resolved, live), LastActive: scan.newest, StoreMissing: missing}
		bytes, sizeErr := dirSize(resolved)
		if scanErr == nil && classify(ws, now, cfg.idle) == verdictReclaimable {
			out.printf("workspace %s: %s\n", resolved, verdictReclaimable)
			reports.add(string(verdictReclaimable), bytes)
			return kept{root: resolved, line: resolved + ": " + string(verdictReclaimable) + " (store " + missing + " missing)", key: reportKeyOf(resolved, nil)}, true, errors.Join(sizeErr, scanErr)
		}
		out.printf("workspace %s: skipped: %s, store %s missing, %.2f GiB\n", resolved, skipOrphanedStore, missing, float64(bytes)/(1<<30))
		skips.add(skipOrphanedStore, bytes)
		return kept{root: resolved, line: resolved + ": not inspected (" + skipOrphanedStore + ", store " + missing + " missing)", key: reportKeyOf(resolved, nil)}, true, errors.Join(sizeErr, scanErr)
	}
	if _, err := os.Stat(filepath.Join(resolved, workspaceMarker)); err == nil {
		// A marker means an earlier run began deleting this workspace. Keep it for the report; do not finish it.
		if hasLiveCwd(resolved, live) {
			out.printf("workspace %s: interrupted deletion, skipped while a process has its cwd inside\n", resolved)
			return kept{}, false, nil
		}
		if !cfg.apply {
			out.printf("workspace %s: interrupted deletion, would report and keep (dry run)\n", resolved)
			return kept{}, false, nil
		}
		out.printf("workspace %s: interrupted deletion, report only\n", resolved)
		bytes, sizeErr := dirSize(resolved)
		reports.add("frozen", bytes)
		return kept{root: resolved, line: resolved + ": interrupted deletion (report only)", key: reportKeyOf(resolved, nil)}, true, sizeErr
	} else if !errors.Is(err, fs.ErrNotExist) {
		return kept{}, false, fmt.Errorf("stat marker in %s: %w", resolved, err)
	}
	ws := workspace{Root: resolved, LiveCwd: hasLiveCwd(resolved, live)}
	// The one walk of this tree provides the activity clock and files for the untracked check.
	scan, err := scanTree(resolved, true)
	if err != nil {
		return kept{}, false, fmt.Errorf("activity of %s: %w", resolved, err)
	}
	ws.LastActive = scan.newest
	// Live and recent verdicts need no jj call: a snapshot would itself change the workspace.
	first := classify(ws, now, cfg.idle)
	switch first {
	case verdictLive:
		out.printf("workspace %s: skipped, a process has its cwd inside\n", resolved)
		return kept{}, false, nil
	case verdictRecent:
		return kept{}, false, nil
	}
	reason, err := inspectWorkspace(ctx, &ws, scan, patches)
	if err != nil {
		return kept{}, false, fmt.Errorf("inspect %s: %w", resolved, err)
	}
	// A kept entry sends the skip once; report keys dedupe later runs. jj stderr can span lines, and one workspace must stay one report line.
	if reason != "" {
		out.printf("workspace %s: skipped: %s\n", resolved, reason)
		bytes, sizeErr := dirSize(resolved)
		skips.add(skipJJInspect, bytes)
		oneLine := strings.ReplaceAll(reason, "\n", "; ")
		return kept{root: resolved, line: resolved + ": not inspected (" + oneLine + ")", key: reportKeyOf(resolved, nil)}, true, sizeErr
	}
	final := classify(ws, now, cfg.idle)
	out.debugf("workspace %s: clean=%t unlanded=%d notes=%q verdict=%s\n",
		resolved, ws.Clean, len(ws.Unlanded), ws.Notes, final)
	switch final {
	case verdictDelete:
		// A clean, landed, idle workspace is reported and kept; this sweep does not delete workspaces.
		out.printf("workspace %s: reclaimable (landed, clean, idle)\n", resolved)
		bytes, sizeErr := dirSize(resolved)
		reports.add(string(verdictReclaimable), bytes)
		return kept{root: resolved, line: resolved + ": reclaimable (landed, clean, idle)", key: reportKeyOf(resolved, nil)}, true, sizeErr
	case verdictReport:
		k := kept{root: resolved, line: reportLine(ws), key: reportKeyOf(resolved, ws.Unlanded)}
		if err := trimBuildOutput(ctx, resolved, cfg.apply, out); err != nil {
			return k, true, err
		}
		return k, true, nil
	case verdictForgotten, verdictReclaimable:
		// Forgotten and reclaimable workspaces are reported and kept, never deleted here.
		out.printf("workspace %s: %s\n", resolved, final)
		bytes, sizeErr := dirSize(resolved)
		reports.add(string(final), bytes)
		return kept{root: resolved, line: resolved + ": " + string(final), key: reportKeyOf(resolved, nil)}, true, sizeErr
	default:
		return kept{}, false, nil
	}
}

// workspaceRoots finds jj checkouts and leftover trash dirs up to maxScanDepth levels below root, for example
// <root>/<name>, <root>/<group>/<name>, and <root>/<owner>/<group>/<name>. A jj checkout is not descended into.
// Unreadable dirs are skipped and returned in skipped, so one bad dir does not stop the scan but still fails the run.
// A missing root is an error: a wrong hard-coded root must not scan nothing in silence.
func workspaceRoots(root string) (roots, trash []string, skipped []error, err error) {
	children, err := os.ReadDir(root)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read workspaces root: %w", err)
	}
	scanLevel(root, children, 1, &roots, &trash, &skipped)
	return roots, trash, skipped, nil
}

// maxScanDepth is how deep workspaceRoots looks. Checkouts under a home-level repos directory sit at depth 3.
const maxScanDepth = 3

// scanLevel classifies the entries of dir, which sit at depth, and descends into non-checkout dirs.
func scanLevel(dir string, entries []fs.DirEntry, depth int, roots, trash *[]string, skipped *[]error) {
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		sub := filepath.Join(dir, entry.Name())
		if strings.HasPrefix(entry.Name(), trashPrefix) {
			*trash = append(*trash, sub)
			continue
		}
		isRoot, err := isJJRoot(sub)
		if err != nil {
			*skipped = append(*skipped, fmt.Errorf("skip %s: %w", sub, err))
			continue
		}
		if isRoot {
			*roots = append(*roots, sub)
			continue
		}
		if depth == maxScanDepth {
			continue
		}
		next, err := os.ReadDir(sub)
		if err != nil {
			*skipped = append(*skipped, fmt.Errorf("skip %s: %w", sub, err))
			continue
		}
		scanLevel(sub, next, depth+1, roots, trash, skipped)
	}
}

func isJJRoot(dir string) (bool, error) {
	_, err := os.Stat(filepath.Join(dir, jjDir))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat %s/.jj: %w", dir, err)
	}
	return true, nil
}

// secondaryWorkspace reports whether .jj/repo is a pointer file. A primary checkout has a directory.
func secondaryWorkspace(dir string) (bool, error) {
	info, err := os.Stat(filepath.Join(dir, jjDir, "repo"))
	if err != nil {
		return false, fmt.Errorf("stat repo pointer in %s: %w", dir, err)
	}
	return info.Mode().IsRegular(), nil
}

// excluded reports whether a walked entry is jj metadata, the colocation link, or regenerable tool state.
// Top-level build output (buildOutputDirs) is excluded too, so the clock and untracked scan never walk it.
func excluded(rel string, entry fs.DirEntry) bool {
	// The root .git is jj's colocation link to the store, not user work.
	if entry.Name() == jjDir || rel == ".git" {
		return true
	}
	if !entry.IsDir() {
		return false
	}
	return slices.Contains(regenerableNames, entry.Name()) ||
		slices.Contains(regenerablePaths, filepath.ToSlash(rel)) ||
		slices.Contains(buildOutputDirs, filepath.ToSlash(rel))
}

// skipEntry returns the walk action for an excluded entry: a directory is skipped whole, a file is only passed over.
func skipEntry(entry fs.DirEntry) error {
	if entry.IsDir() {
		return fs.SkipDir
	}
	return nil
}

// treeScan is what one walk of a workspace learns. files holds slash-separated relative paths of every walked
// file, collected only when asked for; nested reports a .jj directory below the root.
type treeScan struct {
	newest time.Time
	files  []string
	nested bool
}

// scanTree walks root once. It finds the newest mtime and, when collect is set, the files unclaimedFiles judges.
// A nested build tree is walked, since only the tracked list says whether to skip it.
func scanTree(root string, collect bool) (treeScan, error) {
	var scan treeScan
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel != "." {
			if entry.Name() == jjDir && rel != jjDir {
				scan.nested = true
			}
			if excluded(rel, entry) {
				return skipEntry(entry)
			}
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.ModTime().After(scan.newest) {
			scan.newest = info.ModTime()
		}
		if collect && !entry.IsDir() {
			scan.files = append(scan.files, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return treeScan{}, err
	}
	// jj rewrites .jj/working_copy on every command a live agent runs. Unlike the rest of .jj, that is activity.
	wc, err := os.Stat(filepath.Join(root, jjDir, "working_copy"))
	if errors.Is(err, fs.ErrNotExist) {
		return scan, nil
	}
	if err != nil {
		return treeScan{}, err
	}
	if wc.ModTime().After(scan.newest) {
		scan.newest = wc.ModTime()
	}
	return scan, nil
}

// unclaimedFiles lists the scanned files jj does not track. Ignored files are untracked, so they appear here too.
// A file under a build tree at any depth is regenerable unless a tracked file lives under that tree.
func unclaimedFiles(files []string, tracked map[string]bool) []string {
	var extra []string
	buildTree := make(map[string]bool)
	for _, rel := range files {
		if tracked[rel] || underUntrackedBuildTree(rel, tracked, buildTree) {
			continue
		}
		extra = append(extra, rel)
	}
	return extra
}

// underUntrackedBuildTree reports whether a parent dir of rel is a build tree holding no tracked file.
// cache memoizes the verdict per dir, since hasTrackedUnder scans the whole tracked set.
func underUntrackedBuildTree(rel string, tracked, cache map[string]bool) bool {
	parts := strings.Split(rel, "/")
	for i := range len(parts) - 1 {
		if !slices.Contains(buildOutputDirs, parts[i]) {
			continue
		}
		dir := strings.Join(parts[:i+1], "/")
		untracked, seen := cache[dir]
		if !seen {
			untracked = !hasTrackedUnder(tracked, dir)
			cache[dir] = untracked
		}
		if untracked {
			return true
		}
	}
	return false
}

// hasTrackedUnder reports whether any tracked path lies under dir, a slash-separated path relative to the root.
func hasTrackedUnder(tracked map[string]bool, dir string) bool {
	prefix := dir + "/"
	for path := range tracked {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// inspectWorkspace fills the jj-derived fields from jj and the sweep's one tree scan. It snapshots the working
// copy, so callers run it only on idle, non-live candidates. A failed jj call is never guessed: it returns a
// non-empty skipped reason and the workspace stays. err is a local failure, such as the clock restore.
func inspectWorkspace(ctx context.Context, ws *workspace, scan treeScan, patches patchCache) (skipped string, err error) {
	name, err := workspaceName(ctx, ws.Root)
	if errors.Is(err, errForgotten) {
		ws.Forgotten = true
		ws.Clean = false
		return "", nil
	}
	if err != nil {
		return err.Error(), nil
	}
	ws.Name = name
	ws.Clean = true
	// The snapshot below bumps the .jj/working_copy dir mtime even when nothing changed, and scanTree counts that.
	// Restore it on every return, error paths included, so inspection never resets the idle clock.
	wcDir := filepath.Join(ws.Root, jjDir, "working_copy")
	wcBefore, err := os.Stat(wcDir)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", wcDir, err)
	}
	restored := false
	restore := func() error {
		restored = true
		if err := os.Chtimes(wcDir, time.Now(), wcBefore.ModTime()); err != nil {
			return fmt.Errorf("restore activity clock of %s: %w", wcDir, err)
		}
		return nil
	}
	defer func() {
		if !restored {
			err = errors.Join(err, restore())
		}
	}()
	// The dirty query snapshots first, so unsaved edits are seen; it alone runs without --ignore-working-copy.
	// A refused snapshot exits 0 with a warning, so stderr is read too.
	dirty, err := jj(ctx, ws.Root, "log", "--no-graph", "-r", "@ & ~empty()", "-T", "change_id.short()")
	if err != nil {
		return fmt.Sprintf("check working copy: %v", err), nil
	}
	if strings.TrimSpace(dirty.stdout) != "" {
		ws.Clean = false
		ws.Notes = append(ws.Notes, "dirty")
	}
	if strings.Contains(dirty.stderr, snapshotRefused) {
		ws.Clean = false
		ws.Notes = append(ws.Notes, "snapshot refused")
	}
	tracked, err := jjQuery(ctx, ws.Root, "file", "list")
	if err != nil {
		return fmt.Sprintf("list tracked files: %v", err), nil
	}
	unlanded, err := findUnlandedChanges(ctx, ws.Root, patches)
	if err != nil {
		return fmt.Sprintf("list unlanded changes: %v", err), nil
	}
	ws.Unlanded = unlanded
	files := make(map[string]bool)
	for _, path := range nonEmptyLines(tracked.stdout) {
		files[path] = true
	}
	if scan.nested {
		ws.Clean = false
		ws.Notes = append(ws.Notes, "nested .jj")
	}
	if extra := unclaimedFiles(scan.files, files); len(extra) > 0 {
		ws.Clean = false
		ws.Notes = append(ws.Notes, fmt.Sprintf("%d untracked or ignored file(s), first %s", len(extra), extra[0]))
	}
	if err := restore(); err != nil {
		return "", err
	}
	return "", nil
}

// workspaceName maps a checkout path to its jj workspace name. Names need not match directory names.
// Both sides are resolved: jj stores the resolved root, and the caller may hold a symlinked path.
func workspaceName(ctx context.Context, dir string) (string, error) {
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", dir, err)
	}
	listing, err := jjQuery(ctx, dir, "workspace", "list", "-T", `name ++ "\t" ++ root ++ "\n"`)
	if err != nil {
		return "", fmt.Errorf("list workspaces: %w", err)
	}
	for line := range strings.SplitSeq(listing.stdout, "\n") {
		name, root, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		got, err := filepath.EvalSymlinks(root)
		if err != nil {
			// A listed root that no longer exists cannot be the target; keep looking.
			continue
		}
		if got == want {
			return name, nil
		}
	}
	return "", fmt.Errorf("%w: %s (resolved %s)", errForgotten, dir, want)
}

// pointerTarget resolves a secondary workspace's .jj/repo pointer to the path it names. A relative target is relative
// to the workspace's .jj dir. The target is not checked here, so each caller decides what a missing store means.
func pointerTarget(dir string) (string, error) {
	pointer, err := os.ReadFile(filepath.Join(dir, jjDir, "repo"))
	if err != nil {
		return "", fmt.Errorf("read repo pointer in %s: %w", dir, err)
	}
	target := strings.TrimSpace(string(pointer))
	if !filepath.IsAbs(target) {
		target = filepath.Join(dir, jjDir, target)
	}
	return target, nil
}

// dirSize sums the sizes of the regular files under root. A root that does not exist has size zero.
func dirSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	return total, err
}

// trimBuildOutput removes top-level build trees from a kept workspace, but only where no tracked file lives.
// Source is never touched.
func trimBuildOutput(ctx context.Context, root string, apply bool, out *printer) error {
	// Removing a child updates the root's mtime, which is the activity clock. Restore it so trimming does not reset the idle clock.
	before, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("stat %s: %w", root, err)
	}
	removed := false
	for _, name := range buildOutputDirs {
		path := filepath.Join(root, name)
		info, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("stat %s: %w", path, err)
		}
		if !info.IsDir() {
			continue
		}
		tracked, err := jjQuery(ctx, root, "file", "list", name)
		if err != nil {
			return fmt.Errorf("check tracked files under %s: %w", path, err)
		}
		if strings.TrimSpace(tracked.stdout) != "" {
			out.printf("workspace %s: %s holds tracked files, kept\n", root, name)
			continue
		}
		if apply {
			if err := os.RemoveAll(path); err != nil {
				return fmt.Errorf("remove %s: %w", path, err)
			}
			removed = true
		}
		out.printf("workspace %s: untracked %s build output%s\n", root, name, applyNote(apply))
	}
	if removed {
		if err := os.Chtimes(root, time.Now(), before.ModTime()); err != nil {
			return fmt.Errorf("restore activity clock of %s: %w", root, err)
		}
	}
	return nil
}

// reportIDCap bounds the unlanded changes named on one report line.
const reportIDCap = 5

// reportLine names a kept workspace, why it was kept, and up to reportIDCap unlanded changes.
func reportLine(ws workspace) string {
	state := slices.Clone(ws.Notes)
	if len(ws.Unlanded) > 0 {
		state = append(state, "unlanded")
	}
	shown := ws.Unlanded[:min(len(ws.Unlanded), reportIDCap)]
	line := fmt.Sprintf("%s [%s] %s", ws.Root, strings.Join(state, ","), strings.Join(shown, " | "))
	if more := len(ws.Unlanded) - len(shown); more > 0 {
		line += fmt.Sprintf(" | +%d more", more)
	}
	return line
}

// jj runs a jj command that may snapshot the working copy.
func jj(ctx context.Context, dir string, args ...string) (commandResult, error) {
	return commandRun(ctx, dir, nil, "jj", append([]string{"--no-pager", "--color=never"}, args...)...)
}

// jjQuery runs a read-only jj command. --ignore-working-copy keeps jj from rewriting the workspace it reads.
func jjQuery(ctx context.Context, dir string, args ...string) (commandResult, error) {
	return jj(ctx, dir, append([]string{"--ignore-working-copy"}, args...)...)
}

func nonEmptyLines(text string) []string {
	var lines []string
	for line := range strings.SplitSeq(text, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
