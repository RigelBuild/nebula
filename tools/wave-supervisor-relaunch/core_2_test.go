package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"testing"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/cotalreach"
	"github.com/RigelBuild/nebula/tools/go-lib/relaunch"
)

// zellijMainPIDRunner stubs the systemctl MainPID read to return stdout.
func zellijMainPIDRunner(stdout string) processRunner {
	return func(_ context.Context, argv []string, _ string) (cotalreach.CommandResult, error) {
		if !slices.Equal(argv, []string{systemctlCommand, userFlag, "show", "-p", "MainPID", "--value", "zellij-server.service"}) {
			return cotalreach.CommandResult{}, fmt.Errorf("unexpected argv %q", argv)
		}
		return cotalreach.CommandResult{Stdout: stdout}, nil
	}
}

func testCoreTriggerAndSupervisorStateDevboxServerIdentityFindsTheDevboxServerBy(t *testing.T) {
	t.Helper()
	fsys := newMemoryFS()
	addProc := func(pid, cmdline, start string) {
		fsys.files["/proc-fixture/"+pid+"/cmdline"] = []byte(cmdline)
		fsys.files["/proc-fixture/"+pid+"/stat"] = []byte(procStat(start))
	}
	addProc("10", "zellij\x00attach\x00devbox\x00", "111")
	addProc("3845913", "/nix/store/x/bin/zellij\x00--server\x00/run/user/1000/zellij/contract_version_1/devbox\x00", "9828715")
	addProc("40", "zellij\x00--server\x00/run/user/1000/zellij/contract_version_1/devbox\x00", "333")
	if got := zellijServerIdentity(t.Context(), "/proc-fixture", "/run/user/1000", "zellij-server.service", fsys, zellijMainPIDRunner("3845913\n")); got != "3845913:9828715" {
		t.Fatalf("identity = %q", got)
	}
	delete(fsys.files, "/proc-fixture/3845913/cmdline")
	delete(fsys.files, "/proc-fixture/3845913/stat")
	if got := zellijServerIdentity(t.Context(), "/proc-fixture", "/run/user/1000", "zellij-server.service", fsys, zellijMainPIDRunner("3845913\n")); got != "none" {
		t.Fatalf("missing identity = %q", got)
	}
}

func testCoreTriggerAndSupervisorStateDevboxServerIdentityReportsAnIncompleteScanAs(t *testing.T) {
	t.Helper()
	fsys := newMemoryFS()
	cmd := "zellij\x00--server\x00/run/user/1000/zellij/contract_version_1/devbox\x00"
	fsys.files["/proc/7/cmdline"] = []byte(cmd)
	runner := zellijMainPIDRunner("7\n")
	reads := 0
	fsys.readHook = func(path string) ([]byte, error, bool) {
		if path == "/proc/7/stat" {
			reads++
			if reads == 1 {
				return []byte(procStat("100")), nil, true
			}
			return []byte(procStat("200")), nil, true
		}
		return nil, nil, false
	}
	if got := zellijServerIdentity(t.Context(), "/proc", "/run/user/1000", "zellij-server.service", fsys, runner); got != "unavailable" {
		t.Fatalf("reused pid identity = %q", got)
	}
	fsys.readHook = func(path string) ([]byte, error, bool) {
		if path == "/proc/7/stat" {
			return nil, fs.ErrPermission, true
		}
		return nil, nil, false
	}
	if got := zellijServerIdentity(t.Context(), "/proc", "/run/user/1000", "zellij-server.service", fsys, runner); got != "unavailable" {
		t.Fatalf("unreadable stat identity = %q", got)
	}
	fsys.readHook = func(path string) ([]byte, error, bool) {
		if path == "/proc/7/stat" {
			return nil, fs.ErrNotExist, true
		}
		return nil, nil, false
	}
	if got := zellijServerIdentity(t.Context(), "/proc", "/run/user/1000", "zellij-server.service", fsys, runner); got != "none" {
		t.Fatalf("exited pid identity = %q", got)
	}
}

