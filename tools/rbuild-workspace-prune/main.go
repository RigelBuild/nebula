// Package main removes rbuild workspaces that no running process is using.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	defaultMaxIdle = 24 * time.Hour
	lockDirName    = ".locks"
	// pruneStagePrefix marks a workspace renamed out of the way for deletion.
	pruneStagePrefix = ".prune-"
)

// options holds the parsed settings for one prune run.
type options struct {
	root    string
	maxIdle time.Duration
	procDir string
	dryRun  bool
	home    bool                         // prunes the fleet home tree (see home.go) instead of seat workspaces
	walk    homeWalk                     // bounds the home idle walk; the zero value uses the defaults
	readCwd func(string) (string, error) // reads one cwd link; tests substitute it
}

// printer writes progress lines and keeps the first write error.
type printer struct {
	w   io.Writer
	err error
}

func (p *printer) printf(format string, args ...any) {
	if p.err == nil {
		_, p.err = fmt.Fprintf(p.w, format, args...)
	}
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		// Nothing is left to report to if stderr is gone; the exit code still signals failure.
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("rbuild-workspace-prune", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var opts options
	var home, maxIdleText string
	flags.StringVar(&opts.root, "root", "", "rbuild workspace root, for example $HOME/workspaces")
	flags.StringVar(&home, "home", "", "rbuild home to prune instead of --root, for example $HOME")
	flags.StringVar(&maxIdleText, "max-idle", defaultMaxIdle.String(), "remove workspaces untouched for longer than this")
	flags.StringVar(&opts.procDir, "proc", "/proc", "process table checked for busy workspaces")
	flags.BoolVar(&opts.dryRun, "dry-run", false, "report what would be removed without removing it")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if (opts.root == "") == (home == "") {
		return errors.New("exactly one of --root or --home is required")
	}
	maxIdle, err := time.ParseDuration(maxIdleText)
	if err != nil || maxIdle <= 0 {
		return errors.New("--max-idle must be a positive duration")
	}
	opts.maxIdle, opts.readCwd = maxIdle, os.Readlink
	if home != "" {
		opts.root, opts.home = home, true
	}
	// Resolve symlinks so cwd paths from /proc compare against the same root.
	root, err := filepath.EvalSymlinks(opts.root)
	if errors.Is(err, fs.ErrNotExist) {
		// A host without an rbuild root or home has nothing to prune.
		return nil
	}
	if err != nil {
		return fmt.Errorf("resolve workspace root: %w", err)
	}
	opts.root = root
	return prune(opts, time.Now(), stdout)
}

// prune removes idle, unbusy workspaces under opts.root.
//
// Candidates are listed before /proc is read. Each one is then renamed out of
// its name and /proc is read again, so a process that entered the workspace
// after the first read keeps its cwd under the staged name and is seen.
func prune(opts options, now time.Time, stdout io.Writer) error {
	root, err := os.OpenRoot(opts.root)
	if err != nil {
		return fmt.Errorf("open workspace root: %w", err)
	}
	defer func() { _ = root.Close() }()

	out := &printer{w: stdout}
	candidates, leftovers, err := selectCandidates(root, now, opts, out)
	if err != nil {
		return err
	}
	busy, err := scanBusy(opts)
	if err != nil {
		return fmt.Errorf("refusing to prune: %w", err)
	}
	held, err := heldSlot(root, opts, now)
	if err != nil {
		return fmt.Errorf("refusing to prune: %w", err)
	}
	if held != "" {
		// Slot locks are host-wide, so a held lock may own any workspace.
		_, err := fmt.Fprintf(stdout, "skip all: slot lock %s held\n", held)
		return err
	}
	var removeErrs []error
	for _, staged := range leftovers {
		if err := pruneStaged(root, staged, busy, opts, out); err != nil {
			removeErrs = append(removeErrs, err)
		}
	}
	for _, rel := range candidates {
		if err := pruneCandidate(root, rel, busy, opts, now, out); err != nil {
			removeErrs = append(removeErrs, err)
		}
	}
	return errors.Join(append(removeErrs, out.err)...)
}

// pruneCandidate stages one workspace, rechecks /proc and the slot locks, and
// deletes it if it is still idle. A staged copy that became busy, or any held
// slot lock, renames it back unless a new run recreated the old name, in which
// case it stays staged for a later run.
func pruneCandidate(root *os.Root, rel string, busy []string, opts options, now time.Time, out *printer) error {
	if isBusy(rel, busy) {
		out.printf("skip busy %s\n", rel)
		return nil
	}
	if opts.dryRun {
		out.printf("would remove %s\n", rel)
		return nil
	}
	staged := stagedName(rel)
	if err := root.Rename(rel, staged); err != nil {
		return fmt.Errorf("stage %s: %w", rel, err)
	}
	rescan, err := scanBusy(opts)
	if err == nil {
		var held string
		if held, err = heldSlot(root, opts, now); err == nil && held != "" {
			// A run took a slot after the first check; it may be about to use this tree.
			rescan = append(rescan, staged)
		}
	}
	if err != nil {
		restoreErr := root.Rename(staged, rel)
		return errors.Join(fmt.Errorf("refusing to prune %s: %w", rel, err), restoreErr)
	}
	if isBusy(staged, rescan) {
		out.printf("skip busy after stage %s\n", rel)
		if isBusy(rel, rescan) {
			// A busy rel belongs to a new run; leave staged for a later run.
			return nil
		}
		if err := root.Rename(staged, rel); err != nil {
			return fmt.Errorf("restore %s: %w", rel, err)
		}
		return nil
	}
	// The staged copy is idle; a busy rel is a new workspace and is left alone.
	return removeStaged(root, staged, rel, out)
}

// pruneStaged deletes a workspace left staged by a crashed run, if it is idle.
func pruneStaged(root *os.Root, staged string, busy []string, opts options, out *printer) error {
	if isBusy(staged, busy) {
		out.printf("skip busy leftover %s\n", staged)
		return nil
	}
	if opts.dryRun {
		out.printf("would remove leftover %s\n", staged)
		return nil
	}
	return removeStaged(root, staged, staged, out)
}

// staleLockAge matches rbuild's staleLockShell: a pending, ownerless or
// unknown-state lock whose dir is older than this is stale, not held.
const staleLockAge = 120 * time.Second

// maxOwnerBytes bounds an owner file read; the file is writable by rbuild.
const maxOwnerBytes = 256

// slotLockHeld returns the held <lockDir>/slot-N lock name, or "". It mirrors
// rbuild's staleLockShell: a "run <id> <pid>" lock is held while the pid lives,
// a malformed run lock is not, and any other lock is held until its dir is
// older than staleLockAge.
func slotLockHeld(root *os.Root, lockDir, procDir string, now time.Time) (string, error) {
	entries, err := readDirFS(root, lockDir)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read slot locks: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || !strings.HasPrefix(name, "slot-") || strings.HasSuffix(name, ".reclaim") {
			continue
		}
		lock := filepath.Join(lockDir, name)
		owner, err := readOwner(root, filepath.Join(lock, "owner"))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("read owner of %s: %w", name, err)
		}
		fields := strings.Fields(owner)
		if len(fields) > 0 && fields[0] == "run" {
			if len(fields) != 3 || !isPID(fields[2]) {
				// rbuild reclaims a malformed run lock at once.
				continue
			}
			_, err = os.Stat(filepath.Join(procDir, fields[2]))
			if err == nil {
				return name, nil
			}
			if !errors.Is(err, fs.ErrNotExist) {
				return "", fmt.Errorf("check owner pid of %s: %w", name, err)
			}
			continue
		}
		// Pending, ownerless (the dir is made before owner) or unknown state.
		info, err := root.Stat(lock)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("stat %s: %w", name, err)
		}
		if now.Sub(info.ModTime()) <= staleLockAge {
			return name, nil
		}
	}
	return "", nil
}

