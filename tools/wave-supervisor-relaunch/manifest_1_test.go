package main

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/relaunch"
)

func TestProcessCensusAndAtomicManifest(t *testing.T) {
	t.Run("resolves --fork parent to the open child session header and records process names", testProcessCensusAndAtomicManifestResolvesForkParentToTheOpen)

	t.Run("prefers a matching session-id header across every open descriptor", testProcessCensusAndAtomicManifestPrefersAMatchingSessionIdHeader)

	t.Run("counts unmapped scopes and preserves an explicit stale session id", testProcessCensusAndAtomicManifestCountsUnmappedScopesAndPreservesAn)

	t.Run("reads only the first 64 KiB and stops at the session header", testProcessCensusAndAtomicManifestReadsOnlyTheFirst64KiB)

	t.Run("uses parent fallback and marks a missing session stale", testProcessCensusAndAtomicManifestUsesParentFallbackAndMarksA)

	t.Run("writes manifests and state through temporary files and rename", testProcessCensusAndAtomicManifestWritesManifestsAndStateThroughTemporary)

	t.Run("reads, expires and removes holds", testProcessCensusAndAtomicManifestReadsExpiresAndRemovesHolds)

	t.Run("fails census when proc cannot be enumerated", func(t *testing.T) {
		fsys := newMemoryFS()
		fsys.readDirErr = errors.New("denied")
		_, err := readWaveManifest("/proc-fixture", fsys, "/sessions", "wave-agents.slice")
		if err == nil || !strings.Contains(err.Error(), "cannot enumerate proc") {
			t.Fatalf("readWaveManifest() error = %v, want cannot enumerate proc", err)
		}
	})
}

type prefixMemoryFS struct {
	relaunch.FileSystem
	bytesRead map[string]int
}

func (f prefixMemoryFS) ReadPrefix(path string, maxBytes int) ([]byte, error) {
	data, err := f.FileSystem.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) > maxBytes {
		data = data[:maxBytes]
	}
	f.bytesRead[path] = len(data)
	return data, nil
}

type fakeTimeoutContext struct {
	parent context.Context
	done   chan struct{}
	mu     sync.Mutex
	err    error
}

func newFakeTimeoutContext(parent context.Context) *fakeTimeoutContext {
	return &fakeTimeoutContext{parent: parent, done: make(chan struct{})}
}

func (c *fakeTimeoutContext) Deadline() (time.Time, bool) { return c.parent.Deadline() }

func (c *fakeTimeoutContext) Done() <-chan struct{} { return c.done }

func (c *fakeTimeoutContext) Value(key any) any { return c.parent.Value(key) }

func (c *fakeTimeoutContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *fakeTimeoutContext) finish(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		c.err = err
		close(c.done)
	}
}

func TestManifestTypeScriptParityHelpers(t *testing.T) {
	t.Run("process agent name follows Node POSIX basename and first declaration", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			env  string
			want string
		}{
			{name: "POSIX separator", env: "COTAL_AGENT_FILE=/agents/accounts.md\x00", want: "accounts"},
			{name: "backslash is not POSIX separator", env: "COTAL_AGENT_FILE=C:\\agents\\accounts.md\x00", want: "C:\\agents\\accounts"},
			{name: "trailing separator", env: "COTAL_AGENT_FILE=/agents/\x00", want: "agents"},
			{name: "root path", env: "COTAL_AGENT_FILE=/\x00", want: ""},
			{name: "empty first declaration wins", env: "COTAL_AGENT_FILE=\x00COTAL_AGENT_FILE=/agents/accounts.md\x00", want: ""},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if got := processAgentName(tc.env); got != tc.want {
					t.Fatalf("processAgentName(%q) = %q, want %q", tc.env, got, tc.want)
				}
			})
		}
	})

	t.Run("argument values use first flag occurrence and reject empty value", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			args []string
			want string
		}{
			{name: "first repeated flag", args: []string{"omp", "--fork", "", "--fork", "parent"}, want: ""},
			{name: "nonempty value", args: []string{"omp", "--fork", "parent"}, want: "parent"},
			{name: "missing value", args: []string{"omp", "--fork"}, want: ""},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if got := argumentValue(tc.args, "--fork"); got != tc.want {
					t.Fatalf("argumentValue(%q) = %q, want %q", tc.args, got, tc.want)
				}
			})
		}
	})

	t.Run("PID names accept any nonempty ASCII decimal string", func(t *testing.T) {
		for _, tc := range []struct {
			pid  string
			want bool
		}{
			{pid: "999999999999999999999999999999", want: true},
			{pid: "0007", want: true},
			{pid: "", want: false},
			{pid: "7a", want: false},
			{pid: "７", want: false},
		} {
			if got := decimalDigits(tc.pid); got != tc.want {
				t.Fatalf("decimalDigits(%q) = %t, want %t", tc.pid, got, tc.want)
			}
		}
	})
}

