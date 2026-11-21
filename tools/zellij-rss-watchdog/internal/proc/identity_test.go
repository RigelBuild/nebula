package proc

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"syscall"
	"testing"
)

const (
	testProcRoot   = "/proc"
	testRuntimeDir = "/run/user/1000"
	sessionServer  = "/run/user/1000/zellij/contract_version_1/example-session"
	testSession    = "example-session"
)

// fakeProc serves /proc reads from a map keyed by path. A key mapped to an error returns it.
type fakeProc struct {
	dirs  map[string][]string
	files map[string]string
	errs  map[string]error
	// statOnRead returns successive stat values for one pid, so a start time can change between checks.
	statSeq map[string][]string
}

func (f *fakeProc) readDir(path string) ([]string, error) {
	if err, ok := f.errs[path]; ok {
		return nil, err
	}
	entries, ok := f.dirs[path]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return entries, nil
}

func (f *fakeProc) readText(path string) (string, error) {
	if err, ok := f.errs[path]; ok {
		return "", err
	}
	if strings.HasSuffix(path, "/stat") && f.statSeq != nil {
		pid := strings.TrimSuffix(strings.TrimPrefix(path, testProcRoot+"/"), "/stat")
		if seq := f.statSeq[pid]; len(seq) > 0 {
			f.statSeq[pid] = seq[1:]
			return seq[0], nil
		}
	}
	text, ok := f.files[path]
	if !ok {
		return "", fs.ErrNotExist
	}
	return text, nil
}

// statLine returns a /proc/<pid>/stat line whose starttime is the field StatStartTime reads.
func statLine(starttime string) string {
	// After the ")" split, index 0 is the state; starttime sits at index 19, so 18 placeholders precede it.
	fields := make([]string, 0, 20)
	fields = append(fields, "S")
	for range 18 {
		fields = append(fields, "0")
	}
	fields = append(fields, starttime)
	return "1 (zellij) " + strings.Join(fields, " ")
}

// cmdlineFor returns a NUL-separated cmdline with the given --server path.
func cmdlineFor(server string) string {
	return strings.Join([]string{"zellij", "--server", server}, "\x00")
}

// sessionProc returns a fake /proc holding one pid per entry, each with its cmdline and stat.
func sessionProc(pids map[string]string) *fakeProc {
	f := &fakeProc{dirs: map[string][]string{}, files: map[string]string{}, errs: map[string]error{}}
	entries := make([]string, 0, len(pids))
	for pid, server := range pids {
		entries = append(entries, pid)
		f.files[fmt.Sprintf("%s/%s/cmdline", testProcRoot, pid)] = cmdlineFor(server)
		f.files[fmt.Sprintf("%s/%s/stat", testProcRoot, pid)] = statLine("456")
	}
	f.dirs[testProcRoot] = entries
	return f
}

func TestSessionServerIdentityReturnsTheOneMatch(t *testing.T) {
	fsys := sessionProc(map[string]string{"123": sessionServer, "999": "/run/user/1000/zellij/other"})
	got := sessionServerIdentity(testProcRoot, testRuntimeDir, testSession, fsys)
	if got != "123:456" {
		t.Fatalf("identity = %q, want 123:456", got)
	}
}

func TestSessionServerIdentityIsNoneWithoutAMatch(t *testing.T) {
	fsys := sessionProc(map[string]string{"999": "/run/user/1000/zellij/other"})
	if got := sessionServerIdentity(testProcRoot, testRuntimeDir, testSession, fsys); got != "none" {
		t.Fatalf("identity = %q, want none", got)
	}
}

func TestSessionServerIdentityIsUnavailableWithTwoMatches(t *testing.T) {
	fsys := sessionProc(map[string]string{"123": sessionServer, "124": sessionServer})
	if got := sessionServerIdentity(testProcRoot, testRuntimeDir, testSession, fsys); got != IdentityUnavailable {
		t.Fatalf("identity = %q, want %q", got, IdentityUnavailable)
	}
}

func TestSessionServerIdentityIgnoresAVanishedPid(t *testing.T) {
	// A pid that exits between the listing and the cmdline read is not an incomplete read.
	fsys := sessionProc(map[string]string{"123": sessionServer})
	fsys.errs[testProcRoot+"/123/cmdline"] = syscall.ENOENT
	if got := sessionServerIdentity(testProcRoot, testRuntimeDir, testSession, fsys); got != "none" {
		t.Fatalf("identity = %q, want none for a vanished pid", got)
	}
}

func TestSessionServerIdentityIsUnavailableOnAnUnreadablePid(t *testing.T) {
	// EACCES on a read is incomplete: the server could be hidden behind it.
	fsys := sessionProc(map[string]string{"123": sessionServer})
	fsys.errs[testProcRoot+"/123/stat"] = syscall.EACCES
	if got := sessionServerIdentity(testProcRoot, testRuntimeDir, testSession, fsys); got != IdentityUnavailable {
		t.Fatalf("identity = %q, want %q for an EACCES read", got, IdentityUnavailable)
	}
}

func TestSessionServerIdentityIsUnavailableWhenStartTimeChanges(t *testing.T) {
	// The start time read before and after the cmdline re-check differ: the pid was reused.
	fsys := sessionProc(map[string]string{"123": sessionServer})
	fsys.statSeq = map[string][]string{"123": {statLine("456"), statLine("789")}}
	if got := sessionServerIdentity(testProcRoot, testRuntimeDir, testSession, fsys); got != IdentityUnavailable {
		t.Fatalf("identity = %q, want %q when the start time changes", got, IdentityUnavailable)
	}
}

func TestSessionServerIdentityIsUnavailableWhenRootIsUnreadable(t *testing.T) {
	fsys := &fakeProc{errs: map[string]error{testProcRoot: errors.New("permission denied")}}
	if got := sessionServerIdentity(testProcRoot, testRuntimeDir, testSession, fsys); got != IdentityUnavailable {
		t.Fatalf("identity = %q, want %q when /proc is unreadable", got, IdentityUnavailable)
	}
}
