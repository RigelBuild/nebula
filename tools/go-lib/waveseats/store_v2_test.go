// SPDX-License-Identifier: MIT OR Apache-2.0

package waveseats

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type memoryStoreFS struct {
	files      map[string][]byte
	events     []string
	failRename bool
	failWrite  bool
}

func newMemoryStoreFS() *memoryStoreFS { return &memoryStoreFS{files: map[string][]byte{}} }

func (m *memoryStoreFS) ReadFile(path string) ([]byte, error) {
	data, ok := m.files[path]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return append([]byte(nil), data...), nil
}

func (m *memoryStoreFS) ReadDir(string) ([]fs.DirEntry, error) {
	return nil, errors.New("not implemented")
}

func (m *memoryStoreFS) Stat(string) (fs.FileInfo, error) { return nil, errors.New("not implemented") }

func (m *memoryStoreFS) Readlink(string) (string, error) { return "", errors.New("not implemented") }

func (m *memoryStoreFS) WriteFile(path string, data []byte, _ fs.FileMode) error {
	m.events = append(m.events, "write "+path)
	if m.failWrite {
		return errors.New("interrupted write")
	}
	m.files[path] = append([]byte(nil), data...)
	return nil
}

func (m *memoryStoreFS) CreateExclusive(path string, data []byte, _ fs.FileMode) error {
	if _, ok := m.files[path]; ok {
		return fs.ErrExist
	}
	m.events = append(m.events, "create-exclusive "+path)
	m.files[path] = append([]byte(nil), data...)
	return nil
}

func (m *memoryStoreFS) AppendFile(path string, data []byte, _ fs.FileMode) error {
	m.events = append(m.events, "append "+path)
	m.files[path] = append(m.files[path], data...)
	return nil
}

func (m *memoryStoreFS) Rename(from, to string) error {
	m.events = append(m.events, "rename "+from+" "+to)
	if m.failRename {
		return errors.New("crash before rename")
	}
	data, ok := m.files[from]
	if !ok {
		return fmt.Errorf("missing: %s", from)
	}
	m.files[to] = data
	delete(m.files, from)
	return nil
}

func (m *memoryStoreFS) MkdirAll(string, fs.FileMode) error { return nil }

func (m *memoryStoreFS) Remove(path string) error { delete(m.files, path); return nil }

func (m *memoryStoreFS) FsyncPath(path string) error {
	m.events = append(m.events, "fsync "+path)
	return nil
}

func (m *memoryStoreFS) FsyncDir(path string) error {
	m.events = append(m.events, "fsyncdir "+path)
	return nil
}

func TestCommitLocked(t *testing.T) {
	operation := Operation{OperationID: "test", Principal: Principal{Kind: "operator", Operator: "test"}}
	t.Run("appends and fsyncs journal before replacing list", func(t *testing.T) { testCommitCase1(t, operation) })
	t.Run("bumps revision", func(t *testing.T) { testCommitCase2(t, operation) })
	t.Run("retains operation results across journal compactions", func(t *testing.T) { testCommitCase3(t, operation) })
}

func TestCommitImportedSeatListV1CreatesBackupBeforeCommit(t *testing.T) {
	const path = "/state/wave-seats.json"
	legacy := []byte(`{"version":1,"seats":[]}`)
	filesystem := newMemoryStoreFS()
	filesystem.files[path] = append([]byte(nil), legacy...)
	operation := Operation{OperationID: "import", Principal: Principal{Kind: operatorPrincipal, Operator: "test"}}

	_, err := CommitImportedSeatListV1Locked(path, func(string) (SeatListV2, error) {
		return SeatListV2{GenerationBase: 7, Seats: []SeatEntryV2{}}, nil
	}, operation, filesystem)
	if err != nil {
		t.Fatal(err)
	}
	if got := filesystem.files[path+".v1"]; string(got) != string(legacy) {
		t.Fatalf("backup = %q, want original bytes %q", got, legacy)
	}
	backupAt, commitAt := -1, -1
	for index, event := range filesystem.events {
		if event == "create-exclusive "+path+".v1" {
			backupAt = index
		}
		if event == "append "+JournalPath(path) {
			commitAt = index
		}
	}
	if backupAt < 0 || backupAt >= commitAt {
		t.Fatalf("backup event %d, commit event %d: %v", backupAt, commitAt, filesystem.events)
	}
}