func TestManifestProcessEnumeration(t *testing.T) {
	fsys := newMemoryFS()
	fsys.files["/proc-fixture/0007/cgroup"] = []byte("0::/user.slice/wave-agents.slice/scope.scope")
	fsys.files["/proc-fixture/0007/cmdline"] = []byte("/nix/store/omp\x00--session-id\x00session\x00")
	fsys.files["/proc-fixture/999999999999999999999999999999/cgroup"] = []byte("0::/wave-agents.slice/scope")
	fsys.files["/proc-fixture/999999999999999999999999999999/cmdline"] = []byte("node\x00")
	fsys.files["/proc-fixture/8/cgroup"] = []byte("0::/user.slice/other.slice/scope")
	fsys.files["/proc-fixture/8/cmdline"] = []byte("omp\x00")

	pids, err := listSliceOmpPIDs("/proc-fixture", fsys, "wave-agents.slice")
	if err != nil || len(pids) != 1 || pids[0] != "0007" {
		t.Fatalf("listSliceOmpPIDs() = %v, %v", pids, err)
	}
	if !waveProcessesPresent("/proc-fixture", fsys, "wave-agents.slice") {
		t.Fatal("waveProcessesPresent() = false with an in-slice OMP process")
	}
	delete(fsys.files, "/proc-fixture/0007/cmdline")
	if waveProcessesPresent("/proc-fixture", fsys, "wave-agents.slice") {
		t.Fatal("waveProcessesPresent() = true without a readable OMP process")
	}
	fsys.readDirErr = errors.New("denied")
	if !waveProcessesPresent("/proc-fixture", fsys, "wave-agents.slice") {
		t.Fatal("waveProcessesPresent() = false when process enumeration fails")
	}
}

func TestManifestCensusAndAtomicWrite(t *testing.T) {
	t.Run("resolves an open child session by fork parent and records process name", testManifestCensusAndAtomicWriteCase1)

	t.Run("uses newest cwd-scoped fork child and preserves stale parent fallback", testManifestCensusAndAtomicWriteCase2)
	t.Run("uses matching session id, counts unmapped scope and marks explicit missing id stale", testManifestCensusAndAtomicWriteCase3)
	t.Run("fails census when an in-slice OMP command line is unreadable", testManifestCensusAndAtomicWriteCase4)

	t.Run("fails census when proc cannot be enumerated", testManifestCensusAndAtomicWriteCase5)

	t.Run("writes manifest through temporary file and rename", testManifestCensusAndAtomicWriteCase6)
}

type manifestWriteFailureFS struct {
	*memoryFS
	writeErr  error
	renameErr error
}

func (f manifestWriteFailureFS) WriteFile(path string, data []byte, mode fs.FileMode) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	return f.memoryFS.WriteFile(path, data, mode)
}

