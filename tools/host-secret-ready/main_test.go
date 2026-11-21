package main

import (
	"bytes"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
)

type testOwner struct {
	name  string
	group string
	uid   int
	gid   int
}

type readyCase struct {
	name         string
	mode         os.FileMode
	reconcile    bool
	missing      bool
	wantMode     os.FileMode
	wantErr      bool
	wantErrorHas string
}

func TestRun(t *testing.T) {
	owner := lookupTestOwner(t)

	tests := []readyCase{
		{
			name:     "declared metadata passes",
			mode:     0o400,
			wantMode: 0o400,
		},
		{
			name:         "looser mode fails without reconcile",
			mode:         0o440,
			wantMode:     0o440,
			wantErr:      true,
			wantErrorHas: "metadata mismatch",
		},
		{
			name:      "looser mode reconciles safely",
			mode:      0o440,
			reconcile: true,
			wantMode:  0o400,
		},
		{
			name:         "missing file fails on first check",
			missing:      true,
			wantErr:      true,
			wantErrorHas: "timed out",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			checkReadyCase(t, owner, test)
		})
	}
}

// lookupTestOwner returns the running process's uid and effective gid, with
// their account and group names. The fixture files are created and reconciled
// to this identity.
func lookupTestOwner(t *testing.T) testOwner {
	t.Helper()
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	group, err := user.LookupGroupId(strconv.Itoa(os.Getegid()))
	if err != nil {
		t.Fatal(err)
	}
	return testOwner{name: current.Username, group: group.Name, uid: os.Getuid(), gid: os.Getegid()}
}

// writeFixture writes a secret fixture and pins its owner and group to owner.
// A new file's group can come from the parent directory on BSD-style systems,
// so the group is set explicitly and the fixture does not depend on the
// environment.
func writeFixture(path string, content []byte, mode os.FileMode, owner testOwner) error {
	if err := os.WriteFile(path, content, mode); err != nil {
		return err
	}
	return os.Chown(path, owner.uid, owner.gid)
}

// checkReadyCase runs one run() case against a fresh fixture file and checks
// the outcome, the file metadata, and that secret contents stay out of output.
func checkReadyCase(t *testing.T, owner testOwner, test readyCase) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret")
	if !test.missing {
		if err := writeFixture(path, []byte("not logged"), test.mode, owner); err != nil {
			t.Fatal(err)
		}
	}
	args := []string{
		"--path", path,
		"--owner", owner.name,
		"--group", owner.group,
		"--mode", "0400",
		"--timeout", "0s",
	}
	if test.reconcile {
		args = append(args, "--reconcile")
	}
	var stdout, stderr bytes.Buffer
	err := run(args, &stdout, &stderr)
	if (err != nil) != test.wantErr {
		t.Fatalf("run() error = %v, wantErr %t; stderr = %q", err, test.wantErr, stderr.String())
	}
	if test.wantErrorHas != "" && (err == nil || !strings.Contains(err.Error(), test.wantErrorHas)) {
		t.Fatalf("run() error = %v, want substring %q", err, test.wantErrorHas)
	}
	if err == nil {
		assertReadyFile(t, path, owner, test.wantMode, stdout.String())
	}
	if bytes.Contains(stdout.Bytes(), []byte("not logged")) || bytes.Contains(stderr.Bytes(), []byte("not logged")) {
		t.Fatal("secret file contents appeared in output")
	}
}

// assertReadyFile checks the file has the wanted mode and the fixture owner,
// and that run reported readiness.
func assertReadyFile(t *testing.T, path string, owner testOwner, wantMode os.FileMode, stdout string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != wantMode {
		t.Errorf("mode = %04o, want %04o", got, wantMode)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("file metadata does not expose ownership")
	}
	if stat.Uid != uint32(owner.uid) || stat.Gid != uint32(owner.gid) {
		t.Errorf("owner = %d:%d, want %d:%d", stat.Uid, stat.Gid, owner.uid, owner.gid)
	}
	if !strings.Contains(stdout, "ready") {
		t.Errorf("stdout = %q, want readiness confirmation", stdout)
	}
}

func TestRunRejectsInvalidArguments(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "missing path", args: []string{"--owner", "root", "--group", "root", "--mode", "0400"}},
		{name: "invalid mode", args: []string{"--path", "/tmp/x", "--owner", "root", "--group", "root", "--mode", "8888"}},
		{name: "missing owner", args: []string{"--path", "/tmp/x", "--group", "root", "--mode", "0400"}},
		{name: "special bits", args: []string{"--path", "/tmp/x", "--owner", "root", "--group", "root", "--mode", "1400"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if err := run(test.args, &stdout, &stderr); err == nil {
				t.Fatal("run() error = nil, want invalid argument error")
			}
		})
	}
}

func TestRunMissingReconcile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing")
	var stdout, stderr bytes.Buffer
	err := run([]string{"--path", path, "--owner", "unused", "--group", "unused", "--mode", "0400", "--reconcile"}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stat missing file: %v", err)
	}
}

func TestRunDelayedRender(t *testing.T) {
	owner := lookupTestOwner(t)
	path := filepath.Join(t.TempDir(), "secret")
	synctest.Test(t, func(t *testing.T) {
		done := make(chan error, 1)
		go func() {
			<-time.After(300 * time.Millisecond)
			done <- writeFixture(path, []byte("secret"), 0o400, owner)
		}()
		var stdout, stderr bytes.Buffer
		err := run([]string{"--path", path, "--owner", owner.name, "--group", owner.group, "--mode", "0400", "--timeout", "2s"}, &stdout, &stderr)
		if writeErr := <-done; writeErr != nil {
			t.Fatal(writeErr)
		}
		if err != nil {
			t.Fatal(err)
		}
		assertReadyFile(t, path, owner, 0o400, stdout.String())
	})
}

func TestRunUnknownOwner(t *testing.T) {
	owner := lookupTestOwner(t)
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, nil, 0o400); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err := run([]string{"--path", path, "--owner", "host-secret-ready-no-such-user-82947", "--group", owner.group, "--mode", "0400", "--timeout", "0s"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "lookup owner") {
		t.Fatalf("got %v, want owner lookup error", err)
	}
}

func TestRunReconcileGroup(t *testing.T) {
	owner := lookupTestOwner(t)
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	groups, err := current.GroupIds()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "secret")
	if err := writeFixture(path, nil, 0o400, owner); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	initial := info.Sys().(*syscall.Stat_t).Gid
	for _, id := range groups {
		gid, err := strconv.Atoi(id)
		if err != nil {
			t.Fatal(err)
		}
		if uint32(gid) == initial {
			continue
		}
		group, err := user.LookupGroupId(id)
		if err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		err = run([]string{"--path", path, "--owner", owner.name, "--group", group.Name, "--mode", "0400", "--reconcile"}, &stdout, &stderr)
		if err != nil {
			t.Fatal(err)
		}
		owner.gid = gid
		assertReadyFile(t, path, owner, 0o400, stdout.String())
		return
	}
	t.Skip("no second group available")
}
