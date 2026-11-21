// openbao-agent-macos-smoke is the state gate for the darwin host-secret
// backend on a live Mac. It reads state only: the launchd agent is running,
// each rendered file has its declared owner, group and mode, and the agent log
// shows a login.
//
// The rotation gate needs --expect-sha256 and
// --since-offset together. It passes only when the rendered secret_id hashes
// to n+1's printed SHA-256 and the login line is written after the byte offset
// the operator recorded before the kickstart. Without those flags the login
// check requires only a matching login line in the agent log.
//
// Still open, not covered here: the cold reboot with tailscaled held down, a
// mode-only change from 0440 to 0400, and an ownership change applied after the
// file is renamed into place. Those are a manual live-host run; this tool
// changes nothing.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec" //nolint:depguard // Fixed /bin/launchctl with argv only; no shell.
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const (
	agentLog = "/var/log/openbao-agent.log"
)

// Unverified against bao's live log text; pass --login-line to override on a host.
const defaultLoginLine = "authentication successful"

// options holds the parsed flags. expectSHA256 and logOffset are set together
// for the rotation gate; logOffset is -1 when unset.
type options struct {
	secretIDPath string
	agentLabel   string
	logPath      string
	loginLine    string
	expectSHA256 string
	logOffset    int64
	files        []fileExpectation
}

// rotation reports whether the rotation gate is requested.
func (o options) rotation() bool { return o.expectSHA256 != "" }

// fileExpectation is one rendered secret and its declared metadata.
type fileExpectation struct {
	path  string
	owner string
	group string
	mode  uint32
}

// fileInfo is the subset of stat the checks use.
type fileInfo struct {
	mode uint32
	uid  uint32
	gid  uint32
}

