package phase

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCutoverRecordRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cutover.json")
	want := CutoverRecord{RunID: "run-123", State: "committing"}
	if err := WriteCutover(path, want); err != nil {
		t.Fatalf("WriteCutover() error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written record: %v", err)
	}
	if got, expected := string(data), `{"runID":"run-123","state":"committing"}`+"\n"; got != expected {
		t.Fatalf("written JSON = %q, want %q", got, expected)
	}

	got, err := ReadCutover(path)
	if err != nil {
		t.Fatalf("ReadCutover() error = %v", err)
	}
	if got != want {
		t.Fatalf("ReadCutover() = %#v, want %#v", got, want)
	}
}

func TestRestorePrepareRecordRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restore-prepare.json")
	want := RestorePrepareRecord{
		BackupData:    "backup-data-2026-11-15",
		BackupControl: "backup-control-2026-11-15",
		Nonce:         "nonce-123",
	}
	if err := WriteRestorePrepare(path, want); err != nil {
		t.Fatalf("WriteRestorePrepare() error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written record: %v", err)
	}
	if got, expected := string(data), `{"backupData":"backup-data-2026-11-15","backupControl":"backup-control-2026-11-15","nonce":"nonce-123"}`+"\n"; got != expected {
		t.Fatalf("written JSON = %q, want %q", got, expected)
	}

	got, err := ReadRestorePrepare(path)
	if err != nil {
		t.Fatalf("ReadRestorePrepare() error = %v", err)
	}
	if got != want {
		t.Fatalf("ReadRestorePrepare() = %#v, want %#v", got, want)
	}
}

func TestWriteCutoverRejectsInvalidRecords(t *testing.T) {
	tests := []struct {
		name   string
		record CutoverRecord
	}{
		{name: "empty run id", record: CutoverRecord{State: "started"}},
		{name: "empty state", record: CutoverRecord{RunID: "run-123"}},
		{name: "unknown state", record: CutoverRecord{RunID: "run-123", State: "failed"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cutover.json")
			if err := WriteCutover(path, test.record); err == nil {
				t.Fatal("WriteCutover() error = nil, want validation error")
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("invalid record final path stat error = %v, want not-exist", err)
			}
		})
	}
}

func TestReadCutoverRejectsInvalidRecords(t *testing.T) {
	tests := []struct {
		name string
		json string
	}{
		{name: "empty run id", json: `{"runID":"","state":"started"}`},
		{name: "empty state", json: `{"runID":"run-123","state":""}`},
		{name: "unknown state", json: `{"runID":"run-123","state":"failed"}`},
		{name: "missing state", json: `{"runID":"run-123"}`},
		{name: "unknown field", json: `{"runID":"run-123","state":"started","extra":true}`},
		{name: "corrupt JSON", json: `{"runID":"run-123","state":`},
		{name: "trailing JSON value", json: `{"runID":"run-123","state":"started"}{}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cutover.json")
			if err := os.WriteFile(path, []byte(test.json), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadCutover(path); err == nil {
				t.Fatal("ReadCutover() error = nil, want validation error")
			}
		})
	}
}

func TestWriteRestorePrepareRejectsEmptyFields(t *testing.T) {
	tests := []struct {
		name   string
		record RestorePrepareRecord
	}{
		{name: "empty data backup", record: RestorePrepareRecord{BackupControl: "control", Nonce: "nonce"}},
		{name: "empty control backup", record: RestorePrepareRecord{BackupData: "data", Nonce: "nonce"}},
		{name: "empty nonce", record: RestorePrepareRecord{BackupData: "data", BackupControl: "control"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "restore-prepare.json")
			if err := WriteRestorePrepare(path, test.record); err == nil {
				t.Fatal("WriteRestorePrepare() error = nil, want validation error")
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("invalid record final path stat error = %v, want not-exist", err)
			}
		})
	}
}

func TestReadRestorePrepareRejectsInvalidRecords(t *testing.T) {
	tests := []struct {
		name string
		json string
	}{
		{name: "empty data backup", json: `{"backupData":"","backupControl":"control","nonce":"nonce"}`},
		{name: "empty control backup", json: `{"backupData":"data","backupControl":"","nonce":"nonce"}`},
		{name: "empty nonce", json: `{"backupData":"data","backupControl":"control","nonce":""}`},
		{name: "missing nonce", json: `{"backupData":"data","backupControl":"control"}`},
		{name: "unknown field", json: `{"backupData":"data","backupControl":"control","nonce":"nonce","extra":true}`},
		{name: "corrupt JSON", json: `{"backupData":"data","backupControl":`},
		{name: "trailing JSON value", json: `{"backupData":"data","backupControl":"control","nonce":"nonce"} null`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "restore-prepare.json")
			if err := os.WriteFile(path, []byte(test.json), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadRestorePrepare(path); err == nil {
				t.Fatal("ReadRestorePrepare() error = nil, want validation error")
			}
		})
	}
}

func TestReadRecordsReturnErrorsForMissingPaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.json")
	if _, err := ReadCutover(path); err == nil || !strings.Contains(err.Error(), "read cutover record") {
		t.Fatalf("ReadCutover() error = %v, want contextual read error", err)
	}
	if _, err := ReadRestorePrepare(path); err == nil || !strings.Contains(err.Error(), "read restore prepare record") {
		t.Fatalf("ReadRestorePrepare() error = %v, want contextual read error", err)
	}
}
