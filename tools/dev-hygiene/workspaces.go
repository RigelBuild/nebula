package main

import (
	"context"
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

// unpushedRevset selects commits no remote bookmark contains. The root commit and an empty undescribed @ are not work.
const unpushedRevset = `((::@) ~ (::remote_bookmarks()) ~ root()) ~ (@ & empty() & description(exact:""))`

// buildOutputDirs are build trees. A top-level one is excluded outright; a nested one only when no tracked file lives under it.
var buildOutputDirs = []string{"node_modules", "target", "dist"}

// regenerableNames are tool state recreated on demand (devenv, direnv, node_modules at any depth). Matched by base name.
// Files such as .env are deliberately absent: they can hold work and must still block deletion.
var regenerableNames = []string{"node_modules", ".devenv", ".direnv"}

// regenerablePaths are top-level tool caches matched by exact relative path (moon's task cache).
var regenerablePaths = []string{".moon/cache"}

const (
	jjDir = ".jj"
	// trashPrefix names a workspace moved aside before its forget. A leftover one is finished on the next run.
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
		if err != nil {
			// One unreadable root must not stop the sweep of the others.
			errs = append(errs, err)
			continue
		}
		dirs = append(dirs, d...)
		trash = append(trash, t...)
	}
	for _, dir := range trash {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, errors.Join(append(errs, ctxErr)...)
		}
		if !cfg.apply {
			out.printf("leftover %s: would finish deletion (dry run)\n", dir)
			continue
		}
		if _, err := finishMarked(ctx, dir, out); err != nil {
			errs = append(errs, err)
		}
	}
	now := time.Now()
	var keptWorkspaces []kept
	skips := make(skipSummary)
	for _, dir := range dirs {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return keptWorkspaces, errors.Join(append(errs, ctxErr)...)
		}
		k, err := sweepWorkspace(ctx, cfg, dir, live, now, skips, out)
		if err != nil {
			errs = append(errs, err)
		}
		if k != nil {
			keptWorkspaces = append(keptWorkspaces, *k)
		}
	}
	for _, reason := range slices.Sorted(maps.Keys(skips)) {
		t := skips[reason]
		out.printf("skip %s: %d workspace(s), %.2f GiB, kept in place\n", reason, t.count, float64(t.bytes)/(1<<30))
	}
	return keptWorkspaces, errors.Join(errs...)
}

