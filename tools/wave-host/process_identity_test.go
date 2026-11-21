package main

import (
	"bytes"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestRunProcessIdentityPrintsToken(t *testing.T) {
	var out, stderr bytes.Buffer
	pid := os.Getpid()
	code := runProcessIdentity([]string{"process-identity", "--pid", strconv.Itoa(pid)}, &out, &stderr)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	token := strings.TrimSuffix(out.String(), "\n")
	if token == "" {
		t.Fatal("empty token")
	}
	// The integer microsecond shape is the Darwin token. Linux tokens are
	// boot:start, so only the Darwin shape is asserted on darwin.
	if runtime.GOOS == "darwin" {
		if strings.ContainsAny(token, ". :") {
			t.Fatalf("darwin token must be bare digits, got %q", token)
		}
		if _, err := strconv.ParseUint(token, 10, 64); err != nil {
			t.Fatalf("darwin token is not an unsigned integer: %q: %v", token, err)
		}
	}
}

func TestRunProcessIdentityRefusesBadPID(t *testing.T) {
	for _, args := range [][]string{
		{"process-identity"},
		{"process-identity", "--pid", "0"},
		{"process-identity", "--pid", "nope"},
		{"process-identity", "--pid", "1", "extra"},
	} {
		var out, stderr bytes.Buffer
		if code := runProcessIdentity(args, &out, &stderr); code != 2 {
			t.Fatalf("args %v: exit %d, want usage exit 2", args, code)
		}
		if out.Len() != 0 {
			t.Fatalf("args %v: wrote token %q on refusal", args, out.String())
		}
	}
}

func TestRunProcessIdentityAbsentExitsThree(t *testing.T) {
	child := startProcessHelper(t)
	pid := child.cmd.Process.Pid
	if err := child.stop(); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if code := runProcessIdentity([]string{"process-identity", "--pid", strconv.Itoa(pid)}, &out, &stderr); code != 3 {
		t.Fatalf("exit %d, want 3 for an absent process; stderr %q", code, stderr.String())
	}
	if out.Len() != 0 {
		t.Fatalf("wrote token %q for an absent process", out.String())
	}
}