// readOwner reads at most maxOwnerBytes of a lock owner file through the root.
func readOwner(root *os.Root, name string) (string, error) {
	f, err := root.Open(name)
	if err != nil {
		return "", err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxOwnerBytes))
	return string(data), errors.Join(err, f.Close())
}

// removeStaged deletes name through the root and reports success under label.
// Errors name the staged path, which is where the leftover stays.
func removeStaged(root *os.Root, name, label string, out *printer) error {
	if err := root.RemoveAll(name); err != nil {
		return fmt.Errorf("remove %s: %w", name, err)
	}
	out.printf("removed %s\n", label)
	return nil
}

// stagedName returns the staging name for rel, e.g. seat/.prune-ws-<pid>.
func stagedName(rel string) string {
	seat, ws := filepath.Split(rel)
	return filepath.Join(seat, pruneStagePrefix+ws+"-"+strconv.Itoa(os.Getpid()))
}

// idleCandidates returns root-relative seat/workspace paths whose last activity
// is older than maxIdle. Lock directories, staged names and files are skipped.
func idleCandidates(root *os.Root, now time.Time, maxIdle time.Duration) ([]string, error) {
	var out []string
	seats, err := listSeats(root)
	if err != nil {
		return nil, err
	}
	for _, seat := range seats {
		workspaces, err := readDirFS(root, seat)
		if err != nil {
			return nil, fmt.Errorf("read seat %s: %w", seat, err)
		}
		for _, workspace := range workspaces {
			if !workspace.IsDir() || workspace.Name() == lockDirName || strings.HasPrefix(workspace.Name(), pruneStagePrefix) {
				continue
			}
			rel := filepath.Join(seat, workspace.Name())
			idle, err := idleLongerThan(root, rel, workspace, now, maxIdle)
			if err != nil {
				return nil, err
			}
			if idle {
				out = append(out, rel)
			}
		}
	}
	return out, nil
}

