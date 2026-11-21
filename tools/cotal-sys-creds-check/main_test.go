package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testNow = 1_800_000_000
	testDay = 86_400
)

// goldenLinePath is the golden check line the Grafana alert test also reads.
const goldenLinePath = "testdata/line.golden"

// goldenUpdate rewrites testdata/line.golden from the current output when set.
var goldenUpdate = flag.Bool("update", false, "rewrite testdata/line.golden")

// credsText returns a NATS creds file whose user JWT carries claims.
func credsText(t *testing.T, claims map[string]any) string {
	t.Helper()
	seg := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	jwt := seg(map[string]string{"typ": "JWT", "alg": "ed25519-nkey"}) + "." + seg(claims) + ".sig"
	return "-----BEGIN NATS USER JWT-----\n" + jwt + "\n------END NATS USER JWT------\n\n" +
		"-----BEGIN USER NKEY SEED-----\nSUAFAKE\n------END USER NKEY SEED------\n"
}

func testContract(t *testing.T) contract {
	t.Helper()
	c, err := loadContract()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// writeSpace writes both creds files into a new space material dir and returns the dir.
// The membership observer creds expire 30 days out; only the evictor expiry varies.
func writeSpace(t *testing.T, evictorExp int) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"membership-observer.creds": credsText(t, map[string]any{"exp": testNow + 30*testDay}),
		"connection-evictor.creds":  credsText(t, map[string]any{"exp": evictorExp}),
	}
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// runCLI runs the CLI with args at the given clock, returning the exit code and the captured output.
func runCLI(args []string, now time.Time) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr, func() time.Time { return now })
	return code, stdout.String(), stderr.String()
}

func TestCredsExpiry(t *testing.T) {
	t.Run("reads exp from the JWT block", func(t *testing.T) {
		exp, err := credsExpiry(credsText(t, map[string]any{"exp": testNow + 5}))
		if err != nil {
			t.Fatal(err)
		}
		if exp != testNow+5 {
			t.Fatalf("exp = %v, want %d", exp, testNow+5)
		}
	})

	t.Run("refuses a JWT with no exp, so an unbounded cred is not read as healthy", func(t *testing.T) {
		_, err := credsExpiry(credsText(t, map[string]any{"iat": testNow}))
		if err == nil || !strings.Contains(err.Error(), "exp") {
			t.Fatalf("err = %v, want an error naming exp", err)
		}
	})

	t.Run("refuses a file with no JWT block", func(t *testing.T) {
		_, err := credsExpiry("garbage")
		if err == nil || !strings.Contains(err.Error(), "JWT block") {
			t.Fatalf("err = %v, want an error naming the JWT block", err)
		}
	})
}

func TestReport(t *testing.T) {
	c := testContract(t)

	t.Run("reports the file that expires first", func(t *testing.T) {
		files := []creds{
			{name: "membership-observer.creds", text: credsText(t, map[string]any{"exp": testNow + 20*testDay})},
			{name: "connection-evictor.creds", text: credsText(t, map[string]any{"exp": testNow + 10*testDay})},
		}
		line, warn, err := report(c, files, testNow)
		if err != nil {
			t.Fatal(err)
		}
		prefix := c.LinePrefix + " days_left=10 file=connection-evictor.creds "
		if !strings.HasPrefix(line, prefix) {
			t.Fatalf("line = %q, want prefix %q", line, prefix)
		}
		if warn {
			t.Fatal("warn = true at 10 days, want false")
		}
	})

	t.Run("a tie on expiry goes to the earlier name", func(t *testing.T) {
		files := []creds{
			{name: "membership-observer.creds", text: credsText(t, map[string]any{"exp": testNow + 10*testDay})},
			{name: "connection-evictor.creds", text: credsText(t, map[string]any{"exp": testNow + 10*testDay})},
		}
		line, _, err := report(c, files, testNow)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(line, " file=membership-observer.creds ") {
			t.Fatalf("line = %q, want membership-observer.creds on a tie", line)
		}
	})

	t.Run("warns below 7 days and not at exactly 7", func(t *testing.T) {
		at := func(secs int) (string, bool, error) {
			return report(c, []creds{{name: "connection-evictor.creds", text: credsText(t, map[string]any{"exp": testNow + secs})}}, testNow)
		}
		if _, warn, err := at(7 * testDay); err != nil || warn {
			t.Fatalf("at 7 days: warn = %v, err = %v; want no warn", warn, err)
		}
		line, warn, err := at(7*testDay - 1)
		if err != nil {
			t.Fatal(err)
		}
		if !warn {
			t.Fatal("at 7 days minus 1s: warn = false, want true")
		}
		if !strings.Contains(line, "days_left=6") {
			t.Fatalf("line = %q, want days_left=6", line)
		}
	})

	t.Run("an expired cred reports negative days and warns", func(t *testing.T) {
		files := []creds{{name: "connection-evictor.creds", text: credsText(t, map[string]any{"exp": testNow - 2*testDay})}}
		line, warn, err := report(c, files, testNow)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(line, "days_left=-2") {
			t.Fatalf("line = %q, want days_left=-2", line)
		}
		if !warn {
			t.Fatal("warn = false for an expired cred, want true")
		}
	})
}

