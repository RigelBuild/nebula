package relaunch

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

type testFileInfo struct {
	name string
	dir  bool
	mod  time.Time
}

func (i testFileInfo) Name() string       { return i.name }
func (i testFileInfo) Size() int64        { return 0 }
func (i testFileInfo) Mode() fs.FileMode  { return 0 }
func (i testFileInfo) ModTime() time.Time { return i.mod }
func (i testFileInfo) IsDir() bool        { return i.dir }
func (i testFileInfo) Sys() any           { return nil }

type testFS struct {
	files map[string][]byte
	links map[string]string
	times map[string]time.Time
}

func newTestFS() *testFS {
	return &testFS{files: make(map[string][]byte), links: make(map[string]string), times: make(map[string]time.Time)}
}

func (f *testFS) ReadFile(path string) ([]byte, error) {
	data, ok := f.files[path]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return data, nil
}

func (f *testFS) ReadDir(path string) ([]fs.DirEntry, error) {
	prefix := filepath.Clean(path) + string(filepath.Separator)
	seen := make(map[string]bool)
	entries := make([]fs.DirEntry, 0)
	for file := range f.files {
		if !strings.HasPrefix(file, prefix) {
			continue
		}
		remainder := strings.TrimPrefix(file, prefix)
		name, _, _ := strings.Cut(remainder, string(filepath.Separator))
		if seen[name] {
			continue
		}
		seen[name] = true
		isDir := strings.Contains(remainder, string(filepath.Separator))
		entries = append(entries, fs.FileInfoToDirEntry(testFileInfo{name: name, dir: isDir, mod: f.times[filepath.Join(path, name)]}))
	}
	for file := range f.links {
		if !strings.HasPrefix(file, prefix) {
			continue
		}
		remainder := strings.TrimPrefix(file, prefix)
		name, _, _ := strings.Cut(remainder, string(filepath.Separator))
		if seen[name] {
			continue
		}
		seen[name] = true
		isDir := strings.Contains(remainder, string(filepath.Separator))
		entries = append(entries, fs.FileInfoToDirEntry(testFileInfo{name: name, dir: isDir, mod: f.times[filepath.Join(path, name)]}))
	}
	if len(entries) == 0 {
		return nil, fs.ErrNotExist
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, nil
}

func (f *testFS) Stat(path string) (fs.FileInfo, error) {
	if _, ok := f.files[path]; ok {
		return testFileInfo{name: filepath.Base(path), mod: f.times[path]}, nil
	}
	prefix := filepath.Clean(path) + string(filepath.Separator)
	for file := range f.files {
		if strings.HasPrefix(file, prefix) {
			return testFileInfo{name: filepath.Base(path), dir: true, mod: f.times[path]}, nil
		}
	}
	return nil, fs.ErrNotExist
}

func (f *testFS) Readlink(path string) (string, error) {
	value, ok := f.links[path]
	if !ok {
		return "", fs.ErrNotExist
	}
	return value, nil
}
func (f *testFS) WriteFile(path string, data []byte, _ fs.FileMode) error {
	f.files[path] = append([]byte(nil), data...)
	return nil
}
func (f *testFS) MkdirAll(string, fs.FileMode) error { return nil }
func (f *testFS) Rename(from, to string) error {
	data, ok := f.files[from]
	if !ok {
		return fs.ErrNotExist
	}
	f.files[to] = data
	delete(f.files, from)
	return nil
}
func (f *testFS) Remove(path string) error {
	if _, ok := f.files[path]; !ok {
		return fs.ErrNotExist
	}
	delete(f.files, path)
	return nil
}

func TestSessionDirForSlugsHomeRelativeAndAbsoluteCwds(t *testing.T) {
	root := "/home/u/.omp/agent/sessions"
	cases := []struct {
		name string
		cwd  string
		home string
		want string
	}{
		{name: "home cwd", cwd: "/home/u/agents", home: "/home/u", want: root + "/-agents"},
		{name: "nested home cwd", cwd: "/home/u/agents/workspaces/example", home: "/home/u", want: root + "/-agents-workspaces-example"},
		{name: "prefix sibling", cwd: "/home/user2/x", home: "/home/u", want: root + "/-home-user2-x"},
		{name: "absolute cwd", cwd: "/srv/x", home: "", want: root + "/-srv-x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SessionDirFor(tc.cwd, tc.home, root); got != tc.want {
				t.Fatalf("SessionDirFor() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolvesOpenParentSessionIDAndStaleSessions(t *testing.T) {
	fsys := newTestFS()
	fsys.files["/sessions/child.jsonl"] = []byte(`{"type":"session","id":"child-id","parentSession":"fork-id"}`)
	cases := []struct {
		name              string
		openFiles         []string
		topLevelOpenFiles []string
		files             []string
		sessionID, forkID string
		want              *SessionResult
	}{
		{name: "open parent", openFiles: []string{"/sessions/child.jsonl"}, topLevelOpenFiles: []string{"/sessions/child.jsonl"}, files: []string{"/sessions/child.jsonl"}, forkID: "fork-id", want: &SessionResult{ID: "child-id"}},
		{name: "session id open", openFiles: []string{"/sessions/child.jsonl"}, sessionID: "child-id", want: &SessionResult{ID: "child-id"}},
		{name: "parent fallback stale", forkID: "fork-id", want: &SessionResult{ID: "fork-id", Stale: true}},
		{name: "explicit stale session", sessionID: "session-id", want: &SessionResult{ID: "session-id", Stale: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveForkSession(tc.openFiles, tc.topLevelOpenFiles, tc.files, tc.sessionID, tc.forkID, fsys, nil, nil)
			if got == nil || *got != *tc.want {
				t.Fatalf("ResolveForkSession() = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestReconcilesNewSessionsForEligibleTitlesOnly(t *testing.T) {
	before := map[string]struct{}{"old.jsonl": {}}
	rows := ReconcileSessions(before, []string{"old.jsonl", "lane.jsonl", "other.jsonl"}, map[string]struct{}{"accounts": {}}, nil, func(path string) (any, error) {
		if path == "lane.jsonl" {
			return map[string]any{"id": "lane-id", "title": "accounts"}, nil
		}
		return map[string]any{"id": "not-used", "title": "other"}, nil
	})
	if len(rows) != 1 || rows[0] != (WaveAgent{Name: "accounts", SessionID: "lane-id"}) {
		t.Fatalf("ReconcileSessions() = %#v", rows)
	}
}

type statErrorFS struct {
	*testFS
	path string
	err  error
}

func (f statErrorFS) Stat(path string) (fs.FileInfo, error) {
	if path == f.path {
		return nil, f.err
	}
	return f.testFS.Stat(path)
}

func TestTopLevelSessionFilesErr(t *testing.T) {
	fsys := newTestFS()
	if _, err := TopLevelSessionFilesErr("/missing", fsys); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing directory error = %v", err)
	}
	fsys.files["/sessions/own.jsonl"] = []byte("session")
	fsys.files["/sessions/nested/child.jsonl"] = []byte("session")
	fsys.files["/sessions/other.txt"] = []byte("other")
	fsys.links["/sessions/dangling.jsonl"] = "/missing"
	files, err := TopLevelSessionFilesErr("/sessions", fsys)
	if err != nil || len(files) != 1 || files[0] != "/sessions/own.jsonl" {
		t.Fatalf("session files = %v, %v", files, err)
	}
	denied := statErrorFS{testFS: fsys, path: "/sessions/own.jsonl", err: fs.ErrPermission}
	if _, err := TopLevelSessionFilesErr("/sessions", denied); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("stat error = %v, want permission error", err)
	}
}

func TestPicksByMtimeNotFilenameAndSkipsBurnedSessionIDs(t *testing.T) {
	fsys := newTestFS()
	fsys.files["/sessions/new-name.jsonl"] = []byte(`{"type":"session","id":"older","title":"supervisor","cwd":"/home/example/agents"}`)
	fsys.files["/sessions/old-name.jsonl"] = []byte(`{"type":"session","id":"newer","title":"supervisor","cwd":"/home/example/agents"}`)
	fsys.times["/sessions/new-name.jsonl"] = time.UnixMilli(1)
	fsys.times["/sessions/old-name.jsonl"] = time.UnixMilli(2)
	pick, err := NewestSession("/sessions", "supervisor", "/home/example/agents", map[string]struct{}{"newer": {}}, SessionDeps{FileSystem: fsys})
	if err != nil || pick == nil || pick.ID != "older" {
		t.Fatalf("NewestSession() = %#v, %v; want older session", pick, err)
	}
}

const (
	testSessionRoot = "/home/example/.omp/agent/sessions"
	testHome        = "/home/example"
	testSeatCWD     = "/home/example/agents/workspaces/seat"
)

// seatAndParentSessionDirs derives the seat cwd's session directory and its parent cwd's
// directory, where a continued seat session is recorded.
func seatAndParentSessionDirs() (seatDir, parentDir string) {
	return SessionDirFor(testSeatCWD, testHome, testSessionRoot),
		SessionDirFor(filepath.Dir(testSeatCWD), testHome, testSessionRoot)
}

func TestNewestSessionAttributesContinuedSessionRecordedAtParentCWD(t *testing.T) {
	seatDir, parentDir := seatAndParentSessionDirs()
	fsys := newTestFS()
	fsys.files[filepath.Join(seatDir, "old.jsonl")] = []byte(`{"type":"session","id":"stale","title":"seat","cwd":"/home/example/agents/workspaces/seat"}`)
	fsys.files[filepath.Join(parentDir, "live.jsonl")] = []byte(`{"type":"session","id":"live","title":"seat","cwd":"/home/example/agents/workspaces"}`)
	fsys.times[filepath.Join(seatDir, "old.jsonl")] = time.UnixMilli(1)
	fsys.times[filepath.Join(parentDir, "live.jsonl")] = time.UnixMilli(2)
	pick, err := NewestSeatSession(testSeatCWD, testHome, testSessionRoot, "seat", nil, SessionDeps{FileSystem: fsys})
	if err != nil || pick == nil || pick.ID != "live" {
		t.Fatalf("NewestSeatSession() = %#v, %v; want live continued session", pick, err)
	}
}

func TestNewestSessionIgnoresParentCWDSessionWithOtherTitle(t *testing.T) {
	seatDir, parentDir := seatAndParentSessionDirs()
	fsys := newTestFS()
	fsys.files[filepath.Join(seatDir, "old.jsonl")] = []byte(`{"type":"session","id":"stale","title":"seat","cwd":"/home/example/agents/workspaces/seat"}`)
	fsys.files[filepath.Join(parentDir, "other.jsonl")] = []byte(`{"type":"session","id":"other","title":"foreman","cwd":"/home/example/agents/workspaces"}`)
	fsys.times[filepath.Join(seatDir, "old.jsonl")] = time.UnixMilli(1)
	fsys.times[filepath.Join(parentDir, "other.jsonl")] = time.UnixMilli(2)
	pick, err := NewestSeatSession(testSeatCWD, testHome, testSessionRoot, "seat", nil, SessionDeps{FileSystem: fsys})
	if err != nil || pick == nil || pick.ID != "stale" {
		t.Fatalf("NewestSeatSession() = %#v, %v; want stale seat session, not the other-title parent session", pick, err)
	}
}

func TestNewestSessionIgnoresOlderParentCWDSession(t *testing.T) {
	seatDir, parentDir := seatAndParentSessionDirs()
	fsys := newTestFS()
	fsys.files[filepath.Join(seatDir, "old.jsonl")] = []byte(`{"type":"session","id":"stale","title":"seat","cwd":"/home/example/agents/workspaces/seat"}`)
	fsys.files[filepath.Join(parentDir, "parent.jsonl")] = []byte(`{"type":"session","id":"parent","title":"seat","cwd":"/home/example/agents/workspaces"}`)
	fsys.times[filepath.Join(seatDir, "old.jsonl")] = time.UnixMilli(2)
	fsys.times[filepath.Join(parentDir, "parent.jsonl")] = time.UnixMilli(1)
	pick, err := NewestSeatSession(testSeatCWD, testHome, testSessionRoot, "seat", nil, SessionDeps{FileSystem: fsys})
	if err != nil || pick == nil || pick.ID != "stale" {
		t.Fatalf("NewestSeatSession() = %#v, %v; want seat session, not the older parent session", pick, err)
	}
}

func TestNewestSessionSeatCWDSessionWinsMtimeTie(t *testing.T) {
	seatDir, parentDir := seatAndParentSessionDirs()
	fsys := newTestFS()
	fsys.files[filepath.Join(seatDir, "old.jsonl")] = []byte(`{"type":"session","id":"stale","title":"seat","cwd":"/home/example/agents/workspaces/seat"}`)
	fsys.files[filepath.Join(parentDir, "parent.jsonl")] = []byte(`{"type":"session","id":"parent","title":"seat","cwd":"/home/example/agents/workspaces"}`)
	fsys.times[filepath.Join(seatDir, "old.jsonl")] = time.UnixMilli(5)
	fsys.times[filepath.Join(parentDir, "parent.jsonl")] = time.UnixMilli(5)
	pick, err := NewestSeatSession(testSeatCWD, testHome, testSessionRoot, "seat", nil, SessionDeps{FileSystem: fsys})
	if err != nil || pick == nil || pick.ID != "stale" {
		t.Fatalf("NewestSeatSession() = %#v, %v; want seat-cwd session on an mtime tie", pick, err)
	}
}

func TestNewestSeatSessionMatchesTitleExactlyUnderSharedParent(t *testing.T) {
	// Two seats share the parent folder and their titles differ only by a suffix, so each
	// seat must attach only its own continued session, never a prefix or substring match.
	const parentCWD = "/home/example/agents/workspaces"
	parentDir := SessionDirFor(parentCWD, testHome, testSessionRoot)
	seatDir := SessionDirFor(testSeatCWD, testHome, testSessionRoot)
	fsys := newTestFS()
	fsys.files[filepath.Join(seatDir, "seat.jsonl")] = []byte(`{"type":"session","id":"seat-id","title":"seat","cwd":"/home/example/agents/workspaces/seat"}`)
	fsys.times[filepath.Join(seatDir, "seat.jsonl")] = time.UnixMilli(1)
	reviewCWD := parentCWD + "/seat-review"
	reviewSeatDir := SessionDirFor(reviewCWD, testHome, testSessionRoot)
	fsys.files[filepath.Join(reviewSeatDir, "review-seat.jsonl")] = []byte(`{"type":"session","id":"review-seat-id","title":"seat-review","cwd":"/home/example/agents/workspaces/seat-review"}`)
	fsys.times[filepath.Join(reviewSeatDir, "review-seat.jsonl")] = time.UnixMilli(1)
	fsys.files[filepath.Join(parentDir, "parent-seat.jsonl")] = []byte(`{"type":"session","id":"parent-seat-id","title":"seat","cwd":"/home/example/agents/workspaces"}`)
	fsys.files[filepath.Join(parentDir, "parent-review.jsonl")] = []byte(`{"type":"session","id":"parent-review-id","title":"seat-review","cwd":"/home/example/agents/workspaces"}`)
	fsys.times[filepath.Join(parentDir, "parent-seat.jsonl")] = time.UnixMilli(2)
	fsys.times[filepath.Join(parentDir, "parent-review.jsonl")] = time.UnixMilli(3)
	deps := SessionDeps{FileSystem: fsys}
	seat, err := NewestSeatSession(testSeatCWD, testHome, testSessionRoot, "seat", nil, deps)
	if err != nil || seat == nil || seat.ID != "parent-seat-id" {
		t.Fatalf("seat = %#v, %v; want its own parent-seat-id session", seat, err)
	}
	review, err := NewestSeatSession(reviewCWD, testHome, testSessionRoot, "seat-review", nil, deps)
	if err != nil || review == nil || review.ID != "parent-review-id" {
		t.Fatalf("seat-review = %#v, %v; want its own parent-review-id session", review, err)
	}
}

func TestRecognizesOnlyALiveNamedZellijSession(t *testing.T) {
	cases := []struct {
		name    string
		session string
		text    string
		want    bool
	}{
		{name: "live configured session", session: "viewer", text: "viewer [Created 1h ago]\nother", want: true},
		{name: "exited configured session", session: "viewer", text: "viewer [EXITED]\nother"},
		{name: "prefix sibling of configured session", session: "viewer", text: "viewer-old [Created]"},
		{name: "other session is not the configured one", session: "viewer", text: "devbox [Created 1h ago]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SessionLive(tc.text, tc.session); got != tc.want {
				t.Fatalf("SessionLive(%q) = %t, want %t", tc.session, got, tc.want)
			}
		})
	}
}

func TestClassifiesFailedCallsIncompleteSweepsRetryStatesAndReadyRetryWait(t *testing.T) {
	cases := []struct {
		name   string
		status any
		kind   ReadinessKind
	}{
		{name: "unavailable call", status: nil, kind: ReadinessWaiting},
		{name: "running manager", status: map[string]any{"state": "running", "lastSweep": map[string]any{"completedAt": "now"}}, kind: ReadinessWaiting},
		{name: "sweep error", status: map[string]any{"state": "idle", "lastSweep": map[string]any{"error": "failed", "completedAt": "now"}}, kind: ReadinessBlocked},
		{name: "failed manager", status: map[string]any{"state": "failed", "lastSweep": map[string]any{"completedAt": "now"}}, kind: ReadinessBlocked},
		{name: "retry wait", status: map[string]any{"state": "retry-wait", "lastSweep": map[string]any{"completedAt": "now"}, "failures": []any{}}, kind: ReadinessReady},
		{name: "retrying manager", status: map[string]any{"state": "retrying", "lastSweep": map[string]any{"completedAt": "now"}, "failures": []any{}}, kind: ReadinessWaiting},
		{name: "alias retry", status: map[string]any{"state": "idle", "lastSweep": map[string]any{"completedAt": "now"}, "failures": []any{map[string]any{"alias": "supervisor", "phase": "retry-scheduled"}}}, kind: ReadinessWaiting},
		{name: "alias refused", status: map[string]any{"state": "idle", "lastSweep": map[string]any{"completedAt": "now"}, "failures": []any{map[string]any{"alias": "supervisor", "disposition": "refused"}}}, kind: ReadinessBlocked},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyStatus(tc.status, "supervisor"); got.Kind != tc.kind {
				t.Fatalf("ClassifyStatus() = %#v, want kind %q", got, tc.kind)
			}
		})
	}
}

func TestClassifiesTerminalizingAliasRecovery(t *testing.T) {
	status := map[string]any{"staticReconciliation": map[string]any{
		"state":     "idle",
		"lastSweep": map[string]any{"completedAt": "done"},
		"failures": []any{map[string]any{
			"alias":       "infra",
			"phase":       "terminalizing",
			"disposition": "retrying",
		}},
	}}
	if got := ClassifyStatus(status, "infra"); got.Kind != ReadinessWaiting || got.Why != "supervisor-retrying" {
		t.Fatalf("ClassifyStatus(infra) = %#v, want alias retry waiting", got)
	}
	if got := ClassifyStatus(status, "supervisor"); got.Kind != ReadinessReady {
		t.Fatalf("ClassifyStatus(supervisor) = %#v, want ready", got)
	}
}

func TestWaitsByConditionPollWithInjectedClockAndSleep(t *testing.T) {
	now := time.Unix(0, 0)
	attempts := 0
	got := WaitReady(t.Context(), ReadyDeps{
		Status: func(context.Context) (any, error) {
			attempts++
			if attempts < 3 {
				return map[string]any{}, nil
			}
			return map[string]any{"state": "idle", "lastSweep": map[string]any{"completedAt": "done"}}, nil
		},
		Now: func() time.Time { return now },
		Sleep: func(_ context.Context, delay time.Duration) error {
			now = now.Add(delay)
			return nil
		},
		Deadline: 10 * time.Millisecond,
		Poll:     2 * time.Millisecond,
	})
	if got.Kind != ReadinessReady || attempts != 3 {
		t.Fatalf("WaitReady() = %#v after %d attempts", got, attempts)
	}
}

func TestManagerReadyIgnoresPerAliasFailures(t *testing.T) {
	status := map[string]any{"state": "idle", "lastSweep": map[string]any{"completedAt": "now"}, "failures": []any{map[string]any{"alias": "supervisor", "disposition": "refused"}}}
	if got := ManagerReady(status); got.Kind != ReadinessReady {
		t.Fatalf("ManagerReady() = %#v", got)
	}
}

func TestReadsExpiresAndRemovesHolds(t *testing.T) {
	fsys := newTestFS()
	fsys.files["/hold"] = []byte(`{"until":"2026-11-01T00:01:00Z","reason":"operator"}`)
	now, _ := time.Parse(time.RFC3339, "2026-11-01T00:00:00Z")
	hold, expired, err := ReadHold("/hold", now, fsys)
	if err != nil || expired || hold == nil || hold.Reason != "operator" || hold.Until != "2026-11-01T00:01:00.000Z" {
		t.Fatalf("ReadHold() = %#v, %t, %v", hold, expired, err)
	}
	later, _ := time.Parse(time.RFC3339, "2026-11-01T00:02:00Z")
	_, expired, err = ReadHold("/hold", later, fsys)
	if err != nil || !expired {
		t.Fatalf("expired ReadHold() = %t, %v", expired, err)
	}
	if err := ReleaseHold("/hold", fsys); err != nil {
		t.Fatal(err)
	}
	if err := ReleaseHold("/hold", fsys); err != nil {
		t.Fatalf("releasing missing hold: %v", err)
	}
}

func TestRestartRequestStorage(t *testing.T) {
	fsys := newTestFS()
	now, err := time.Parse(time.RFC3339, "2026-11-03T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	fsys.files["/restart"] = []byte(`{"at":"2026-11-03T00:00:00Z","reason":"test"}`)
	request, stale, err := ReadRestartRequest("/restart", now, fsys)
	if err != nil || stale || request == nil || request.Cause != "wedge" {
		t.Fatalf("ReadRestartRequest() = %#v, %t, %v", request, stale, err)
	}
	fsys.files["/restart"] = []byte(`{"at":"2026-11-03T00:00:00Z","reason":"rss","cause":"rss","identity":"123:456"}`)
	request, stale, err = ReadRestartRequest("/restart", now, fsys)
	if err != nil || stale || request == nil || request.Cause != "rss" || request.Identity != "123:456" {
		t.Fatalf("RSS ReadRestartRequest() = %#v, %t, %v", request, stale, err)
	}
	for _, text := range []string{
		`{"at":"2026-11-03T00:00:00Z","reason":"rss","cause":"rss"}`,
		`{"at":"2026-11-03T00:00:00Z","reason":"test","cause":"other"}`,
	} {
		fsys.files["/restart"] = []byte(text)
		_, stale, err = ReadRestartRequest("/restart", now, fsys)
		if err != nil || !stale {
			t.Fatalf("ReadRestartRequest(%s) stale=%t err=%v", text, stale, err)
		}
	}
}

func TestAtomicStateWriteAndMalformedState(t *testing.T) {
	fsys := newTestFS()
	state := EmptyState()
	if err := WriteState("/state/state.json", state, fsys); err != nil {
		t.Fatal(err)
	}
	got, valid := ReadState("/state/state.json", fsys)
	if !valid || len(got.Spawns) != 0 || len(got.Burned) != 0 {
		t.Fatalf("ReadState() = %#v, %t", got, valid)
	}
	if _, ok := fsys.files["/state/state.json"]; !ok {
		t.Fatal("atomic state was not renamed into place")
	}
	for path := range fsys.files {
		if strings.HasSuffix(path, ".tmp") {
			t.Fatalf("temporary file remains: %s", path)
		}
	}
	fsys.files["/state/state.json"] = []byte("{")
	if _, valid := ReadState("/state/state.json", fsys); valid {
		t.Fatal("malformed state was accepted")
	}
}

func TestStateWriteFailurePreservesError(t *testing.T) {
	fsys := newTestFS()
	want := errors.New("write failure")
	bad := failingWriteFS{FileSystem: fsys, err: want}
	if err := WriteState("/state/state.json", EmptyState(), bad); !errors.Is(err, want) {
		t.Fatalf("WriteState() error = %v, want wrapped write error", err)
	}
}

type failingWriteFS struct {
	FileSystem
	err error
}

func (f failingWriteFS) WriteFile(string, []byte, fs.FileMode) error { return f.err }