// idleLongerThan reports whether the newest mtime of rel and its top-level
// entries is older than maxIdle. A change only in deeper subdirectories is not seen.
func idleLongerThan(root *os.Root, rel string, dir fs.DirEntry, now time.Time, maxIdle time.Duration) (bool, error) {
	info, err := dir.Info()
	if err != nil {
		return false, fmt.Errorf("stat workspace %s: %w", rel, err)
	}
	newest := info.ModTime()
	entries, err := readDirFS(root, rel)
	if err != nil {
		return false, fmt.Errorf("read workspace %s: %w", rel, err)
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("stat %s/%s: %w", rel, entry.Name(), err)
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	return now.Sub(newest) > maxIdle, nil
}

// stagedLeftovers returns staged seat/.prune-* directories from earlier runs
// that have been idle longer than maxIdle.
func stagedLeftovers(root *os.Root, now time.Time, maxIdle time.Duration) ([]string, error) {
	var out []string
	seats, err := listSeats(root)
	if err != nil {
		return nil, err
	}
	for _, seat := range seats {
		workspaces, err := readDirFS(root, seat)
		if err != nil {
			return nil, fmt.Errorf("read seat %s: %w", seat, err)
		}
		for _, workspace := range workspaces {
			if !workspace.IsDir() || !strings.HasPrefix(workspace.Name(), pruneStagePrefix) {
				continue
			}
			rel := filepath.Join(seat, workspace.Name())
			idle, err := idleLongerThan(root, rel, workspace, now, maxIdle)
			if err != nil {
				return nil, err
			}
			if idle {
				out = append(out, rel)
			}
		}
	}
	return out, nil
}

// listSeats returns the seat directory names under root, skipping the lock directory.
func listSeats(root *os.Root) ([]string, error) {
	entries, err := readDirFS(root, ".")
	if err != nil {
		return nil, fmt.Errorf("read workspace root: %w", err)
	}
	var seats []string
	for _, entry := range entries {
		if entry.IsDir() && entry.Name() != lockDirName {
			seats = append(seats, entry.Name())
		}
	}
	return seats, nil
}

// readDirFS lists name through the root, so no path can resolve outside it.
func readDirFS(root *os.Root, name string) ([]fs.DirEntry, error) {
	dir, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()
	return dir.ReadDir(-1)
}

// busyRelative returns each readable process cwd under root, as a root-relative
// path. A cwd that cannot be read because the process exited (ENOENT or ESRCH),
// or a cwd outside root, is skipped. Any other read error is returned so the
// caller removes nothing.
func busyRelative(procDir, root string, readLink func(string) (string, error)) ([]string, error) {
	entries, err := os.ReadDir(procDir)
	if err != nil {
		return nil, fmt.Errorf("read process table: %w", err)
	}
	var busy []string
	for _, entry := range entries {
		if !isPID(entry.Name()) {
			continue
		}
		cwd, err := readLink(filepath.Join(procDir, entry.Name(), "cwd"))
		if exitedProcess(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read cwd of pid %s: %w", entry.Name(), err)
		}
		rel, ok := relativeUnder(root, cwd)
		if !ok {
			continue
		}
		busy = append(busy, rel)
	}
	return busy, nil
}

// relativeUnder returns path relative to root when path is root or inside it.
func relativeUnder(root, path string) (string, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// isBusy reports whether a busy cwd is rel itself or lies inside it.
func isBusy(rel string, busy []string) bool {
	for _, cwd := range busy {
		if cwd == rel || strings.HasPrefix(cwd, rel+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// exitedProcess reports whether err means the process exited during the scan.
func exitedProcess(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH)
}

func isPID(name string) bool {
	_, err := strconv.Atoi(name)
	return err == nil
}