func testCoreTriggerAndSupervisorStateDevboxServerIdentityMatchesOnlyThisUserS(t *testing.T) {
	t.Helper()
	fsys := newMemoryFS()
	fsys.files["/proc/1/cmdline"] = []byte("zellij\x00--server\x00/run/user/1001/zellij/contract_version_1/devbox\x00")
	fsys.files["/proc/2/cmdline"] = []byte("zellij\x00--server\x00/run/user/1000/zellij/contract_version_2/devbox\x00")
	fsys.files["/proc/2/stat"] = []byte(procStat("77"))
	fsys.files["/proc/4/cmdline"] = []byte("zellij\x00--server\x00/tmp/run/user/1000/zellij/contract_version_1/devbox\x00")
	if got := zellijServerIdentity(t.Context(), "/proc", "/run/user/1000/", "zellij-server.service", fsys, zellijMainPIDRunner("2\n")); got != "2:77" {
		t.Fatalf("identity = %q", got)
	}
	if got := zellijServerIdentity(t.Context(), "/proc", "/run/user/1000", "zellij-server.service", fsys, zellijMainPIDRunner("1\n")); got != "none" {
		t.Fatalf("other user identity = %q", got)
	}
	if got := zellijServerIdentity(t.Context(), "/proc", "/run/user/1000", "zellij-server.service", fsys, zellijMainPIDRunner("4\n")); got != "none" {
		t.Fatalf("non-runtime identity = %q", got)
	}
	fsys.readHook = func(path string) ([]byte, error, bool) {
		if path == "/proc/3/cmdline" {
			return nil, fs.ErrPermission, true
		}
		return nil, nil, false
	}
	if got := zellijServerIdentity(t.Context(), "/proc", "/run/user/1000", "zellij-server.service", fsys, zellijMainPIDRunner("3\n")); got != "unavailable" {
		t.Fatalf("unreadable MainPID identity = %q", got)
	}
	fsys.readHook = nil
	if got := zellijServerIdentity(t.Context(), "/proc", "/run/user/10.0", "zellij-server.service", fsys, zellijMainPIDRunner("2\n")); got != "none" {
		t.Fatalf("literal runtime identity = %q", got)
	}
}

func testCoreTriggerAndSupervisorStateDevboxServerIdentityFindsTheMainPIDServerOnAHashedSocket(t *testing.T) {
	t.Helper()
	fsys := newMemoryFS()
	fsys.files["/proc/1853/cmdline"] = []byte("zellij\x00--server\x00/run/user/1000/zellij/contract_version_1/d9779d973c63\x00")
	fsys.files["/proc/1853/stat"] = []byte(procStat("9828715"))
	if got := zellijServerIdentity(t.Context(), "/proc", "/run/user/1000", "zellij-server.service", fsys, zellijMainPIDRunner("1853\n")); got != "1853:9828715" {
		t.Fatalf("identity = %q", got)
	}
}

func testCoreTriggerAndSupervisorStateDevboxServerIdentityIgnoresANonZellijMainPID(t *testing.T) {
	t.Helper()
	fsys := newMemoryFS()
	fsys.files["/proc/1853/cmdline"] = []byte("sleep\x00--server\x00/run/user/1000/zellij/contract_version_1/devbox\x00")
	fsys.files["/proc/1853/stat"] = []byte(procStat("9828715"))
	fsys.files["/proc/77/cmdline"] = []byte("zellij\x00--server\x00/run/user/1000/zellij/contract_version_1/devbox\x00")
	fsys.files["/proc/77/stat"] = []byte(procStat("333"))
	if got := zellijServerIdentity(t.Context(), "/proc", "/run/user/1000", "zellij-server.service", fsys, zellijMainPIDRunner("1853\n")); got != "none" {
		t.Fatalf("identity = %q", got)
	}
}