// sweepWorkspace decides one workspace. Cheap, jj-free checks run first; snapshotting jj queries run last,
// and only for an idle workspace with no process inside it. A workspace is kept, never deleted, when its store is
// gone (orphaned-store: no jj call runs on it), when its directory is gone, or when jj could not inspect it. Each
// of those skips is counted in skips under a fixed reason.
func sweepWorkspace(ctx context.Context, cfg config, dir string, live []string, now time.Time, skips skipSummary, out *printer) (*kept, error) {
	resolved, err := filepath.EvalSymlinks(dir)
	if errors.Is(err, fs.ErrNotExist) {
		// The directory went away after the scan. Keep it and report it; the run goes on.
		const reason = "directory missing; jj may still list it"
		out.printf("workspace %s: skipped: %s\n", dir, reason)
		skips.add(skipDirMissing, 0)
		return &kept{root: dir, line: dir + ": not inspected (" + reason + ")", key: reportKeyOf(dir, nil)}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", dir, err)
	}
	secondary, err := secondaryWorkspace(resolved)
	if err != nil {
		return nil, err
	}
	if !secondary {
		// A primary checkout owns the shared store. Forgetting it would be destructive.
		out.printf("workspace %s: primary checkout, never touched\n", resolved)
		return nil, nil
	}
	store, err := pointerTarget(resolved)
	if err != nil {
		return nil, err
	}
	missing := ""
	if _, err := os.Stat(store); errors.Is(err, fs.ErrNotExist) {
		missing = store
	}
	if classify(workspace{Root: resolved, StoreMissing: missing}, now, cfg.idle) == verdictOrphaned {
		// jj cannot judge a workspace whose store is gone, so no jj call runs on it. It is kept and sized for the report.
		bytes, sizeErr := dirSize(resolved)
		out.printf("workspace %s: skipped: %s, store %s missing, %.2f GiB\n", resolved, skipOrphanedStore, missing, float64(bytes)/(1<<30))
		skips.add(skipOrphanedStore, bytes)
		return &kept{root: resolved, line: resolved + ": not inspected (" + skipOrphanedStore + ", store " + missing + " missing)", key: reportKeyOf(resolved, nil)}, sizeErr
	}
	if _, err := os.Stat(filepath.Join(resolved, workspaceMarker)); err == nil {
		// A marker means an earlier run began deleting this workspace; finishMarked decides whether to finish it.
		if hasLiveCwd(resolved, live) {
			out.printf("workspace %s: interrupted deletion, skipped while a process has its cwd inside\n", resolved)
			return nil, nil
		}
		if !cfg.apply {
			out.printf("workspace %s: interrupted deletion, would finish (dry run)\n", resolved)
			return nil, nil
		}
		reclassify, err := finishMarked(ctx, resolved, out)
		if err != nil || !reclassify {
			return nil, err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("stat marker in %s: %w", resolved, err)
	}
	ws := workspace{Root: resolved, LiveCwd: hasLiveCwd(resolved, live)}
	// The one walk of this tree: the activity clock, the stamp, and the files the untracked check reads.
	scan, err := scanTree(resolved, true)
	if err != nil {
		return nil, fmt.Errorf("activity of %s: %w", resolved, err)
	}
	ws.LastActive = scan.newest
	// Live and recent verdicts need no jj call: a snapshot would itself change the workspace.
	first := classify(ws, now, cfg.idle)
	out.debugf("workspace %s: live=%t last-active=%s idle-cutoff=%s verdict=%s\n",
		resolved, ws.LiveCwd, ws.LastActive.Format(time.RFC3339), now.Add(-cfg.idle).Format(time.RFC3339), first)
	switch first {
	case verdictLive:
		out.printf("workspace %s: skipped, a process has its cwd inside\n", resolved)
		return nil, nil
	case verdictRecent:
		return nil, nil
	}
	reason, err := inspectWorkspace(ctx, &ws, scan)
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", resolved, err)
	}
	if reason != "" {
		out.printf("workspace %s: skipped: %s\n", resolved, reason)
		bytes, sizeErr := dirSize(resolved)
		skips.add(skipJJInspect, bytes)
		// A kept entry sends the skip to the report recipient once; the reported set dedupes later runs. jj stderr can span lines, and
		// one workspace must stay one report line.
		oneLine := strings.ReplaceAll(reason, "\n", "; ")
		return &kept{root: resolved, line: resolved + ": not inspected (" + oneLine + ")", key: reportKeyOf(resolved, nil)}, sizeErr
	}
	final := classify(ws, now, cfg.idle)
	out.debugf("workspace %s: clean=%t unpushed=%d notes=%q verdict=%s\n",
		resolved, ws.Clean, len(ws.Unpushed), ws.Notes, final)
	switch final {
	case verdictDelete:
		return nil, deleteWorkspace(ctx, ws, cfg.apply, out)
	case verdictReport:
		k := &kept{root: resolved, line: reportLine(ws), key: reportKeyOf(resolved, ws.Unpushed)}
		if err := trimBuildOutput(ctx, resolved, cfg.apply, out); err != nil {
			return k, err
		}
		return k, nil
	default:
		return nil, nil
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

// newestMtime returns the newest modification time in the tree, excluding .jj and build output.
// It also counts .jj/working_copy, so jj commands a live agent runs count as activity (fleet-gc-roots rule).
func newestMtime(root string) (time.Time, error) {
	scan, err := scanTree(root, false)
	return scan.newest, err
}

// scanTree walks root once. It finds the newest mtime as newestMtime describes and, when collect is set, the
// files unclaimedFiles judges. A nested build tree is walked, since only the tracked list says whether to skip it.
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
func inspectWorkspace(ctx context.Context, ws *workspace, scan treeScan) (skipped string, err error) {
	name, err := workspaceName(ctx, ws.Root)
	if err != nil {
		return err.Error(), nil
	}
	ws.Name = name
	ws.Clean = true
	// The snapshot below bumps the .jj/working_copy dir mtime even when nothing changed, and newestMtime counts that
	// mtime as activity. Restore it on every return, error paths included, so inspection never resets the idle clock.
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
	unpushed, err := jjQuery(ctx, ws.Root, "log", "--no-graph", "-r", unpushedRevset,
		"-T", `change_id.short() ++ " " ++ description.first_line() ++ "\n"`)
	if err != nil {
		return fmt.Sprintf("list unpushed changes: %v", err), nil
	}
	ws.Unpushed = nonEmptyLines(unpushed.stdout)
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
	// The stamp is the scan's clock, taken before any jj call. The restore undoes the snapshot's only counted
	// change, so any file edit since the scan moves the delete recheck past it.
	ws.Stamp = scan.newest
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
	return "", fmt.Errorf("no jj workspace rooted at %s (resolved %s)", dir, want)
}

// deleteWorkspace forgets the workspace while its directory is still in place, so jj removes the git worktree
// registered for it. Only then does it rename and remove the directory. The marker written first lets the next run
// finish a crash at any step.
func deleteWorkspace(ctx context.Context, ws workspace, apply bool, out *printer) error {
	if !apply {
		out.printf("workspace %s: would forget %q and delete (dry run)\n", ws.Root, ws.Name)
		return nil
	}
	// The run's live scan can be up to 30 minutes old. Recheck right before any destructive step.
	fresh, err := recheckCwds(ctx)
	if err != nil {
		return fmt.Errorf("recheck live cwds before deleting %s: %w", ws.Root, err)
	}
	if hasLiveCwd(ws.Root, fresh) {
		out.printf("workspace %s: skipped, a process entered it after the run's scan\n", ws.Root)
		return nil
	}
	// An edit or jj command since inspection moves the clock past Stamp. A zero Stamp refuses every delete.
	activity, err := newestMtime(ws.Root)
	if err != nil {
		return fmt.Errorf("recheck activity of %s: %w", ws.Root, err)
	}
	if activity.After(ws.Stamp) {
		out.printf("workspace %s: skipped, changed after the run's scan\n", ws.Root)
		return nil
	}
	storeRoot, err := primaryRoot(ws.Root)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(ws.Root, workspaceMarker), []byte(storeRoot+"\t"+ws.Name+"\n"), 0o600); err != nil {
		return fmt.Errorf("write marker in %s: %w", ws.Root, err)
	}
	if err := forgetName(ctx, storeRoot, ws.Name); err != nil {
		// The workspace is still registered and usable. A stale marker would make the next run act on it.
		return errors.Join(err, os.Remove(filepath.Join(ws.Root, workspaceMarker)))
	}
	trash := filepath.Join(filepath.Dir(ws.Root), trashPrefix+filepath.Base(ws.Root))
	if err := os.Rename(ws.Root, trash); err != nil {
		return fmt.Errorf("move %s aside: %w", ws.Root, err)
	}
	return removeTrash(trash, out)
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

// primaryRoot resolves a secondary workspace's .jj/repo pointer to the primary checkout that owns the store.
// It refuses any target that is not a primary, so a marker never names a store the cleanup must not touch.
func primaryRoot(dir string) (string, error) {
	target, err := pointerTarget(dir)
	if err != nil {
		return "", err
	}
	storeRoot := filepath.Clean(filepath.Join(target, "..", ".."))
	info, err := os.Stat(target)
	if err != nil {
		return "", fmt.Errorf("stat store of %s: %w", dir, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("store of %s is not a primary checkout", dir)
	}
	return storeRoot, nil
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

// rootedAt reports whether any jj workspace, under any name, is rooted at dir. A marker's name can be stale, so the
// finish guard asks about the path, not the name. Both paths are resolved before the comparison.
func rootedAt(ctx context.Context, storeRoot, dir string) (bool, error) {
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return false, nil
	}
	listing, err := jjQuery(ctx, storeRoot, "workspace", "list", "-T", `name ++ "\t" ++ root ++ "\n"`)
	if err != nil {
		return false, fmt.Errorf("list workspaces of %s: %w", storeRoot, err)
	}
	for line := range strings.SplitSeq(listing.stdout, "\n") {
		_, root, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		got, err := filepath.EvalSymlinks(root)
		if err == nil && got == want {
			return true, nil
		}
	}
	return false, nil
}

// sameDir reports whether two paths name the same directory. Both are resolved, so a symlinked store path matches
// its target. A path that does not resolve matches nothing.
func sameDir(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

// forgetName drops the jj workspace name from the store at storeRoot, whatever path it is rooted at. It runs
// --ignore-working-copy, so the primary's working copy is never snapshotted. It never runs git gc, repack, or prune.
func forgetName(ctx context.Context, storeRoot, name string) error {
	listing, err := jjQuery(ctx, storeRoot, "workspace", "list", "-T", `name ++ "\n"`)
	if err != nil {
		return fmt.Errorf("list workspaces of %s: %w", storeRoot, err)
	}
	if !slices.Contains(nonEmptyLines(listing.stdout), name) {
		return nil
	}
	if _, err := jjQuery(ctx, storeRoot, "workspace", "forget", name); err != nil {
		return fmt.Errorf("forget workspace %q: %w", name, err)
	}
	return nil
}

// removeTrash deletes a directory that is already out of its workspace path.
func removeTrash(dir string, out *printer) error {
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove %s: %w", dir, err)
	}
	out.printf("workspace %s: forgotten and deleted\n", dir)
	return nil
}

// readMarker returns the primary store root and workspace name a marked directory recorded.
func readMarker(dir string) (storeRoot, name string, err error) {
	raw, err := os.ReadFile(filepath.Join(dir, workspaceMarker))
	if err != nil {
		return "", "", fmt.Errorf("read marker in %s: %w", dir, err)
	}
	var ok bool
	storeRoot, name, ok = strings.Cut(strings.TrimSpace(string(raw)), "\t")
	if !ok {
		return "", "", fmt.Errorf("malformed marker in %s", dir)
	}
	return storeRoot, name, nil
}

// finishMarked completes a deletion a crash or error interrupted. A leftover trash dir needs only the forget and
// removal. A marked dir still at its path is judged by jj first: if its name is still registered there, the forget
// never ran, so the marker goes and it reports reclassify=true. Never delete it blind.
func finishMarked(ctx context.Context, dir string, out *printer) (reclassify bool, err error) {
	storeRoot, name, err := readMarker(dir)
	if err != nil {
		return false, err
	}
	if strings.HasPrefix(filepath.Base(dir), trashPrefix) {
		if err := forgetName(ctx, storeRoot, name); err != nil {
			return false, err
		}
		return false, removeTrash(dir, out)
	}
	owner, err := primaryRoot(dir)
	if err != nil {
		return false, err
	}
	if !sameDir(owner, storeRoot) {
		out.printf("workspace %s: marker names another store, removed; reclassifying\n", dir)
		return true, dropMarker(dir)
	}
	rooted, err := rootedAt(ctx, storeRoot, dir)
	if err != nil {
		return false, err
	}
	if rooted {
		// A workspace is still rooted here, under some name. The marker is stale: a forget never ran, or the file
		// was snapshotted into another workspace. Drop it and judge the workspace normally.
		out.printf("workspace %s: stale marker, a workspace is still rooted here; removed, reclassifying\n", dir)
		return true, dropMarker(dir)
	}
	fresh, err := liveCwds(ctx)
	if err != nil {
		return false, fmt.Errorf("recheck live cwds before finishing %s: %w", dir, err)
	}
	if hasLiveCwd(dir, fresh) {
		out.printf("workspace %s: interrupted deletion skipped, a process has its cwd inside\n", dir)
		return false, nil
	}
	trash := filepath.Join(filepath.Dir(dir), trashPrefix+filepath.Base(dir))
	if err := os.Rename(dir, trash); err != nil {
		return false, fmt.Errorf("move %s aside: %w", dir, err)
	}
	return false, removeTrash(trash, out)
}

// dropMarker removes a stale marker and restores the root dir mtime, which the removal bumps. The root mtime is the
// activity clock, so dropping a marker must not make the workspace read as recent (trimBuildOutput pattern).
func dropMarker(dir string) error {
	before, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("stat %s: %w", dir, err)
	}
	if err := os.Remove(filepath.Join(dir, workspaceMarker)); err != nil {
		return fmt.Errorf("remove marker in %s: %w", dir, err)
	}
	if err := os.Chtimes(dir, time.Now(), before.ModTime()); err != nil {
		return fmt.Errorf("restore activity clock of %s: %w", dir, err)
	}
	return nil
}

// trimBuildOutput removes top-level build trees from a kept workspace, but only where no tracked file lives.
// Source is never touched.
func trimBuildOutput(ctx context.Context, root string, apply bool, out *printer) error {
	// Removing a child updates the root's mtime, which is the activity clock. Restore it so a trim never defers deletion.
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

// reportIDCap bounds the unpushed changes named on one report line, so one workspace cannot make a DM line unbounded.
const reportIDCap = 5

// reportLine names a kept workspace, why it was kept, and up to reportIDCap unpushed change IDs with first lines.
func reportLine(ws workspace) string {
	state := slices.Clone(ws.Notes)
	if len(ws.Unpushed) > 0 {
		state = append(state, "unpushed")
	}
	shown := ws.Unpushed[:min(len(ws.Unpushed), reportIDCap)]
	line := fmt.Sprintf("%s [%s] %s", ws.Root, strings.Join(state, ","), strings.Join(shown, " | "))
	if more := len(ws.Unpushed) - len(shown); more > 0 {
		line += fmt.Sprintf(" | +%d more", more)
	}
	return line
}

// recheckCwds is the live-cwd scan deleteWorkspace repeats before deleting. Tests replace it.
var recheckCwds = liveCwds

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