func (f manifestWriteFailureFS) Rename(from, to string) error {
	if f.renameErr != nil {
		return f.renameErr
	}
	return f.memoryFS.Rename(from, to)
}

func TestManifestAtomicWriteFailures(t *testing.T) {
	t.Run("temporary write error is returned without replacing target", func(t *testing.T) {
		fsys := newMemoryFS()
		fsys.files["/state/manifest.json"] = []byte("previous")
		writeErr := errors.New("write failed")
		badFS := manifestWriteFailureFS{memoryFS: fsys, writeErr: writeErr}
		if err := writeWaveManifest("/state/manifest.json", WaveManifest{}, badFS); !errors.Is(err, writeErr) {
			t.Fatalf("writeWaveManifest() error = %v", err)
		}
		if string(fsys.files["/state/manifest.json"]) != "previous" {
			t.Fatal("failed manifest write replaced the previous target")
		}
	})
	t.Run("rename error is returned and temporary file is removed", func(t *testing.T) {
		fsys := newMemoryFS()
		fsys.files["/state/manifest.json"] = []byte("previous")
		renameErr := errors.New("rename failed")
		badFS := manifestWriteFailureFS{memoryFS: fsys, renameErr: renameErr}
		if err := writeWaveManifest("/state/manifest.json", WaveManifest{}, badFS); !errors.Is(err, renameErr) {
			t.Fatalf("writeWaveManifest() error = %v", err)
		}
		if string(fsys.files["/state/manifest.json"]) != "previous" {
			t.Fatal("failed rename replaced the previous target")
		}
		for name := range fsys.files {
			if strings.HasSuffix(name, ".tmp") {
				t.Fatalf("temporary file remains: %s", name)
			}
		}
	})
}

func testProcessCensusAndAtomicManifestResolvesForkParentToTheOpen(t *testing.T) {
	t.Helper()
	fsys := newMemoryFS()
	fsys.files["/proc-fixture/123/cgroup"] = []byte("0::/user.slice/wave-agents.slice/scope.scope")
	fsys.files["/proc-fixture/123/cmdline"] = []byte("omp\x00--fork\x00parent-id\x00")
	fsys.files["/proc-fixture/123/environ"] = []byte("COTAL_AGENT_FILE=/home/test/.cotal/agents/foreman.md\x00")
	fsys.links["/proc-fixture/123/fd/3"] = "/home/test/.omp/agent/sessions/-agents/child.jsonl"
	fsys.files["/home/test/.omp/agent/sessions/-agents/child.jsonl"] = []byte(`{"type":"session","id":"child-id","parentSession":"parent-id","title":"foreman","cwd":"/home/mattw/agents"}`)

	manifest, err := readWaveManifest("/proc-fixture", fsys, "/home/test/.omp/agent/sessions", "wave-agents.slice")
	if err != nil {
		t.Fatal(err)
	}
	want := []relaunch.WaveAgent{{Name: "foreman", SessionID: "child-id"}}
	if !slices.Equal(manifest.Agents, want) {
		t.Fatalf("readWaveManifest().Agents = %#v, want %#v", manifest.Agents, want)
	}
}