func testCoreTriggerAndSupervisorStateDevboxServerIdentityIsNoneWhenMainPIDIsZero(t *testing.T) {
	t.Helper()
	fsys := newMemoryFS()
	fsys.files["/proc/77/cmdline"] = []byte("zellij\x00--server\x00/run/user/1000/zellij/contract_version_1/devbox\x00")
	fsys.files["/proc/77/stat"] = []byte(procStat("333"))
	if got := zellijServerIdentity(t.Context(), "/proc", "/run/user/1000", "zellij-server.service", fsys, zellijMainPIDRunner("0\n")); got != "none" {
		t.Fatalf("identity = %q", got)
	}
}

func testCoreTriggerAndSupervisorStateDevboxServerIdentityIgnoresANonMainPIDTestServer(t *testing.T) {
	t.Helper()
	fsys := newMemoryFS()
	fsys.files["/proc/1853/cmdline"] = []byte("zellij\x00--server\x00/run/user/1000/zellij/contract_version_1/d9779d973c63\x00")
	fsys.files["/proc/1853/stat"] = []byte(procStat("9828715"))
	fsys.files["/proc/99/cmdline"] = []byte("zellij\x00--server\x00/run/user/1000/zellij/contract_version_1/ztest-42\x00")
	fsys.files["/proc/99/stat"] = []byte(procStat("444"))
	if got := zellijServerIdentity(t.Context(), "/proc", "/run/user/1000", "zellij-server.service", fsys, zellijMainPIDRunner("1853\n")); got != "1853:9828715" {
		t.Fatalf("identity = %q", got)
	}
}

