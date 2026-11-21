// SPDX-License-Identifier: MIT OR Apache-2.0

package waveseats

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/RigelBuild/nebula/tools/go-lib/filelock"
)

const operatorPrincipal = "operator"

// Principal identifies the operator that owns an idempotent seat operation.
type Principal struct {
	Kind     string `json:"kind"`
	Operator string `json:"operator"`
}

// Operation contains request identity and audit digests for a seat mutation.
type Operation struct {
	OperationID       string    `json:"operationId"`
	Principal         Principal `json:"principal"`
	Request           *string   `json:"request,omitempty"`
	Name              *string   `json:"name,omitempty"`
	PolicyDigest      *string   `json:"policyDigest,omitempty"`
	EntitlementDigest *string   `json:"entitlementDigest,omitempty"`
}

// NewSeatOperation creates an operator operation identity.
func NewSeatOperation() (Operation, error) {
	operationID, err := randomUUID()
	if err != nil {
		return Operation{}, fmt.Errorf("generate operation id: %w", err)
	}
	operator := os.Getenv("USER")
	if operator == "" {
		operator = os.Getenv("LOGNAME")
	}
	if operator == "" {
		operator = operatorPrincipal
	}
	return Operation{OperationID: operationID, Principal: Principal{Kind: operatorPrincipal, Operator: operator}}, nil
}
func randomUUID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value[:])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}

// JournalRecord stores a complete serialized list so recovery can roll it forward.
type JournalRecord struct {
	Operation
	Revision uint64 `json:"revision"`
	Digest   string `json:"digest"`
	Text     string `json:"text"`
}

// StoreFileSystem is the durable I/O surface used by the journaled v2 store.
type StoreFileSystem interface {
	ReadFile(path string) ([]byte, error)
	WriteFile(path string, data []byte, perm fs.FileMode) error
	CreateExclusive(path string, data []byte, perm fs.FileMode) error
	AppendFile(path string, data []byte, perm fs.FileMode) error
	Rename(oldpath, newpath string) error
	MkdirAll(path string, perm fs.FileMode) error
	Remove(path string) error
	FsyncPath(path string) error
	FsyncDir(path string) error
}

// NodeStoreFileSystem uses the host filesystem and fsyncs durable updates.
type NodeStoreFileSystem struct{}

func (NodeStoreFileSystem) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path) // #nosec G304 -- caller supplies store paths at the host filesystem boundary.
}
func (NodeStoreFileSystem) WriteFile(path string, data []byte, perm fs.FileMode) error {
	return os.WriteFile(path, data, perm)
}
func (NodeStoreFileSystem) CreateExclusive(path string, data []byte, perm fs.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm) // #nosec G304 -- caller supplies store paths at the host filesystem boundary.
	if err != nil {
		return err
	}
	n, writeErr := file.Write(data)
	if n != len(data) && writeErr == nil {
		writeErr = io.ErrShortWrite
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return errors.Join(err, os.Remove(path))
	}
	return nil
}
func (NodeStoreFileSystem) AppendFile(path string, data []byte, perm fs.FileMode) error {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, perm) // #nosec G304 -- caller supplies store paths at the host filesystem boundary.
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	return errors.Join(writeErr, closeErr)
}
func (NodeStoreFileSystem) Rename(oldpath, newpath string) error { return os.Rename(oldpath, newpath) }
func (NodeStoreFileSystem) MkdirAll(path string, perm fs.FileMode) error {
	return os.MkdirAll(path, perm)
}
func (NodeStoreFileSystem) FsyncPath(path string) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0) // #nosec G304 -- caller supplies store paths at the host filesystem boundary.
	if err != nil {
		return err
	}
	return errors.Join(file.Sync(), file.Close())
}
func (NodeStoreFileSystem) FsyncDir(path string) error {
	file, err := os.Open(path) // #nosec G304 -- the store owns this directory path.
	if err != nil {
		return err
	}
	return errors.Join(file.Sync(), file.Close())
}

func (NodeStoreFileSystem) Remove(path string) error { return os.Remove(path) }

// JournalPath returns the NDJSON journal beside a seat-list file.
func JournalPath(listPath string) string {
	stem := strings.TrimSuffix(filepath.Base(listPath), filepath.Ext(listPath))
	return filepath.Join(filepath.Dir(listPath), stem+".journal.ndjson")
}

// SeatListLockPath returns the shared writer lock path beside the seat list.
func SeatListLockPath(listPath string) string {
	return filepath.Join(filepath.Dir(listPath), "wave-seats.lock")
}

// WithSeatListLock runs work under the shared lock used by all seat-list writers.
func WithSeatListLock(ctx context.Context, listPath string, work func() error) error {
	return filelock.With(ctx, SeatListLockPath(listPath), work)
}