func testProcessCensusAndAtomicManifestPrefersAMatchingSessionIdHeader(t *testing.T) {
	t.Helper()
	fsys := newMemoryFS()
	fsys.files["/proc-fixture/125/cgroup"] = []byte("0::/wave-agents.slice/scope")
	fsys.files["/proc-fixture/125/cmdline"] = []byte("omp\x00--session-id\x00wanted-id\x00")
	fsys.files["/proc-fixture/125/environ"] = []byte("COTAL_AGENT_FILE=/home/test/.cotal/agents/accounts.md\x00HOME=/home/test\x00")
	fsys.links["/proc-fixture/125/fd/3"] = "/home/test/.omp/agent/sessions/-agents/subagent/AgentId.jsonl"
	fsys.links["/proc-fixture/125/fd/4"] = "/home/test/.omp/agent/sessions/-agents/lane.jsonl"
	fsys.files["/home/test/.omp/agent/sessions/-agents/subagent/AgentId.jsonl"] = []byte("{\"type\":\"session\",\"id\":\"subagent-id\"}\n")
	fsys.files["/home/test/.omp/agent/sessions/-agents/lane.jsonl"] = []byte("{\"type\":\"session\",\"id\":\"wanted-id\"}\n")

	fsys.files["/proc-fixture/900/cgroup"] = []byte("0::/user.slice/other.slice")
	fsys.files["/proc-fixture/900/cmdline"] = []byte("omp\x00--session-id\x00unowned\x00")
	fsys.files["/proc-fixture/901/cgroup"] = []byte("0::/wave-agents.slice/scope")
	fsys.files["/proc-fixture/901/cmdline"] = []byte("bash\x00--session-id\x00not-omp\x00")

	manifest, err := readWaveManifest("/proc-fixture", fsys, "/home/test/.omp/agent/sessions", "wave-agents.slice")
	if err != nil {
		t.Fatal(err)
	}
	want := []relaunch.WaveAgent{{Name: "accounts", SessionID: "wanted-id"}}
	if !slices.Equal(manifest.Agents, want) {
		t.Fatalf("readWaveManifest().Agents = %#v, want %#v", manifest.Agents, want)
	}
	pids, err := listSliceOmpPIDs("/proc-fixture", fsys, "wave-agents.slice")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pids, []string{"125"}) {
		t.Fatalf("listSliceOmpPIDs() = %#v, want [125]", pids)
	}
}

func testProcessCensusAndAtomicManifestCountsUnmappedScopesAndPreservesAn(t *testing.T) {
	t.Helper()
	fsys := newMemoryFS()
	fsys.files["/proc-fixture/126/cgroup"] = []byte("0::/wave-agents.slice/scope")
	fsys.files["/proc-fixture/126/cmdline"] = []byte("omp\x00")
	fsys.files["/proc-fixture/126/environ"] = []byte("HOME=/home/test\x00")
	fsys.files["/proc-fixture/127/cgroup"] = []byte("0::/wave-agents.slice/scope")
	fsys.files["/proc-fixture/127/cmdline"] = []byte("omp\x00--session-id\x00stale-session\x00")
	fsys.files["/proc-fixture/127/environ"] = []byte("COTAL_AGENT_FILE=/home/test/.cotal/agents/accounts.md\x00HOME=/home/test\x00")

	census, err := readWaveManifest("/proc-fixture", fsys, "/sessions", "wave-agents.slice")
	if err != nil {
		t.Fatal(err)
	}
	if census.Unmapped != 1 {
		t.Fatalf("readWaveManifest().Unmapped = %d, want 1", census.Unmapped)
	}
	want := []relaunch.WaveAgent{{Name: "accounts", SessionID: "stale-session", Stale: true}}
	if !slices.Equal(census.Agents, want) {
		t.Fatalf("readWaveManifest().Agents = %#v, want %#v", census.Agents, want)
	}
}