// probes are the host reads the checks need. Tests replace them with fakes.
type probes struct {
	launchctlPrint func(label string) (string, error)
	readFile       func(path string) ([]byte, error)
	stat           func(path string) (fileInfo, error)
	lookupUID      func(name string) (uint32, error)
	lookupGID      func(name string) (uint32, error)
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr, osProbes()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer, p probes) error {
	fs := flag.NewFlagSet("openbao-agent-macos-smoke", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var opts options
	var files stringList
	fs.StringVar(&opts.secretIDPath, "secret-id", "", "rendered secret_id file the agent reads (required)")
	fs.StringVar(&opts.agentLabel, "agent-label", "", "launchd label of the OpenBao agent (required)")
	fs.StringVar(&opts.logPath, "log", agentLog, "agent log to search for the login line")
	fs.StringVar(&opts.loginLine, "login-line", defaultLoginLine, "log line that marks a successful login")
	fs.StringVar(&opts.expectSHA256, "expect-sha256", "", "rotation gate: lowercase hex SHA-256 of n+1's secret_id; needs --since-offset")
	fs.Int64Var(&opts.logOffset, "since-offset", -1, "rotation gate: agent log size in bytes recorded before the kickstart; needs --expect-sha256")
	fs.Var(&files, "file", "rendered file as path:owner:group:mode (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if opts.secretIDPath == "" || opts.agentLabel == "" {
		return errors.New("--secret-id and --agent-label are required")
	}
	if err := validateRotation(opts); err != nil {
		return err
	}
	for _, spec := range files {
		exp, err := parseFile(spec)
		if err != nil {
			return err
		}
		opts.files = append(opts.files, exp)
	}
	if len(opts.files) == 0 {
		return errors.New("at least one --file is required")
	}
	return check(opts, p, stdout)
}

// validateRotation keeps the rotation gate all-or-nothing. A hash without an
// offset, or an offset without a hash, would check less than the gate requires.
func validateRotation(opts options) error {
	if opts.logOffset < -1 {
		return errors.New("--since-offset must be a byte count")
	}
	if (opts.expectSHA256 != "") != (opts.logOffset >= 0) {
		return errors.New("--expect-sha256 and --since-offset must be given together")
	}
	if !opts.rotation() {
		return nil
	}
	raw, err := hex.DecodeString(opts.expectSHA256)
	if err != nil || len(raw) != sha256.Size || strings.ToLower(opts.expectSHA256) != opts.expectSHA256 {
		return errors.New("--expect-sha256 must be 64 lowercase hex digits")
	}
	return nil
}

// check runs every probe in order and stops at the first failure.
func check(opts options, p probes, stdout io.Writer) error {
	out, err := p.launchctlPrint(opts.agentLabel)
	if err != nil {
		return fmt.Errorf("agent not loaded: %w", err)
	}
	if !strings.Contains(out, "state = running") {
		return fmt.Errorf("agent %s is not running", opts.agentLabel)
	}
	if _, err := fmt.Fprintln(stdout, "agent: running"); err != nil {
		return err
	}

	for _, exp := range opts.files {
		if err := checkFile(exp, p); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(stdout, "file: %s metadata ok\n", exp.path); err != nil {
			return err
		}
	}

	if opts.rotation() {
		if err := checkSecretID(opts, p); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(stdout, "rotation: secret_id sha256 matches n+1"); err != nil {
			return err
		}
	}
	return checkLogin(opts, p, stdout)
}

// checkFile compares one rendered file with its declared owner, group and mode.
func checkFile(exp fileExpectation, p probes) error {
	info, err := p.stat(exp.path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", exp.path, err)
	}
	wantUID, err := p.lookupUID(exp.owner)
	if err != nil {
		return fmt.Errorf("lookup owner %q: %w", exp.owner, err)
	}
	wantGID, err := p.lookupGID(exp.group)
	if err != nil {
		return fmt.Errorf("lookup group %q: %w", exp.group, err)
	}
	if info.mode&0o777 != exp.mode {
		return fmt.Errorf("%s mode %04o, want %04o", exp.path, info.mode&0o777, exp.mode)
	}
	if info.uid != wantUID || info.gid != wantGID {
		return fmt.Errorf("%s owner %d:%d, want %s:%s", exp.path, info.uid, info.gid, exp.owner, exp.group)
	}
	return nil
}

// checkSecretID hashes the rendered secret_id. The gate passes only when the
// file is n+1, so a stale file cannot pass as the new generation. The error
// names the hashes only, never the secret.
func checkSecretID(opts options, p probes) error {
	data, err := p.readFile(opts.secretIDPath)
	if err != nil {
		return fmt.Errorf("read secret_id: %w", err)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != opts.expectSHA256 {
		return fmt.Errorf("rendered secret_id sha256 %s, want n+1 %s", got, opts.expectSHA256)
	}
	return nil
}

// checkLogin requires a matching login line. The rotation gate requires it after
// the recorded offset, so an old login cannot pass.
func checkLogin(opts options, p probes, stdout io.Writer) error {
	logBytes, err := p.readFile(opts.logPath)
	if err != nil {
		return fmt.Errorf("read agent log: %w", err)
	}
	window := logBytes
	if opts.rotation() {
		if int64(len(logBytes)) < opts.logOffset {
			return fmt.Errorf("%s is shorter than offset %d: the log was rotated or truncated, rerun the rotation", opts.logPath, opts.logOffset)
		}
		window = logBytes[opts.logOffset:]
	}
	if !strings.Contains(string(window), opts.loginLine) {
		if opts.rotation() {
			return fmt.Errorf("no %q line in %s after offset %d", opts.loginLine, opts.logPath, opts.logOffset)
		}
		return fmt.Errorf("no %q line in %s", opts.loginLine, opts.logPath)
	}
	if opts.rotation() {
		_, err := fmt.Fprintln(stdout, "login: authenticated after the rotation offset")
		return err
	}
	_, err = fmt.Fprintln(stdout, "login: authenticated")
	return err
}

// parseFile parses path:owner:group:mode.
func parseFile(spec string) (fileExpectation, error) {
	parts := strings.Split(spec, ":")
	if len(parts) != 4 {
		return fileExpectation{}, fmt.Errorf("--file %q must be path:owner:group:mode", spec)
	}
	mode, err := strconv.ParseUint(parts[3], 8, 12)
	if err != nil || mode&^uint64(0o777) != 0 {
		return fileExpectation{}, fmt.Errorf("--file %q mode must be octal", spec)
	}
	if parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return fileExpectation{}, fmt.Errorf("--file %q needs path, owner and group", spec)
	}
	return fileExpectation{path: parts[0], owner: parts[1], group: parts[2], mode: uint32(mode)}, nil
}

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// osProbes binds the checks to the live host.
func osProbes() probes {
	return probes{
		launchctlPrint: func(label string) (string, error) {
			out, err := exec.Command("/bin/launchctl", "print", label).CombinedOutput() //nolint:gosec // Fixed launchctl binary; arguments do not pass through a shell.
			return string(out), err
		},
		readFile: func(path string) ([]byte, error) {
			return os.ReadFile(filepath.Clean(path))
		},
		stat: func(path string) (fileInfo, error) {
			info, err := os.Stat(path)
			if err != nil {
				return fileInfo{}, err
			}
			st, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				return fileInfo{}, errors.New("file metadata does not expose ownership")
			}
			return fileInfo{
				mode: uint32(info.Mode().Perm()),
				uid:  st.Uid,
				gid:  st.Gid,
			}, nil
		},
		lookupUID: func(name string) (uint32, error) {
			u, err := user.Lookup(name)
			if err != nil {
				return 0, err
			}
			id, err := strconv.ParseUint(u.Uid, 10, 32)
			return uint32(id), err
		},
		lookupGID: func(name string) (uint32, error) {
			g, err := user.LookupGroup(name)
			if err != nil {
				return 0, err
			}
			id, err := strconv.ParseUint(g.Gid, 10, 32)
			return uint32(id), err
		},
	}
}