// ParseJournal parses complete journal records and ignores one torn trailing record.
func ParseJournal(text string) ([]JournalRecord, int, error) {
	lines := strings.Split(text, "\n")
	records := make([]JournalRecord, 0, len(lines))
	length := 0
	for index, line := range lines {
		if line == "" && index == len(lines)-1 {
			break
		}
		var record JournalRecord
		if index == len(lines)-1 {
			break
		}
		err := json.Unmarshal([]byte(line), &record)
		if err == nil && (record.Revision == 0 || record.Digest == "" || record.Text == "" || record.OperationID == "" || record.Principal.Kind != operatorPrincipal || record.Principal.Operator == "") {
			err = errors.New("seat journal: malformed record")
		}
		if err == nil && DigestText(record.Text) != record.Digest {
			err = fmt.Errorf("seat journal: digest mismatch at revision %d", record.Revision)
		}
		if err != nil {
			if index < len(lines)-2 || (index == len(lines)-2 && lines[len(lines)-1] != "") {
				return nil, 0, err
			}
			break
		}
		records = append(records, record)
		length += len(line) + 1
	}
	return records, length, nil
}

// ParseJournalRecords returns only the complete records in a journal.
func ParseJournalRecords(text string) ([]JournalRecord, error) {
	records, _, err := ParseJournal(text)
	return records, err
}

// LoadSeatListLocked repairs the journal and loads the current v2 list. Caller holds the seat lock.
func LoadSeatListLocked(listPath string, filesystem StoreFileSystem) (SeatListV2, error) {
	if err := rollForward(listPath, filesystem); err != nil {
		return SeatListV2{}, err
	}
	text, err := readOptional(filesystem, listPath)
	if err != nil {
		return SeatListV2{}, err
	}
	if text == nil {
		return SeatListV2{}, fmt.Errorf("seat list %s is missing", listPath)
	}
	list, err := ParseSeatListV2(string(text))
	if err == nil {
		return list, nil
	}
	var value struct {
		Version int `json:"version"`
	}
	if json.Unmarshal(text, &value) == nil && value.Version == 1 {
		return SeatListV2{}, errors.New("seat list is version 1; run wave-seats import-v1")
	}
	return SeatListV2{}, err
}

func rollForward(listPath string, filesystem StoreFileSystem) error {
	journalBytes, err := readOptional(filesystem, JournalPath(listPath))
	if err != nil || journalBytes == nil {
		return err
	}
	journal := string(journalBytes)
	records, length, err := ParseJournal(journal)
	if err != nil {
		return err
	}
	if length != len(journal) {
		if err := writeAndSwap(JournalPath(listPath), []byte(journal[:length]), filesystem); err != nil {
			return err
		}
	}
	if len(records) == 0 {
		return nil
	}
	current, err := readOptional(filesystem, listPath)
	if err != nil {
		return err
	}
	currentRevision := uint64(0)
	if current != nil {
		list, parseErr := ParseSeatListV2(string(current))
		if parseErr == nil {
			currentRevision = list.Revision
		} else {
			var value struct {
				Version int `json:"version"`
			}
			if json.Unmarshal(current, &value) != nil || value.Version != 1 || records[len(records)-1].Revision != 1 {
				return parseErr
			}
		}
	}
	newest := records[len(records)-1]
	if currentRevision >= newest.Revision {
		return nil
	}
	return writeAndSwap(listPath, []byte(newest.Text), filesystem)
}