func TestCommitImportedSeatListV1AcceptsMatchingBackupOnRetry(t *testing.T) {
	const path = "/state/wave-seats.json"
	legacy := []byte(`{"version":1,"seats":[]}`)
	filesystem := newMemoryStoreFS()
	filesystem.files[path] = append([]byte(nil), legacy...)
	filesystem.files[path+".v1"] = append([]byte(nil), legacy...)
	operation := Operation{OperationID: "import", Principal: Principal{Kind: operatorPrincipal, Operator: "test"}}

	_, err := CommitImportedSeatListV1Locked(path, func(string) (SeatListV2, error) {
		return SeatListV2{Seats: []SeatEntryV2{}}, nil
	}, operation, filesystem)
	if err != nil {
		t.Fatalf("retry with matching backup: %v", err)
	}
	if got := filesystem.files[path+".v1"]; !bytes.Equal(got, legacy) {
		t.Fatalf("backup changed on retry: %q", got)
	}
	fsyncAt, appendAt, renamed := -1, -1, false
	for index, event := range filesystem.events {
		switch {
		case strings.HasPrefix(event, "create-exclusive "):
			t.Fatalf("retry recreated the backup: %v", filesystem.events)
		case event == "fsync "+path+".v1":
			fsyncAt = index
		case event == "append "+JournalPath(path) && appendAt < 0:
			appendAt = index
		case strings.HasPrefix(event, "rename ") && strings.HasSuffix(event, " "+path):
			renamed = true
		}
	}
	if fsyncAt < 0 || appendAt < 0 || fsyncAt > appendAt || !renamed {
		t.Fatalf("want backup fsync before journal append and a list rename: %v", filesystem.events)
	}
}

func TestCommitImportedSeatListV1RejectsDifferentBackupOnRetry(t *testing.T) {
	const path = "/state/wave-seats.json"
	filesystem := newMemoryStoreFS()
	filesystem.files[path] = []byte(`{"version":1,"seats":[]}`)
	filesystem.files[path+".v1"] = []byte("different backup")
	operation := Operation{OperationID: "import", Principal: Principal{Kind: operatorPrincipal, Operator: "test"}}

	_, err := CommitImportedSeatListV1Locked(path, func(string) (SeatListV2, error) {
		return SeatListV2{Seats: []SeatEntryV2{}}, nil
	}, operation, filesystem)
	if err == nil || !strings.Contains(err.Error(), "does not match seat list") {
		t.Fatalf("retry error = %v, want mismatched backup error", err)
	}
	if len(filesystem.events) != 0 {
		t.Fatalf("commit started for mismatched backup: %v", filesystem.events)
	}
}

func TestLoadSeatListLockedRecovery(t *testing.T) {
	const path = "/state/wave-seats.json"
	op := Operation{OperationID: "test", Principal: Principal{Kind: "operator", Operator: "test"}}
	t.Run("keeps missing error", func(t *testing.T) { testLoadRecoveryCase1(t, path) })
	t.Run("rolls forward unfinished commit", func(t *testing.T) { testLoadRecoveryCase2(t, path, op) })
	t.Run("rolls forward journaled import over v1 list", func(t *testing.T) { testLoadRecoveryCase3(t, path, op) })
	t.Run("leaves list alone at journal revision", func(t *testing.T) { testLoadRecoveryCase4(t, path, op) })
}

func TestParseJournalRecovery(t *testing.T) {
	operation := Operation{OperationID: "test", Principal: Principal{Kind: "operator", Operator: "test"}}
	goodBytes, err := json.Marshal(JournalRecord{Operation: operation, Revision: 1, Digest: DigestText("{}"), Text: "{}"})
	if err != nil {
		t.Fatal(err)
	}
	good := string(goodBytes)
	t.Run("drops torn final line", func(t *testing.T) { testJournalRecoveryCase1(t, good) })
	t.Run("rejects bad middle line", func(t *testing.T) { testJournalRecoveryCase2(t, good) })
	t.Run("failed repair write leaves original journal intact", func(t *testing.T) { testJournalRecoveryCase3(t, operation) })
	t.Run("repairs torn tail before another commit and compacts", func(t *testing.T) { testJournalRecoveryCase4(t, operation) })
}

func TestStoreLockPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wave-seats.json")
	if got, want := SeatListLockPath(path), filepath.Join(filepath.Dir(path), "wave-seats.lock"); got != want {
		t.Fatalf("lock path=%q, want %q", got, want)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := WithSeatListLock(ctx, path, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func testCommitCase1(t *testing.T, operation Operation) {
	t.Helper()
	fs := newMemoryStoreFS()
	_, err := CommitLocked("/state/wave-seats.json", v2TestList(0), []SeatEntryV2{v2TestSeat("alpha")}, operation, fs)
	if err != nil {
		t.Fatal(err)
	}
	journalAt, renameAt := -1, -1
	for i, event := range fs.events {
		if event == "append "+JournalPath("/state/wave-seats.json") {
			journalAt = i
		}
		if strings.HasPrefix(event, "rename ") && renameAt < 0 {
			renameAt = i
		}
	}
	if journalAt < 0 || journalAt >= renameAt {
		t.Fatalf("journal append %d, rename %d: %v", journalAt, renameAt, fs.events)
	}
	if !containsString(fs.events, "fsync "+JournalPath("/state/wave-seats.json")) {
		t.Fatalf("journal not fsynced: %v", fs.events)
	}
}

func testCommitCase2(t *testing.T, operation Operation) {
	t.Helper()
	fs := newMemoryStoreFS()
	got, err := CommitLocked("/state/wave-seats.json", v2TestList(4), []SeatEntryV2{v2TestSeat("a")}, operation, fs)
	if err != nil || got.Revision != 5 {
		t.Fatalf("commit = %#v, %v", got, err)
	}
	loaded, err := LoadSeatListLocked("/state/wave-seats.json", fs)
	if err != nil || loaded.Revision != 5 {
		t.Fatalf("load = %#v, %v", loaded, err)
	}
}

func testCommitCase3(t *testing.T, operation Operation) {
	t.Helper()
	fs := newMemoryStoreFS()
	original := Operation{OperationID: "first", Principal: operation.Principal, Request: new("request"), Name: new("alpha")}
	current, err := CommitLocked("/state/wave-seats.json", v2TestList(0), []SeatEntryV2{v2TestSeat("alpha")}, original, fs)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 260 {
		next := Operation{OperationID: strconv.Itoa(i), Principal: operation.Principal, Request: new(strconv.Itoa(i)), Name: new("alpha")}
		current, err = CommitLocked("/state/wave-seats.json", current, []SeatEntryV2{v2TestSeat("alpha")}, next, fs)
		if err != nil {
			t.Fatal(err)
		}
	}
	records, err := ParseJournalRecords(string(fs.files[JournalPath("/state/wave-seats.json")]))
	if err != nil || len(records) != 1 {
		t.Fatalf("journal records=%d err=%v", len(records), err)
	}
	result, err := ReplayLocked("/state/wave-seats.json", original, "request", fs)
	if err != nil || result == nil || result.Revision != 1 {
		t.Fatalf("replay=%#v err=%v", result, err)
	}
	if current.Revision != 261 {
		t.Fatalf("revision=%d", current.Revision)
	}
}

func testLoadRecoveryCase1(t *testing.T, path string) {
	t.Helper()
	_, err := LoadSeatListLocked(path, newMemoryStoreFS())
	if err == nil || !strings.Contains(err.Error(), "seat list "+path+" is missing") {
		t.Fatalf("error=%v", err)
	}
}

func testLoadRecoveryCase2(t *testing.T, path string, op Operation) {
	t.Helper()
	fs := newMemoryStoreFS()
	base, err := CommitLocked(path, v2TestList(0), []SeatEntryV2{}, op, fs)
	if err != nil {
		t.Fatal(err)
	}
	fs.failRename = true
	_, err = CommitLocked(path, base, []SeatEntryV2{v2TestSeat("beta")}, op, fs)
	if err == nil || !strings.Contains(err.Error(), "crash before rename") {
		t.Fatalf("commit error=%v", err)
	}
	fs.failRename = false
	loaded, err := LoadSeatListLocked(path, fs)
	if err != nil || loaded.Revision != 2 || loaded.Seats[0].Name != "beta" {
		t.Fatalf("load=%#v, %v", loaded, err)
	}
}

func testLoadRecoveryCase3(t *testing.T, path string, op Operation) {
	t.Helper()
	fs := newMemoryStoreFS()
	fs.files[path] = []byte(`{"version":1,"seats":[]}`)
	fs.failRename = true
	_, err := CommitLocked(path, v2TestList(0), []SeatEntryV2{v2TestSeat("beta")}, op, fs)
	if err == nil {
		t.Fatal("commit unexpectedly succeeded")
	}
	fs.failRename = false
	loaded, err := LoadSeatListLocked(path, fs)
	if err != nil || loaded.Seats[0].Name != "beta" {
		t.Fatalf("load=%#v, %v", loaded, err)
	}
}

func testLoadRecoveryCase4(t *testing.T, path string, op Operation) {
	t.Helper()
	fs := newMemoryStoreFS()
	_, err := CommitLocked(path, v2TestList(0), []SeatEntryV2{v2TestSeat("gamma")}, op, fs)
	if err != nil {
		t.Fatal(err)
	}
	fs.events = nil
	_, err = LoadSeatListLocked(path, fs)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range fs.events {
		if strings.HasPrefix(event, "rename ") {
			t.Fatalf("unexpected rename: %v", fs.events)
		}
	}
}

func testJournalRecoveryCase1(t *testing.T, good string) {
	t.Helper()
	records, _, err := ParseJournal(good + "\n{\"revision\":2")
	if err != nil || len(records) != 1 {
		t.Fatalf("records=%v err=%v", records, err)
	}
}

func testJournalRecoveryCase2(t *testing.T, good string) {
	t.Helper()
	if _, _, err := ParseJournal(good + "\nbad\n" + good + "\n"); err == nil {
		t.Fatal("bad middle line accepted")
	}
}

func testJournalRecoveryCase3(t *testing.T, operation Operation) {
	t.Helper()
	fs := newMemoryStoreFS()
	_, err := CommitLocked("/state/wave-seats.json", v2TestList(0), []SeatEntryV2{v2TestSeat("alpha")}, operation, fs)
	if err != nil {
		t.Fatal(err)
	}
	path := JournalPath("/state/wave-seats.json")
	fs.files[path] = append(fs.files[path], []byte(`{"revision":`)...)
	original := append([]byte(nil), fs.files[path]...)
	fs.failWrite = true
	_, err = LoadSeatListLocked("/state/wave-seats.json", fs)
	if err == nil || !strings.Contains(err.Error(), "interrupted write") {
		t.Fatalf("load error=%v", err)
	}
	if string(fs.files[path]) != string(original) {
		t.Fatal("original journal changed")
	}
}

func testJournalRecoveryCase4(t *testing.T, operation Operation) {
	t.Helper()
	fs := newMemoryStoreFS()
	first, err := CommitLocked("/state/wave-seats.json", v2TestList(0), []SeatEntryV2{v2TestSeat("alpha")}, operation, fs)
	if err != nil {
		t.Fatal(err)
	}
	path := JournalPath("/state/wave-seats.json")
	fs.files[path] = append(fs.files[path], []byte(`{"revision":`)...)
	fs.events = nil
	recovered, err := LoadSeatListLocked("/state/wave-seats.json", fs)
	if err != nil || recovered.Revision != first.Revision {
		t.Fatalf("recovered=%#v err=%v", recovered, err)
	}
	tempFsync, appendAt := -1, -1
	for i, event := range fs.events {
		if event == "fsync "+path+".tmp" {
			tempFsync = i
		}
		if event == "append "+path {
			appendAt = i
		}
	}
	if tempFsync < 0 || appendAt != -1 {
		t.Fatalf("torn tail was not durably repaired on load: %v", fs.events)
	}
	fs.events = nil
	_, err = CommitLocked("/state/wave-seats.json", recovered, []SeatEntryV2{v2TestSeat("beta")}, operation, fs)
	if err != nil {
		t.Fatal(err)
	}
	records, err := ParseJournalRecords(string(fs.files[path]))
	if err != nil || len(records) != 1 || records[0].Revision != 2 {
		t.Fatalf("records=%#v err=%v", records, err)
	}
	if !containsString(fs.events, "fsync "+path) {
		t.Fatalf("new journal was not fsynced: %v", fs.events)
	}

}
