package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/user"
	"strconv"
	"syscall"
	"time"
)

// pollInterval is how often run re-checks the rendered file while it waits.
const pollInterval = 100 * time.Millisecond

type options struct {
	path      string
	owner     string
	group     string
	mode      os.FileMode
	reconcile bool
	timeout   time.Duration
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("host-secret-ready", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var modeText string
	var timeoutText string
	var opts options
	fs.StringVar(&opts.path, "path", "", "rendered secret path")
	fs.StringVar(&opts.owner, "owner", "", "required file owner")
	fs.StringVar(&opts.group, "group", "", "required file group")
	fs.StringVar(&modeText, "mode", "", "required octal file mode")
	fs.BoolVar(&opts.reconcile, "reconcile", false, "correct file metadata before checking")
	fs.StringVar(&timeoutText, "timeout", "30s", "maximum wait for the rendered file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if opts.path == "" || opts.owner == "" || opts.group == "" {
		return errors.New("--path, --owner, and --group are required")
	}
	mode, err := strconv.ParseUint(modeText, 8, 12)
	if err != nil || mode&^uint64(0o777) != 0 {
		return errors.New("--mode must be an octal permission mode")
	}
	opts.mode = os.FileMode(mode)
	opts.timeout, err = time.ParseDuration(timeoutText)
	if err != nil || opts.timeout < 0 {
		return errors.New("--timeout must be a non-negative duration")
	}
	if opts.reconcile {
		if _, err := os.Stat(opts.path); errors.Is(err, os.ErrNotExist) {
			return nil
		}
	}
	check := func() error { return checkReady(opts, stdout) }
	if opts.reconcile {
		return check()
	}
	return poll(opts.timeout, check)
}

// poll runs check now, then every pollInterval until it succeeds or timeout
// elapses, and returns the last error. A zero timeout checks once.
func poll(timeout time.Duration, check func() error) error {
	err := check()
	if err == nil || timeout == 0 {
		return err
	}
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case <-deadline.C:
			return err
		case <-ticker.C:
			if err = check(); err == nil {
				return nil
			}
		}
	}
}

// checkReady fails until the file exists with the declared metadata. With
// reconcile set it first corrects the metadata of an existing file.
func checkReady(opts options, stdout io.Writer) error {
	if _, err := os.Stat(opts.path); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("timed out waiting for secret file %q", opts.path)
	}
	return checkMetadata(opts, stdout)
}

func checkMetadata(opts options, stdout io.Writer) error {
	owner, err := user.Lookup(opts.owner)
	if err != nil {
		return fmt.Errorf("lookup owner %q: %w", opts.owner, err)
	}
	group, err := user.LookupGroup(opts.group)
	if err != nil {
		return fmt.Errorf("lookup group %q: %w", opts.group, err)
	}
	uid, err := strconv.Atoi(owner.Uid)
	if err != nil {
		return fmt.Errorf("parse owner uid: %w", err)
	}
	gid, err := strconv.Atoi(group.Gid)
	if err != nil {
		return fmt.Errorf("parse group gid: %w", err)
	}

	info, err := os.Stat(opts.path)
	if err != nil {
		return fmt.Errorf("stat secret file: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("file metadata does not expose ownership")
	}
	wantUID, err := toUint32ID(uid)
	if err != nil {
		return fmt.Errorf("owner uid: %w", err)
	}
	wantGID, err := toUint32ID(gid)
	if err != nil {
		return fmt.Errorf("group gid: %w", err)
	}
	modeMismatch := info.Mode().Perm() != opts.mode
	ownerMismatch := stat.Uid != wantUID || stat.Gid != wantGID
	if !modeMismatch && !ownerMismatch {
		return confirmReady(stdout)
	}
	if !opts.reconcile {
		return fmt.Errorf("secret file metadata mismatch: uid=%d gid=%d mode=%04o", stat.Uid, stat.Gid, info.Mode().Perm())
	}
	if err := reconcile(opts.path, uid, gid, opts.mode, ownerMismatch); err != nil {
		return err
	}
	return confirmReady(stdout)
}

// confirmReady prints the readiness line. It never prints file contents.
func confirmReady(stdout io.Writer) error {
	if _, err := fmt.Fprintln(stdout, "ready"); err != nil {
		return fmt.Errorf("write readiness confirmation: %w", err)
	}
	return nil
}

// toUint32ID narrows a uid or gid to uint32, the width syscall.Stat_t uses,
// after checking that it fits.
func toUint32ID(id int) (uint32, error) {
	if id < 0 || int64(id) > math.MaxUint32 {
		return 0, fmt.Errorf("id %d does not fit in uint32", id)
	}
	return uint32(id), nil
}

// reconcile restricts the file to 0400 before any chown, so a reader never
// sees the rendered value with looser metadata than declared.
func reconcile(path string, uid, gid int, mode os.FileMode, ownerMismatch bool) error {
	if err := os.Chmod(path, 0o400); err != nil {
		return fmt.Errorf("restrict secret file mode: %w", err)
	}
	if ownerMismatch {
		if err := os.Chown(path, uid, gid); err != nil {
			return fmt.Errorf("set secret file ownership: %w", err)
		}
	}
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("set secret file mode: %w", err)
	}
	return nil
}
