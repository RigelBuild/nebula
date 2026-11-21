package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"testing"
)

const (
	testSecretPath = "/run/example/secret-id"
	testAgentLabel = "system/com.example.openbao-agent"
	testLogPath    = "/var/log/openbao-agent.log"
	testSecret     = "secret-id-n-plus-one"
)

// testSecretSHA256 is the lowercase hex SHA-256 the rotation gate compares against.
var testSecretSHA256 = func() string {
	sum := sha256.Sum256([]byte(testSecret))
	return hex.EncodeToString(sum[:])
}()

// withFiles serves secret for the secret_id path and log for every other file.
func withFiles(p probes, log, secret string) probes {
	p.readFile = func(path string) ([]byte, error) {
		if path == testSecretPath {
			return []byte(secret), nil
		}
		return []byte(log), nil
	}
	return p
}

// fakeProbes returns a healthy host with a running agent, correct file
// metadata and a login line in the agent log.
func fakeProbes() probes {
	files := map[string]fileInfo{
		"/run/example/secrets/file-one":  {mode: 0o400, uid: 501, gid: 20},
		"/run/example/secrets/file-two": {mode: 0o400, uid: 0, gid: 0},
		testLogPath: {mode: 0o644, uid: 0, gid: 0},
	}
	p := probes{
		launchctlPrint: func(string) (string, error) { return "state = running\n", nil },
		stat: func(path string) (fileInfo, error) {
			info, ok := files[path]
			if !ok {
				return fileInfo{}, errors.New("no such file")
			}
			return info, nil
		},
		lookupUID: func(name string) (uint32, error) {
			return map[string]uint32{"example-user": 501, "root": 0}[name], nil
		},
		lookupGID: func(name string) (uint32, error) {
			return map[string]uint32{"staff": 20, "wheel": 0}[name], nil
		},
	}
	return withFiles(p, "starting\nauthentication successful\n", testSecret)
}

// healthyArgs passes the required --secret-id and --agent-label.
func healthyArgs() []string {
	return []string{
		"--secret-id", testSecretPath,
		"--agent-label", testAgentLabel,
		"--log", testLogPath,
		"--file", "/run/example/secrets/file-one:example-user:staff:0400",
		"--file", "/run/example/secrets/file-two:root:wheel:0400",
	}
}

func TestRunHealthyHostPasses(t *testing.T) {
	var out bytes.Buffer
	if err := run(healthyArgs(), &out, &out, fakeProbes()); err != nil {
		t.Fatalf("healthy host failed: %v\n%s", err, out.String())
	}
	for _, want := range []string{"agent: running", "metadata ok", "login: authenticated"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}

func TestStoppedAgentFails(t *testing.T) {
	p := fakeProbes()
	p.launchctlPrint = func(string) (string, error) { return "state = not running\n", nil }
	err := run(healthyArgs(), &bytes.Buffer{}, &bytes.Buffer{}, p)
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("stopped agent: err = %v, want not running", err)
	}
}

func TestUnloadedAgentFails(t *testing.T) {
	p := fakeProbes()
	p.launchctlPrint = func(string) (string, error) { return "", errors.New("exit 113") }
	err := run(healthyArgs(), &bytes.Buffer{}, &bytes.Buffer{}, p)
	if err == nil || !strings.Contains(err.Error(), "not loaded") {
		t.Fatalf("unloaded agent: err = %v, want not loaded", err)
	}
}

func TestWrongModeFails(t *testing.T) {
	p := fakeProbes()
	base := p.stat
	p.stat = func(path string) (fileInfo, error) {
		info, err := base(path)
		if path == "/run/example/secrets/file-one" {
			info.mode = 0o440
		}
		return info, err
	}
	err := run(healthyArgs(), &bytes.Buffer{}, &bytes.Buffer{}, p)
	if err == nil || !strings.Contains(err.Error(), "mode 0440, want 0400") {
		t.Fatalf("wrong mode: err = %v, want mode mismatch", err)
	}
}

func TestWrongOwnerFails(t *testing.T) {
	p := fakeProbes()
	base := p.stat
	p.stat = func(path string) (fileInfo, error) {
		info, err := base(path)
		if path == "/run/example/secrets/file-two" {
			info.uid = 501
		}
		return info, err
	}
	err := run(healthyArgs(), &bytes.Buffer{}, &bytes.Buffer{}, p)
	if err == nil || !strings.Contains(err.Error(), "owner 501:0, want root:wheel") {
		t.Fatalf("wrong owner: err = %v, want owner mismatch", err)
	}
}

// A same-value secret_id rewrite must not require a new login line.
func TestSteadyStatePassesAfterSecretIDRewriteWithoutAgentRestart(t *testing.T) {
	p := fakeProbes()
	base := p.stat
	p.stat = func(path string) (fileInfo, error) {
		if path == testSecretPath {
			return fileInfo{}, errors.New("steady-state check must not stat secret_id")
		}
		return base(path)
	}
	if err := run(healthyArgs(), &bytes.Buffer{}, &bytes.Buffer{}, p); err != nil {
		t.Fatalf("steady state after unchanged secret rewrite: %v", err)
	}
}

func TestMissingLoginLineFails(t *testing.T) {
	p := withFiles(fakeProbes(), "starting\nretrying\n", testSecret)
	err := run(healthyArgs(), &bytes.Buffer{}, &bytes.Buffer{}, p)
	if err == nil || !strings.Contains(err.Error(), "no \"authentication successful\" line") {
		t.Fatalf("missing login: err = %v, want missing login line", err)
	}
}

