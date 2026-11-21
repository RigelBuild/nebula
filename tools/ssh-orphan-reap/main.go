// Command ssh-orphan-reap signals commands left behind by tailnet SSH sessions
// whose login was reparented to launchd after the client disconnected.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/exec" //nolint:depguard // ps runs directly with argv, without a shell.
	"os/signal"
	"path"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	psPath      = "/bin/ps"
	loginPrefix = "/usr/bin/login "
	grace       = 10 * time.Second
	maxCmdLen   = 200
)

var tailnet = netip.MustParsePrefix("100.64.0.0/10")

// proc is one ps row.
type proc struct {
	PID     int
	PPID    int
	Command string
}

// target is a descendant of an orphaned tailnet login.
type target struct {
	PID      int
	LoginPID int
	Command  string
}

func main() {
	dryRun := flag.Bool("dry-run", false, "log targets without signalling")
	flag.Parse()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, logger, *dryRun); err != nil {
		logger.Error("ssh-orphan-reap failed", "err", err)
		stop()
		os.Exit(1)
	}
}

func run(ctx context.Context, logger *slog.Logger, dryRun bool) error {
	procs, err := snapshot(ctx)
	if err != nil {
		return err
	}
	targets := selectTargets(procs)
	hups := 0
	for _, t := range targets {
		logSignal(logger, "SIGHUP", t, dryRun)
		if dryRun {
			continue
		}
		if err := syscall.Kill(t.PID, syscall.SIGHUP); err != nil {
			logger.Warn("SIGHUP failed", "pid", t.PID, "err", err)
			continue
		}
		hups++
	}
	kills := 0
	if !dryRun && len(targets) > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(grace):
		}
		procs, err = snapshot(ctx)
		if err != nil {
			return err
		}
		for _, t := range recheck(targets, procs) {
			logSignal(logger, "SIGKILL", t, false)
			if err := syscall.Kill(t.PID, syscall.SIGKILL); err != nil {
				logger.Warn("SIGKILL failed", "pid", t.PID, "err", err)
				continue
			}
			kills++
		}
	}
	if len(targets) > 0 {
		logger.Info("ssh-orphan-reap done", "targets", len(targets), "sighup", hups, "sigkill", kills, "dry_run", dryRun)
	}
	return nil
}

func logSignal(logger *slog.Logger, sig string, t target, dryRun bool) {
	logger.Info("signal", "signal", sig, "pid", t.PID, "login_pid", t.LoginPID,
		"command", truncate(t.Command, maxCmdLen), "dry_run", dryRun)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func snapshot(ctx context.Context) (map[int]proc, error) {
	out, err := exec.CommandContext(ctx, psPath, "-axo", "pid=,ppid=,command=").Output()
	if err != nil {
		return nil, fmt.Errorf("run ps: %w", err)
	}
	return parsePS(string(out)), nil
}

// parsePS parses `ps -axo pid=,ppid=,command=` output; malformed rows are skipped.
func parsePS(out string) map[int]proc {
	procs := map[int]proc{}
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		if err1 != nil || err2 != nil || pid <= 1 {
			continue
		}
		procs[pid] = proc{PID: pid, PPID: ppid, Command: strings.Join(fields[2:], " ")}
	}
	return procs
}

// isOrphanTailnetLogin reports a launchd-parented login whose -h host is a tailnet IPv4.
func isOrphanTailnetLogin(p proc) bool {
	if p.PPID != 1 || !strings.HasPrefix(p.Command, loginPrefix) {
		return false
	}
	args := strings.Fields(p.Command)
	// Only leading options count: the first non-option argument is the username.
	for i := 1; i < len(args) && strings.HasPrefix(args[i], "-"); i++ {
		if args[i] == "-h" && i+1 < len(args) {
			addr, err := netip.ParseAddr(args[i+1])
			return err == nil && addr.Is4() && tailnet.Contains(addr)
		}
	}
	return false
}

// loginOf returns the orphaned tailnet login above pid, or 0.
func loginOf(procs map[int]proc, pid int) int {
	seen := map[int]bool{}
	cur, ok := procs[pid]
	for ok && !seen[cur.PID] {
		seen[cur.PID] = true
		parent, found := procs[cur.PPID]
		if !found {
			return 0
		}
		if isOrphanTailnetLogin(parent) {
			return parent.PID
		}
		cur, ok = parent, true
	}
	return 0
}

// underActivate reports whether pid, or an ancestor below its login, runs activate-rs.
func underActivate(procs map[int]proc, pid int) bool {
	seen := map[int]bool{}
	for cur := procs[pid]; cur.PID != 0 && !seen[cur.PID]; cur = procs[cur.PPID] {
		if isOrphanTailnetLogin(cur) {
			return false
		}
		if isActivateRS(cur) {
			return true
		}
		seen[cur.PID] = true
	}
	return false
}

// isActivateRS reports whether p is deploy-rs's activate binary or a shell running activate-rs.
func isActivateRS(p proc) bool {
	fields := strings.Fields(p.Command)
	if len(fields) == 0 {
		return false
	}
	switch path.Base(fields[0]) {
	case "activate":
		return strings.Contains(fields[0], "-deploy-rs-")
	case "bash", "sh":
		return len(fields) > 1 && path.Base(fields[1]) == "activate-rs"
	}
	return false
}

// selectTargets returns every descendant of an orphaned tailnet login, logins excluded.
func selectTargets(procs map[int]proc) []target {
	var out []target
	for pid, p := range procs {
		if isOrphanTailnetLogin(p) {
			continue
		}
		login := loginOf(procs, pid)
		// Skip activate-rs trees: deploy-rs's magic rollback relies on activate-rs outliving the session.
		if login != 0 && !underActivate(procs, pid) {
			out = append(out, target{PID: pid, LoginPID: login, Command: p.Command})
		}
	}
	slices.SortFunc(out, func(a, b target) int { return a.PID - b.PID })
	return out
}

// recheck keeps targets whose PID still has the same command under the same login.
func recheck(targets []target, procs map[int]proc) []target {
	var out []target
	for _, t := range targets {
		p, ok := procs[t.PID]
		if ok && p.Command == t.Command && loginOf(procs, t.PID) == t.LoginPID {
			out = append(out, t)
		}
	}
	return out
}
