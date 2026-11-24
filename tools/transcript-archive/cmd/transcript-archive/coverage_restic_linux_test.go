//go:build linux

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCoverageHistoryArgvAndEnvHideSourceAndPassword checks that no restic child sees the source path or the password.
func TestCoverageHistoryArgvAndEnvHideSourceAndPassword(t *testing.T) {
	const passwordSentinel = "history-password-sentinel-9006"
	const sourceSentinel = "history-source-sentinel-9006"
	fixture := historyFixture(t, passwordSentinel, sourceSentinel)
	real, err := exec.LookPath("restic")
	if err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(t.TempDir(), "calls")
	wrapper := t.TempDir()
	body := "#!/bin/sh\n{ printf 'cmd '; tr '\\000' ' ' < /proc/$$/cmdline; printf '\\n'; cat /proc/$$/environ; printf '\\n'; } >> 'CALLS_LOG'\nexec 'REAL_RESTIC' \"$@\"\n"
	body = strings.NewReplacer("CALLS_LOG", calls, "REAL_RESTIC", real).Replace(body)
	if err := os.WriteFile(filepath.Join(wrapper, "restic"), []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", wrapper+string(os.PathListSeparator)+os.Getenv("PATH"))
	checkHistoryCase(t, fixture.args, fixture.line)
	checkUnexplainedCase(t, fixture.args, fixture.deltas[0], fixture.line, fixture.source)
	records, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	snapshots, lists := 0, 0
	for record := range bytes.SplitSeq(records, []byte{'\n'}) {
		rest, ok := strings.CutPrefix(string(record), "cmd ")
		if !ok {
			continue
		}
		padded := " " + rest + " "
		if strings.Contains(padded, " snapshots ") {
			snapshots++
		}
		if strings.Contains(padded, " ls ") {
			lists++
		}
	}
	t.Logf("healthy-subject: snapshots invocations=%d ls invocations=%d", snapshots, lists)
	if snapshots == 0 || lists == 0 {
		t.Fatalf("expected at least one snapshots and one ls invocation, got snapshots=%d ls=%d", snapshots, lists)
	}
	if bytes.Contains(records, []byte(passwordSentinel)) {
		t.Fatal("password bytes appeared in restic argv or environment")
	}
	if bytes.Contains(records, []byte(sourceSentinel)) {
		t.Fatal("source sentinel appeared in restic argv or environment")
	}
}