func TestLoginLineFlagOverridesDefault(t *testing.T) {
	p := withFiles(fakeProbes(), "logged in as host\n", testSecret)
	args := append(healthyArgs(), "--login-line", "logged in as host")
	if err := run(args, &bytes.Buffer{}, &bytes.Buffer{}, p); err != nil {
		t.Fatalf("custom login line: %v", err)
	}
}

// rotationArgs adds the rotation gate to the healthy arguments.
func rotationArgs(sha, offset string) []string {
	return append(healthyArgs(), "--expect-sha256", sha, "--since-offset", offset)
}

func TestRotationGatePasses(t *testing.T) {
	before := "authentication successful\n"
	after := before + "authentication successful\n"
	p := withFiles(fakeProbes(), after, testSecret)
	var out bytes.Buffer
	args := rotationArgs(testSecretSHA256, strconv.Itoa(len(before)))
	if err := run(args, &out, &out, p); err != nil {
		t.Fatalf("rotation gate: %v\n%s", err, out.String())
	}
	for _, want := range []string{"secret_id sha256 matches n+1", "after the rotation offset"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}

func TestRotationGateRejectsStaleSecretID(t *testing.T) {
	// The rendered file is generation n; the gate expects n+1.
	p := withFiles(fakeProbes(), "authentication successful\n", "secret-id-generation-n")
	err := run(rotationArgs(testSecretSHA256, "0"), &bytes.Buffer{}, &bytes.Buffer{}, p)
	if err == nil || !strings.Contains(err.Error(), "want n+1") {
		t.Fatalf("stale secret_id: err = %v, want n+1 hash mismatch", err)
	}
	if strings.Contains(err.Error(), "secret-id-generation-n") {
		t.Fatalf("error leaks the rendered secret value: %v", err)
	}
}

// An old login followed only by later retries must not pass the gate. The
// review's false pass was exactly this: a stale line plus a 403 retry.
func TestRotationGateRejectsOldLoginBeforeOffset(t *testing.T) {
	before := "authentication successful\n"
	after := before + "code: 403 permission denied\n"
	p := withFiles(fakeProbes(), after, testSecret)
	err := run(rotationArgs(testSecretSHA256, strconv.Itoa(len(before))), &bytes.Buffer{}, &bytes.Buffer{}, p)
	if err == nil || !strings.Contains(err.Error(), "after offset") {
		t.Fatalf("old login before offset: err = %v, want no login after offset", err)
	}
}

func TestRotationGateRejectsShortLog(t *testing.T) {
	p := withFiles(fakeProbes(), "short\n", testSecret)
	err := run(rotationArgs(testSecretSHA256, "100"), &bytes.Buffer{}, &bytes.Buffer{}, p)
	if err == nil || !strings.Contains(err.Error(), "shorter than offset") {
		t.Fatalf("short log: err = %v, want shorter-than-offset failure", err)
	}
}

func TestRotationFlagsMustComeTogether(t *testing.T) {
	hashOnly := append(healthyArgs(), "--expect-sha256", testSecretSHA256)
	offsetOnly := append(healthyArgs(), "--since-offset", "0")
	for name, args := range map[string][]string{"hash only": hashOnly, "offset only": offsetOnly} {
		err := run(args, &bytes.Buffer{}, &bytes.Buffer{}, fakeProbes())
		if err == nil || !strings.Contains(err.Error(), "must be given together") {
			t.Errorf("%s: err = %v, want both-flags error", name, err)
		}
	}
}

func TestRotationRejectsBadHash(t *testing.T) {
	upper := strings.ToUpper(testSecretSHA256)
	for name, sum := range map[string]string{"uppercase": upper, "short": "abcd", "not hex": strings.Repeat("z", 64)} {
		err := run(rotationArgs(sum, "0"), &bytes.Buffer{}, &bytes.Buffer{}, fakeProbes())
		if err == nil || !strings.Contains(err.Error(), "64 lowercase hex") {
			t.Errorf("%s: err = %v, want hash format error", name, err)
		}
	}
}

func TestParseFileRejectsBadSpecs(t *testing.T) {
	cases := map[string]string{
		"too few fields": "/x:root:wheel",
		"bad mode":       "/x:root:wheel:0999",
		"empty owner":    "/x::wheel:0400",
		"empty path":     ":root:wheel:0400",
	}
	for name, spec := range cases {
		if _, err := parseFile(spec); err == nil {
			t.Errorf("%s: parseFile(%q) accepted, want error", name, spec)
		}
	}
}

func TestParseFileAcceptsValidSpec(t *testing.T) {
	got, err := parseFile("/run/a:example-user:staff:0400")
	if err != nil {
		t.Fatalf("valid spec: %v", err)
	}
	want := fileExpectation{path: "/run/a", owner: "example-user", group: "staff", mode: 0o400}
	if got != want {
		t.Fatalf("parseFile = %+v, want %+v", got, want)
	}
}

func TestNoFileIsAnError(t *testing.T) {
	err := run([]string{"--secret-id", testSecretPath, "--agent-label", testAgentLabel, "--log", testLogPath}, &bytes.Buffer{}, &bytes.Buffer{}, fakeProbes())
	if err == nil || !strings.Contains(err.Error(), "--file is required") {
		t.Fatalf("no files: err = %v, want --file required", err)
	}
}

func TestPositionalArgsRejected(t *testing.T) {
	args := append(healthyArgs(), "extra")
	err := run(args, &bytes.Buffer{}, &bytes.Buffer{}, fakeProbes())
	if err == nil || !strings.Contains(err.Error(), "unexpected positional") {
		t.Fatalf("positional: err = %v, want positional rejection", err)
	}
}
