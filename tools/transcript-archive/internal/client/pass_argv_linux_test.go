package client

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestPassChildArgvAndEnvHideSourceAndPassword checks that pass gives restic
// only the list file name, never the source path or the password bytes.
func TestPassChildArgvAndEnvHideSourceAndPassword(t *testing.T) {
	const sentinel = "unique-source-sentinel-9006"
	localRestic(t)
	repo := filepath.Join(t.TempDir(), "repo")
	c, _, state := openTestClient(t, repo)
	source := filepath.Join(t.TempDir(), sentinel)
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(source, "session")
	line := writeVersion(t, file, "data", "normal")
	scanLines(t, c, line)
	calls := filepath.Join(t.TempDir(), "calls")
	resticWrapper(t, fmt.Sprintf(`{ printf 'cmd '; tr '\000' ' ' < /proc/$$/cmdline; printf '\n'; cat /proc/$$/environ; printf '\n'; } >> %q
exec REAL_RESTIC "$@"`, calls))
	ackFiles(t, c, file)
	password, err := os.ReadFile(filepath.Join(state, "password"))
	if err != nil {
		t.Fatal(err)
	}
	if len(password) == 0 {
		t.Fatal("password file is empty")
	}
	records, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	backups := 0
	for record := range bytes.SplitSeq(records, []byte{'\n'}) {
		if rest, ok := strings.CutPrefix(string(record), "cmd "); ok && slices.Contains(strings.Fields(rest), "backup") {
			backups++
		}
	}
	t.Logf("healthy-subject: backup invocations=%d", backups)
	if backups == 0 {
		t.Fatal("no backup invocation was logged")
	}
	if bytes.Contains(records, []byte(sentinel)) {
		t.Fatal("source sentinel appeared in restic argv or environment")
	}
	if bytes.Contains(records, password) {
		t.Fatal("password bytes appeared in restic argv or environment")
	}
}