func TestRun(t *testing.T) {
	now := time.Unix(testNow, 0)

	t.Run("logs the first-expiring file without a warn prefix when healthy", func(t *testing.T) {
		dir := writeSpace(t, testNow+20*testDay)
		code, stdout, stderr := runCLI([]string{dir}, now)
		if code != 0 {
			t.Fatalf("exit = %d, stderr %q", code, stderr)
		}
		if !strings.HasPrefix(stdout, testContract(t).LinePrefix+" days_left=20 file=connection-evictor.creds expires=") {
			t.Fatalf("stdout = %q, want a healthy check line for connection-evictor.creds", stdout)
		}
		if strings.HasPrefix(stdout, "<4>") {
			t.Fatalf("stdout = %q, want no <4> warn prefix", stdout)
		}
	})

	t.Run("prefixes the warn line with <4> below the warn threshold", func(t *testing.T) {
		dir := writeSpace(t, testNow+2*testDay)
		code, stdout, _ := runCLI([]string{dir}, now)
		if code != 0 {
			t.Fatalf("exit = %d", code)
		}
		if !strings.HasPrefix(stdout, "<4>"+testContract(t).LinePrefix+" days_left=2 ") {
			t.Fatalf("stdout = %q, want a <4> warn line with days_left=2", stdout)
		}
	})

	t.Run("extra arguments after the dir are ignored", func(t *testing.T) {
		dir := writeSpace(t, testNow+20*testDay)
		code, stdout, stderr := runCLI([]string{dir, "extra"}, now)
		if code != 0 {
			t.Fatalf("exit = %d, stderr %q", code, stderr)
		}
		if !strings.Contains(stdout, "days_left=20") {
			t.Fatalf("stdout = %q, want the check line", stdout)
		}
	})

	t.Run("a missing creds file fails with a journald <3> error and exit 1", func(t *testing.T) {
		code, stdout, stderr := runCLI([]string{t.TempDir()}, now)
		if code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		if stdout != "" {
			t.Fatalf("stdout = %q, want empty", stdout)
		}
		if !strings.HasPrefix(stderr, "<3>cotal-sys-creds: ") {
			t.Fatalf("stderr = %q, want a <3> error line", stderr)
		}
	})

	t.Run("no directory argument is a usage error with exit 2", func(t *testing.T) {
		code, stdout, stderr := runCLI(nil, now)
		if code != 2 {
			t.Fatalf("exit = %d, want 2", code)
		}
		if stdout != "" {
			t.Fatalf("stdout = %q, want empty", stdout)
		}
		if !strings.HasPrefix(stderr, "<3>usage: ") {
			t.Fatalf("stderr = %q, want a <3> usage line", stderr)
		}
	})

	t.Run("an empty directory argument is a usage error with exit 2", func(t *testing.T) {
		code, _, stderr := runCLI([]string{""}, now)
		if code != 2 {
			t.Fatalf("exit = %d, want 2", code)
		}
		if !strings.Contains(stderr, "usage:") {
			t.Fatalf("stderr = %q, want a usage line", stderr)
		}
	})

	t.Run("an unreadable JWT fails with exit 1", func(t *testing.T) {
		dir := t.TempDir()
		for _, name := range credsFiles {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("garbage"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		code, _, stderr := runCLI([]string{dir}, now)
		if code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		if !strings.Contains(stderr, "JWT block") {
			t.Fatalf("stderr = %q, want the JWT block error", stderr)
		}
	})
}

func TestContractFileShape(t *testing.T) {
	c := testContract(t)
	if c.LinePrefix != "cotal-sys-creds: checked" || c.WarnDays != 7 {
		t.Fatalf("contract = %+v, want the checked prefix and 7 warn days", c)
	}
}

// TestGoldenLine pins the exact check line. The Grafana alert test reads the same file.
func TestGoldenLine(t *testing.T) {
	dir := writeSpace(t, testNow+3*testDay)
	code, stdout, stderr := runCLI([]string{dir}, time.Unix(testNow, 0))
	if code != 0 {
		t.Fatalf("exit = %d, stderr %q", code, stderr)
	}
	if *goldenUpdate {
		if err := os.MkdirAll(filepath.Dir(goldenLinePath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenLinePath, []byte(stdout), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenLinePath)
	if err != nil {
		t.Fatalf("reading %s (run with -update to create it): %v", goldenLinePath, err)
	}
	if stdout != string(want) {
		t.Fatalf("line = %q, want golden %q", stdout, want)
	}
}