func testProcessCensusAndAtomicManifestReadsOnlyTheFirst64KiB(t *testing.T) {
	t.Helper()
	fsys := newMemoryFS()
	header := `{"type":"session","id":"header-id","title":"foreman","cwd":"/agents"}`
	file := "/sessions/" + strings.Repeat("x", 64*1024+1) + ".jsonl"
	fsys.files[file] = []byte(header + "\n" + strings.Repeat("y", 70*1024))
	prefixFS := prefixMemoryFS{FileSystem: fsys, bytesRead: make(map[string]int)}
	selected, err := relaunch.NewestSession("/sessions", "foreman", "/agents", map[string]struct{}{}, relaunch.SessionDeps{
		FileSystem: prefixFS,
		ReadDir: func(string) ([]string, error) {
			return []string{filepath.Base(file)}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if selected == nil || selected.ID != "header-id" {
		t.Fatalf("NewestSession() = %#v, want header-id", selected)
	}
	if got := prefixFS.bytesRead[file]; got != 64*1024 {
		t.Fatalf("read bytes = %d, want %d", got, 64*1024)
	}
}
func testManifestCensusAndAtomicWriteCase1(t *testing.T) {
	fsys := newMemoryFS()
	fsys.files["/proc-fixture/123/cgroup"] = []byte("0::/user.slice/wave-agents.slice/scope.scope")
	fsys.files["/proc-fixture/123/cmdline"] = []byte("omp\x00--fork\x00parent-id\x00")
	fsys.files["/proc-fixture/123/environ"] = []byte("COTAL_AGENT_FILE=/home/test/.cotal/agents/foreman.md\x00")
	fsys.links["/proc-fixture/123/fd/3"] = "/home/test/.omp/agent/sessions/-agents/child.jsonl"
	fsys.files["/home/test/.omp/agent/sessions/-agents/child.jsonl"] = []byte(`{"type":"session","id":"child-id","parentSession":"parent-id","title":"foreman","cwd":"/home/mattw/agents"}`)

	got, err := readWaveManifest("/proc-fixture", fsys, "/home/test/.omp/agent/sessions", "wave-agents.slice")
	if err != nil {
		t.Fatal(err)
	}
	if got.Unmapped != 0 || len(got.Agents) != 1 || got.Agents[0].Name != "foreman" || got.Agents[0].SessionID != "child-id" || got.Agents[0].Stale {
		t.Fatalf("census = %#v", got)
	}
}

func testManifestCensusAndAtomicWriteCase2(t *testing.T) {
	fsys := newMemoryFS()
	fsys.files["/proc-fixture/124/cgroup"] = []byte("0::/user.slice/wave-agents.slice/scope.scope")
	fsys.files["/proc-fixture/124/cmdline"] = []byte("omp\x00--fork\x00parent-id\x00")
	fsys.files["/proc-fixture/124/environ"] = []byte("COTAL_AGENT_FILE=/home/test/.cotal/agents/accounts.md\x00HOME=/home/test\x00")
	fsys.links["/proc-fixture/124/cwd"] = "/home/test/project"
	fsys.files["/sessions/-project/older.jsonl"] = []byte(`{"type":"session","id":"older","parentSession":"parent-id"}`)
	fsys.files["/sessions/-project/newer.jsonl"] = []byte(`{"type":"session","id":"newer","parentSession":"parent-id"}`)
	fsys.times["/sessions/-project/older.jsonl"] = time.UnixMilli(1)
	fsys.times["/sessions/-project/newer.jsonl"] = time.UnixMilli(2)
	got, err := readWaveManifest("/proc-fixture", fsys, "/sessions", "wave-agents.slice")
	if err != nil || len(got.Agents) != 1 || got.Agents[0].SessionID != "newer" {
		t.Fatalf("census = %#v, error = %v; want newest fork child", got, err)
	}
	delete(fsys.files, "/sessions/-project/older.jsonl")
	delete(fsys.files, "/sessions/-project/newer.jsonl")
	got, err = readWaveManifest("/proc-fixture", fsys, "/sessions", "wave-agents.slice")
	if err != nil || len(got.Agents) != 1 || got.Agents[0].SessionID != "parent-id" || !got.Agents[0].Stale {
		t.Fatalf("census = %#v, error = %v; want stale parent fallback", got, err)
	}
}
func testManifestCensusAndAtomicWriteCase3(t *testing.T) {
	fsys := newMemoryFS()
	fsys.files["/proc-fixture/125/cgroup"] = []byte("0::/wave-agents.slice/scope")
	fsys.files["/proc-fixture/125/cmdline"] = []byte("omp\x00--session-id\x00wanted-id\x00")
	fsys.files["/proc-fixture/125/environ"] = []byte("COTAL_AGENT_FILE=/home/test/.cotal/agents/accounts.md\x00HOME=/home/test\x00")
	fsys.links["/proc-fixture/125/fd/3"] = "/home/test/.omp/agent/sessions/-agents/subagent/child.jsonl"
	fsys.links["/proc-fixture/125/fd/4"] = "/home/test/.omp/agent/sessions/-agents/lane.jsonl"
	fsys.files["/home/test/.omp/agent/sessions/-agents/subagent/child.jsonl"] = []byte(`{"type":"session","id":"other-id"}`)
	fsys.files["/home/test/.omp/agent/sessions/-agents/lane.jsonl"] = []byte(`{"type":"session","id":"wanted-id"}`)
	fsys.files["/proc-fixture/126/cgroup"] = []byte("0::/wave-agents.slice/scope")
	fsys.files["/proc-fixture/126/cmdline"] = []byte("omp\x00")
	fsys.files["/proc-fixture/126/environ"] = []byte("HOME=/home/test\x00")
	fsys.files["/proc-fixture/127/cgroup"] = []byte("0::/wave-agents.slice/scope")
	fsys.files["/proc-fixture/127/cmdline"] = []byte("omp\x00--session-id\x00stale-session\x00")
	fsys.files["/proc-fixture/127/environ"] = []byte("COTAL_AGENT_FILE=/home/test/.cotal/agents/runners.md\x00HOME=/home/test\x00")

	got, err := readWaveManifest("/proc-fixture", fsys, "/home/test/.omp/agent/sessions", "wave-agents.slice")
	if err != nil {
		t.Fatal(err)
	}
	if got.Unmapped != 1 || len(got.Agents) != 2 {
		t.Fatalf("census = %#v", got)
	}
	byName := make(map[string]relaunch.WaveAgent, len(got.Agents))
	for _, agent := range got.Agents {
		byName[agent.Name] = agent
	}
	if accounts := byName["accounts"]; accounts.SessionID != "wanted-id" || accounts.Stale {
		t.Fatalf("accounts census = %#v", accounts)
	}
	if runners := byName["runners"]; runners.SessionID != "stale-session" || !runners.Stale {
		t.Fatalf("runners census = %#v", runners)
	}
}
func testManifestCensusAndAtomicWriteCase4(t *testing.T) {
	fsys := newMemoryFS()
	fsys.files["/proc-fixture/123/cgroup"] = []byte("0::/wave-agents.slice/scope")
	if _, err := readWaveManifest("/proc-fixture", fsys, "/sessions", "wave-agents.slice"); err == nil || !strings.Contains(err.Error(), "command line for wave process 123") {
		t.Fatalf("readWaveManifest error = %v", err)
	}
}

func testManifestCensusAndAtomicWriteCase5(t *testing.T) {
	fsys := newMemoryFS()
	fsys.readDirErr = errors.New("denied")
	if _, err := readWaveManifest("/proc-fixture", fsys, "/sessions", "wave-agents.slice"); err == nil || !strings.Contains(err.Error(), "cannot enumerate proc") {
		t.Fatalf("readWaveManifest error = %v", err)
	}
}

func testManifestCensusAndAtomicWriteCase6(t *testing.T) {
	fsys := newMemoryFS()
	manifest := WaveManifest{At: "now", Fingerprint: "boot:manager:z", Agents: []relaunch.WaveAgent{{Name: "foreman", SessionID: "child"}}}
	if err := writeWaveManifest("/state/manifest.json", manifest, fsys); err != nil {
		t.Fatal(err)
	}
	if _, ok := fsys.files["/state/manifest.json"]; !ok {
		t.Fatal("manifest target missing")
	}
	for name := range fsys.files {
		if strings.HasSuffix(name, ".tmp") {
			t.Fatalf("temporary file remains: %s", name)
		}
	}
}