func readOptional(filesystem StoreFileSystem, path string) ([]byte, error) {
	data, err := filesystem.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

func writeAndSwap(path string, data []byte, filesystem StoreFileSystem) error {
	if err := filesystem.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := filesystem.WriteFile(temporary, data, 0o666); err != nil {
		return err
	}
	if err := filesystem.FsyncPath(temporary); err != nil {
		return err
	}
	if err := filesystem.Rename(temporary, path); err != nil {
		return err
	}
	return filesystem.FsyncDir(filepath.Dir(path))
}

// CommitLocked writes a journaled revision and compacts the journal to its latest record.
func CommitLocked(listPath string, current SeatListV2, next []SeatEntryV2, operation Operation, filesystem StoreFileSystem) (SeatListV2, error) {
	revision := current.Revision + 1
	operations := maps.Clone(current.Operations)
	order := slices.Clone(current.operationOrder)
	if operations == nil {
		operations = make(map[string]OperationResult)
	}
	if operation.Request != nil && operation.Name != nil {
		entry, found := SeatEntryV2ByName(SeatListV2{Seats: next}, *operation.Name)
		if !found {
			return SeatListV2{}, errors.New("operation target missing from commit")
		}
		keyBytes, err := json.Marshal([]string{operation.Principal.Operator, operation.OperationID})
		if err != nil {
			return SeatListV2{}, err
		}
		if _, exists := operations[string(keyBytes)]; !exists {
			order = append(order, string(keyBytes))
		}
		operations[string(keyBytes)] = OperationResult{RequestDigest: DigestText(*operation.Request), Result: SeatMutation{Revision: revision, Entry: entry, PolicyDigest: operation.PolicyDigest, EntitlementDigest: operation.EntitlementDigest}}
	}
	committed := SeatListV2{Version: 2, Revision: revision, GenerationBase: current.GenerationBase, Seats: slices.Clone(next)}
	if len(operations) != 0 {
		committed.Operations = operations
		committed.operationOrder = order
	}
	encoded, err := SerializeSeatListV2(committed)
	if err != nil {
		return SeatListV2{}, err
	}
	record := JournalRecord{Operation: operation, Revision: revision, Digest: DigestText(string(encoded)), Text: string(encoded)}
	recordText, err := json.Marshal(record)
	if err != nil {
		return SeatListV2{}, err
	}
	recordText = append(recordText, '\n')
	if err := filesystem.MkdirAll(filepath.Dir(listPath), 0o777); err != nil {
		return SeatListV2{}, err
	}
	if err := filesystem.AppendFile(JournalPath(listPath), recordText, 0o666); err != nil {
		return SeatListV2{}, err
	}
	if err := filesystem.FsyncPath(JournalPath(listPath)); err != nil {
		return SeatListV2{}, err
	}
	if err := writeAndSwap(listPath, encoded, filesystem); err != nil {
		return SeatListV2{}, err
	}
	if err := writeAndSwap(JournalPath(listPath), recordText, filesystem); err != nil {
		return SeatListV2{}, err
	}
	return committed, nil
}

// LoadSeatList loads and repairs a list while holding the shared seat-list lock.
func LoadSeatList(ctx context.Context, listPath string, filesystem StoreFileSystem) (SeatListV2, error) {
	var list SeatListV2
	err := WithSeatListLock(ctx, listPath, func() error { var err error; list, err = LoadSeatListLocked(listPath, filesystem); return err })
	return list, err
}

// ReplayLocked returns a stored result or refuses a reused operation ID. Caller holds the lock.
func ReplayLocked(listPath string, operation Operation, request string, filesystem StoreFileSystem) (*SeatMutation, error) {
	list, err := LoadSeatListLocked(listPath, filesystem)
	if err != nil {
		return nil, err
	}
	keyBytes, err := json.Marshal([]string{operation.Principal.Operator, operation.OperationID})
	if err != nil {
		return nil, err
	}
	result, exists := list.Operations[string(keyBytes)]
	if !exists {
		for operationKey := range list.Operations {
			var identity []string
			if json.Unmarshal([]byte(operationKey), &identity) == nil && len(identity) == 2 && identity[1] == operation.OperationID {
				return nil, errors.New("permission-denied: operation belongs to another principal")
			}
		}
		return nil, nil //nolint:nilnil // A missing replay is a normal result, matching the store contract.
	}
	if result.RequestDigest != DigestText(request) {
		return nil, errors.New("failed-precondition: operation id reused with different request")
	}
	return &result.Result, nil
}

// CommitImportedSeatListV1Locked replaces a v1 list during its one-time import.
func CommitImportedSeatListV1Locked(listPath string, convert func(string) (SeatListV2, error), operation Operation, filesystem StoreFileSystem) (SeatListV2, error) {
	journal, err := readOptional(filesystem, JournalPath(listPath))
	if err != nil {
		return SeatListV2{}, err
	}
	if journal != nil {
		return SeatListV2{}, errors.New("seat journal already exists; refusing a second v1 import")
	}
	legacy, err := readOptional(filesystem, listPath)
	if err != nil {
		return SeatListV2{}, err
	}
	if legacy == nil {
		return SeatListV2{}, fmt.Errorf("seat list %s is missing", listPath)
	}
	list, err := convert(string(legacy))
	if err != nil {
		return SeatListV2{}, err
	}
	backupPath := listPath + ".v1"
	if err := filesystem.CreateExclusive(backupPath, legacy, 0o600); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return SeatListV2{}, fmt.Errorf("create v1 backup %s: %w", backupPath, err)
		}
		backup, readErr := filesystem.ReadFile(backupPath)
		if readErr != nil {
			return SeatListV2{}, fmt.Errorf("read existing v1 backup %s: %w", backupPath, readErr)
		}
		if !bytes.Equal(backup, legacy) {
			return SeatListV2{}, fmt.Errorf("existing v1 backup %s does not match seat list; the seat list is unchanged, compare the files and remove the backup to retry", backupPath)
		}
		// The first attempt may have died before its Sync; make the reused copy durable.
		if err := filesystem.FsyncPath(backupPath); err != nil {
			return SeatListV2{}, fmt.Errorf("fsync v1 backup: %w", err)
		}
	}
	if err := filesystem.FsyncDir(filepath.Dir(backupPath)); err != nil {
		return SeatListV2{}, fmt.Errorf("fsync v1 backup directory: %w", err)
	}
	return CommitLocked(listPath, SeatListV2{Version: 2, GenerationBase: list.GenerationBase, Seats: []SeatEntryV2{}}, list.Seats, operation, filesystem)
}

// DefaultOperation creates an operator operation identity.
func DefaultOperation(operationID, operator string) Operation {
	if operator == "" {
		operator = "operator"
	}
	return Operation{OperationID: operationID, Principal: Principal{Kind: "operator", Operator: operator}}
}
