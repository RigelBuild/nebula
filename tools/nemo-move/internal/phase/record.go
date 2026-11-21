package phase

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// CutoverRecord stores the durable progress of a cutover attempt.
type CutoverRecord struct {
	RunID string `json:"runID"`
	State string `json:"state"`
}

// RestorePrepareRecord stores the backup identities and nonce required to commit a restore.
type RestorePrepareRecord struct {
	BackupData    string `json:"backupData"`
	BackupControl string `json:"backupControl"`
	Nonce         string `json:"nonce"`
}

// ReadCutover reads and validates a durable cutover record.
func ReadCutover(path string) (CutoverRecord, error) {
	var record CutoverRecord
	if err := readRecord(path, "cutover", &record); err != nil {
		return CutoverRecord{}, err
	}
	if err := validateCutover(record); err != nil {
		return CutoverRecord{}, fmt.Errorf("validate cutover record: %w", err)
	}
	return record, nil
}

// WriteCutover validates and atomically writes a durable cutover record.
func WriteCutover(path string, record CutoverRecord) error {
	if err := validateCutover(record); err != nil {
		return fmt.Errorf("validate cutover record: %w", err)
	}
	return writeRecord(path, record)
}

// ReadRestorePrepare reads and validates a durable restore prepare record.
func ReadRestorePrepare(path string) (RestorePrepareRecord, error) {
	var record RestorePrepareRecord
	if err := readRecord(path, "restore prepare", &record); err != nil {
		return RestorePrepareRecord{}, err
	}
	if err := validateRestorePrepare(record); err != nil {
		return RestorePrepareRecord{}, fmt.Errorf("validate restore prepare record: %w", err)
	}
	return record, nil
}

// WriteRestorePrepare validates and atomically writes a durable restore prepare record.
func WriteRestorePrepare(path string, record RestorePrepareRecord) error {
	if err := validateRestorePrepare(record); err != nil {
		return fmt.Errorf("validate restore prepare record: %w", err)
	}
	return writeRecord(path, record)
}

func readRecord(path, kind string, record any) error {
	data, err := os.ReadFile(path) //nolint:gosec // The path comes from the operator-selected control mount.
	if err != nil {
		return fmt.Errorf("read %s record: %w", kind, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(record); err != nil {
		return fmt.Errorf("decode %s record: %w", kind, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("decode %s record: trailing JSON value", kind)
		}
		return fmt.Errorf("decode %s record trailing data: %w", kind, err)
	}
	return nil
}

func writeRecord(path string, record any) error {
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal record: %w", err)
	}
	data = append(data, '\n')
	return WriteAtomic(path, Value(data))
}

func validateCutover(record CutoverRecord) error {
	if strings.TrimSpace(record.RunID) == "" {
		return fmt.Errorf("run id must not be empty")
	}
	switch record.State {
	case "started", "committing", "committed":
		return nil
	default:
		return fmt.Errorf("state %q is invalid", record.State)
	}
}

func validateRestorePrepare(record RestorePrepareRecord) error {
	if strings.TrimSpace(record.BackupData) == "" {
		return fmt.Errorf("data backup must not be empty")
	}
	if strings.TrimSpace(record.BackupControl) == "" {
		return fmt.Errorf("control backup must not be empty")
	}
	if strings.TrimSpace(record.Nonce) == "" {
		return fmt.Errorf("nonce must not be empty")
	}
	return nil
}
