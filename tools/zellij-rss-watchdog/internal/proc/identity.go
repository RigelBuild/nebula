package proc

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"syscall"
)

// procReader is the narrow /proc view the identity rule reads.
type procReader interface {
	readDir(path string) ([]string, error)
	readText(path string) (string, error)
}

// osProcReader reads the live /proc.
type osProcReader struct{}

func (osProcReader) readDir(path string) ([]string, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names, nil
}

func (osProcReader) readText(path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is a /proc path under the watchdog's proc root, never external input.
	if err != nil {
		return "", err
	}
	return string(data), nil
}

var numericPID = regexp.MustCompile(`^\d+$`)

// exited reports a vanished process; only ENOENT or ESRCH is not an incomplete read.
func exited(err error) bool {
	return errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ESRCH)
}

// sessionServerIdentity returns "<pid>:<starttime>" for the one zellij server for the session.
// It returns "none", or IdentityUnavailable when /proc is unreadable or ambiguous.
func sessionServerIdentity(procRoot, runtimeDir, session string, fsys procReader) string {
	socket := regexp.MustCompile(
		"^" + regexp.QuoteMeta(strings.TrimRight(runtimeDir, "/")) + `/zellij/contract_version_\d+/` + regexp.QuoteMeta(session) + `$`,
	)
	entries, err := fsys.readDir(procRoot)
	if err != nil {
		return IdentityUnavailable
	}
	var found []string
	incomplete := false
	for _, pid := range entries {
		if !numericPID.MatchString(pid) {
			continue
		}
		identity, ok, pidIncomplete := sessionPIDIdentity(procRoot, pid, socket, fsys)
		incomplete = incomplete || pidIncomplete
		if ok {
			found = append(found, identity)
		}
	}
	if incomplete || len(found) > 1 {
		return IdentityUnavailable
	}
	if len(found) == 0 {
		return "none"
	}
	return found[0]
}

// sessionPIDIdentity returns "<pid>:<starttime>" when pid is the session server and its
// start time is stable across a re-check. A read error ends this pid's check; only a
// vanished process is not incomplete.
func sessionPIDIdentity(procRoot, pid string, socket *regexp.Regexp, fsys procReader) (identity string, ok, incomplete bool) {
	isSessionServer := func() (bool, error) {
		cmdline, err := fsys.readText(fmt.Sprintf("%s/%s/cmdline", procRoot, pid))
		if err != nil {
			return false, err
		}
		argv := strings.Split(cmdline, "\x00")
		server := indexOf(argv, "--server")
		return server >= 0 && server+1 < len(argv) && socket.MatchString(argv[server+1]), nil
	}
	startOf := func() (string, error) {
		stat, err := fsys.readText(fmt.Sprintf("%s/%s/stat", procRoot, pid))
		if err != nil {
			return "", err
		}
		return StatStartTime(stat), nil
	}
	match, err := isSessionServer()
	if err != nil {
		return "", false, !exited(err)
	}
	if !match {
		return "", false, false
	}
	before, err := startOf()
	if err != nil {
		return "", false, !exited(err)
	}
	match, err = isSessionServer()
	if err != nil {
		return "", false, !exited(err)
	}
	if !match {
		return "", false, false
	}
	after, err := startOf()
	if err != nil {
		return "", false, !exited(err)
	}
	if before != "" && before == after {
		return pid + ":" + before, true, false
	}
	return "", false, true
}

func indexOf(values []string, target string) int {
	for i, value := range values {
		if value == target {
			return i
		}
	}
	return -1
}

// SessionServerIdentity resolves the zellij server for the session against the live /proc.
func SessionServerIdentity(procRoot, runtimeDir, session string) string {
	return sessionServerIdentity(procRoot, runtimeDir, session, osProcReader{})
}
