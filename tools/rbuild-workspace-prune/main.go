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

// options holds settings for one prune root.
type options struct {
	root    string
	pattern string
	maxIdle time.Duration
	procDir string
	dryRun  bool
	home    bool                         // prunes the fleet home tree (see home.go) instead of seat workspaces
	walk    homeWalk                     // bounds the home idle walk; the zero value uses the defaults
	readCwd func(string) (string, error) // reads one cwd link; tests substitute it
}

type rootSpec struct {
	dir     string
	pattern string
}

type rootFlags []rootSpec

func (f *rootFlags) String() string { return "" }

func (f *rootFlags) Set(value string) error {
	dir, pattern, hasPattern := strings.Cut(value, "=")
	if dir == "" {
		return errors.New("--root requires a directory")
	}
	if hasPattern {
		if pattern == "" || filepath.IsAbs(pattern) || strings.HasPrefix(filepath.Clean(pattern), "..") {
			return errors.New("--root glob must be a nonempty relative path")
		}
		if _, err := filepath.Match(pattern, ""); err != nil {
			return fmt.Errorf("invalid --root glob: %w", err)
		}
	}
	*f = append(*f, rootSpec{dir: dir, pattern: pattern})
	return nil
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
	var roots rootFlags
	var home, maxIdleText string
	flags.Var(&roots, "root", "prune a root directory, optionally followed by =GLOB")
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
	if (len(roots) == 0) == (home == "") {
		return errors.New("exactly one of --root or --home is required")
	}
	maxIdle, err := time.ParseDuration(maxIdleText)
	if err != nil || maxIdle <= 0 {
		return errors.New("--max-idle must be a positive duration")
	}
	opts.maxIdle, opts.readCwd = maxIdle, os.Readlink
	if home != "" {
		roots = append(roots, rootSpec{dir: home})
		opts.home = true
	}
	for _, spec := range roots {
		root, err := filepath.EvalSymlinks(spec.dir)
		if errors.Is(err, fs.ErrNotExist) {
			// A host without one of the configured roots has nothing to prune there.
			continue
		}
		if err != nil {
			return fmt.Errorf("resolve workspace root %s: %w", spec.dir, err)
		}
		opts.root, opts.pattern = root, spec.pattern
		if err := prune(opts, time.Now(), stdout); err != nil {
			return err
		}
	}
	return nil
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

// pruneCandidate stages one candidate, rescans /proc, and deletes it if it is
// still idle. A busy staged copy is restored unless a new run recreated its
// original name, in which case it stays staged for a later run.
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
	rescan, err := scanBusy(opts)
	if err != nil {
		return fmt.Errorf("refusing to prune leftover %s: %w", staged, err)
	}
	if isBusy(staged, rescan) {
		out.printf("skip busy leftover %s\n", staged)
		return nil
	}
	return removeStaged(root, staged, staged, out)
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
			idle, err := idleLongerThan(root, rel, now, maxIdle)
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

// idleLongerThan checks the entire tree and keeps it when the walk is bounded.
func idleLongerThan(root *os.Root, rel string, now time.Time, maxIdle time.Duration) (bool, error) {
	scan := homeScan{root: root, now: now, maxIdle: maxIdle, budget: defaultWalkBudget, clock: time.Now, out: &printer{w: io.Discard}}
	idle, err := scan.treeIdle(rel)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return idle, err
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
			idle, err := idleLongerThan(root, rel, now, maxIdle)
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