func testCoreTriggerAndSupervisorStateDevboxServerIdentityFailsClosedWhenSystemctlFails(t *testing.T) {
	t.Helper()
	fsys := newMemoryFS()
	fsys.files["/proc/5/cmdline"] = []byte("zellij\x00--server\x00/run/user/1000/zellij/contract_version_1/devbox\x00")
	fsys.files["/proc/5/stat"] = []byte(procStat("55"))
	failing := func(result cotalreach.CommandResult, err error) processRunner {
		return func(context.Context, []string, string) (cotalreach.CommandResult, error) { return result, err }
	}
	cases := []struct {
		name   string
		runner processRunner
		want   string
	}{
		{"exec error", failing(cotalreach.CommandResult{}, errors.New("exec failed")), "unavailable"},
		{"non-zero exit", failing(cotalreach.CommandResult{ExitCode: 1, Stdout: "5\n"}, nil), "unavailable"},
		{"unparseable", zellijMainPIDRunner("junk\n"), "unavailable"},
		{"empty", zellijMainPIDRunner("\n"), "unavailable"},
		{"zero", zellijMainPIDRunner("0\n"), "none"},
		{"valid", zellijMainPIDRunner("5\n"), "5:55"},
	}
	for _, tc := range cases {
		if got := zellijServerIdentity(t.Context(), "/proc", "/run/user/1000", "zellij-server.service", fsys, tc.runner); got != tc.want {
			t.Fatalf("%s identity = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func testCoreSessionHelpersAndPresentationSessionDirForSlugsHomeRelativeAndAbsolute(t *testing.T) {
	t.Helper()
	root := "/home/u/.omp/agent/sessions"
	cases := []struct{ cwd, home, want string }{
		{"/home/u/agents", "/home/u", root + "/-agents"},
		{"/home/u/agents/workspaces/saiph", "/home/u", root + "/-agents-workspaces-saiph"},
		{"/home/user2/x", "/home/u", root + "/-home-user2-x"},
		{"/srv/x", "", root + "/-srv-x"},
	}
	for _, tc := range cases {
		if got := relaunch.SessionDirFor(tc.cwd, tc.home, root); got != tc.want {
			t.Errorf("SessionDirFor(%q,%q) = %q, want %q", tc.cwd, tc.home, got, tc.want)
		}
	}
}

func testCoreSessionHelpersAndPresentationResolvesOpenParentSessionIdAnd(t *testing.T) {
	t.Helper()
	fsys := newMemoryFS()
	fsys.files["/sessions/child.jsonl"] = []byte(`{"type":"session","id":"child-id","parentSession":"fork-id"}`)
	resolved := relaunch.ResolveForkSession([]string{"/sessions/child.jsonl"}, []string{"/sessions/child.jsonl"}, []string{"/sessions/child.jsonl"}, "", "fork-id", fsys, nil, nil)
	if resolved == nil || resolved.ID != "child-id" || resolved.Stale {
		t.Fatalf("open child = %#v", resolved)
	}
	fsys.files["/sessions/old.jsonl"] = []byte(`{"type":"session","id":"old","parentSession":"fork-id"}`)
	fsys.files["/sessions/new.jsonl"] = []byte(`{"type":"session","id":"new","parentSession":"fork-id"}`)
	fsys.times["/sessions/old.jsonl"], fsys.times["/sessions/new.jsonl"] = time.UnixMilli(1), time.UnixMilli(2)
	resolved = relaunch.ResolveForkSession(nil, nil, []string{"/sessions/old.jsonl", "/sessions/new.jsonl"}, "", "fork-id", fsys, nil, nil)
	if resolved == nil || resolved.ID != "new" || resolved.Stale {
		t.Fatalf("newest parent fallback = %#v", resolved)
	}
	resolved = relaunch.ResolveForkSession(nil, nil, nil, "", "fork-id", fsys, nil, nil)
	if resolved == nil || resolved.ID != "fork-id" || !resolved.Stale {
		t.Fatalf("stale fallback = %#v", resolved)
	}
	fsys.files["/sessions/subagent.jsonl"] = []byte(`{"type":"session","id":"subagent-id","parentSession":"fork-id"}`)
	fsys.files["/sessions/lane.jsonl"] = []byte(`{"type":"session","id":"lane-id","parentSession":"fork-id"}`)
	resolved = relaunch.ResolveForkSession([]string{"/sessions/subagent.jsonl", "/sessions/lane.jsonl"}, []string{"/sessions/lane.jsonl"}, []string{"/sessions/lane.jsonl"}, "", "fork-id", fsys, nil, nil)
	if resolved == nil || resolved.ID != "lane-id" {
		t.Fatalf("top-level child = %#v", resolved)
	}
	fsys.files["/sessions/session.jsonl"] = []byte(`{"type":"session","id":"session-id"}`)
	resolved = relaunch.ResolveForkSession([]string{"/sessions/session.jsonl"}, []string{"/sessions/session.jsonl"}, []string{"/sessions/session.jsonl"}, "session-id", "", fsys, nil, nil)
	if resolved == nil || resolved.ID != "session-id" {
		t.Fatalf("explicit session = %#v", resolved)
	}
	fsys.files["/sessions/lane.jsonl"] = []byte(`{"type":"session","id":"lane-id"}`)
	resolved = relaunch.ResolveForkSession([]string{"/sessions/subagent.jsonl", "/sessions/lane.jsonl"}, []string{"/sessions/lane.jsonl"}, []string{"/sessions/lane.jsonl"}, "not-open", "", fsys, nil, nil)
	if resolved == nil || resolved.ID != "lane-id" {
		t.Fatalf("fallback open = %#v", resolved)
	}
	fsys.files["/sessions/subagent.jsonl"] = []byte(`{"type":"session","id":"subagent-id"}`)
	fsys.files["/sessions/lane.jsonl"] = []byte(`{"type":"session","id":"lane-id"}`)
	resolved = relaunch.ResolveForkSession([]string{"/sessions/subagent.jsonl", "/sessions/lane.jsonl"}, []string{"/sessions/lane.jsonl"}, []string{"/sessions/lane.jsonl"}, "not-open", "", fsys, nil, nil)
	if resolved == nil || resolved.ID != "lane-id" {
		t.Fatalf("top-level fallback after unmatched explicit id = %#v", resolved)
	}
}
