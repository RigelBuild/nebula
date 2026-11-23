package client

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/inventory"
	"github.com/oklog/ulid/v2"
)

var ErrInitBroken = errors.New("client: repository init is broken")

var ErrNoScan = errors.New("client: no scan exists")

// ErrPassDone reports that the current scan has already been passed.
var ErrPassDone = errors.New("client: scan already passed")

// ErrReadOnly reports a write attempted on a client opened with OpenReadOnly.
var ErrReadOnly = errors.New("client: read-only client cannot write")

// RepoRef identifies a transcript scope and the restic repository that stores it.
type RepoRef struct {
	Scope      string
	Repository string
}

// Escrower stores the scope password before repository initialization.
type Escrower interface {
	Put(ctx context.Context, scope string, body []byte) (sha256 [32]byte, err error)
}

// OutcomeKind is the durable result of one pass.
type OutcomeKind uint8

const (
	Acked OutcomeKind = iota
	Incomplete
	Failed
	Stored
	Abandoned
)

// Outcome reports the result of a restic pass or recovery check.
type Outcome struct {
	Kind            OutcomeKind
	PassID          string
	SnapshotID      string
	ScanStartedAt   time.Time
	ExitCode        int
	FilesUnmodified int
	Vanished        []string
}

// Option configures chained lifecycle behavior.
type Option func(*Client)

// WithBaseline marks the predecessor inventory for a chained scope.
func WithBaseline(baseline *inventory.Inventory) Option {
	return func(c *Client) {
		if baseline == nil {
			c.baseline = nil
			return
		}
		baselineCopy := *baseline
		baselineCopy.Lines = append([]inventory.Line(nil), baseline.Lines...)
		c.baseline = &baselineCopy
	}
}

// WithChained tells scans that matching baseline content is already covered.
func WithChained(chained bool) Option {
	return func(c *Client) { c.chained = chained }
}

// WithClock sets the clock used to timestamp scans and passes.
func WithClock(now func() time.Time) Option {
	return func(c *Client) { c.now = now }
}

// WithScratch sets the directory used for restic temporary files.
func WithScratch(dir string) Option {
	return func(c *Client) { c.scratch = dir }
}

// WithRestic sets the restic binary; the default resolves "restic" on PATH.
func WithRestic(path string) Option {
	return func(c *Client) { c.restic = path }
}

type pendingEntry struct {
	line inventory.Line
	at   time.Time
}

// Client owns one scope's durable transcript archive state.
type Client struct {
	mu          sync.Mutex
	stateDir    string
	readOnly    bool
	scratch     string
	restic      string
	repo        RepoRef
	escrow      Escrower
	password    []byte
	baseline    *inventory.Inventory
	chained     bool
	now         func() time.Time
	initialized bool
	newestDelta string
	scanID      string
	scanStarted time.Time
	lastPass    ulid.ULID
	committed   map[string]bool
	scanHook    func(string) error
	escrowed    bool
	pending     map[string]pendingEntry
	scanned     map[string]inventory.Line
	acked       map[string]inventory.Line
	passes      map[string]journalEvent
	outcomes    map[string]bool
}

// newClient builds the in-memory state shared by Open and OpenReadOnly.
func newClient(stateDir string, repo RepoRef, escrow Escrower, options ...Option) *Client {
	c := &Client{
		stateDir:  stateDir,
		repo:      repo,
		escrow:    escrow,
		now:       time.Now,
		restic:    "restic",
		pending:   make(map[string]pendingEntry),
		scanned:   make(map[string]inventory.Line),
		acked:     make(map[string]inventory.Line),
		passes:    make(map[string]journalEvent),
		committed: make(map[string]bool),
		outcomes:  make(map[string]bool),
	}
	for _, option := range options {
		if option != nil {
			option(c)
		}
	}
	return c
}

// OpenReadOnly resumes one scope's state and never creates, chmods or removes anything in stateDir.
// A missing password, journal or scan-state.json reads as empty; write methods return ErrReadOnly.
func OpenReadOnly(stateDir string, repo RepoRef, options ...Option) (*Client, error) {
	if strings.TrimSpace(stateDir) == "" || strings.TrimSpace(repo.Scope) == "" || strings.TrimSpace(repo.Repository) == "" {
		return nil, errors.New("client: state directory, scope and repository are required")
	}
	if _, err := os.Stat(stateDir); err != nil {
		return nil, fmt.Errorf("client: read state directory: %w", withoutPath(err))
	}
	c := newClient(stateDir, repo, nil, options...)
	c.readOnly = true
	if err := c.replayDeltas(); err != nil {
		return nil, err
	}
	if err := c.openJournal(); err != nil {
		return nil, err
	}
	password, err := os.ReadFile(filepath.Join(stateDir, "password")) //nolint:gosec // Password is in the private state directory.
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("client: read password: %w", withoutPath(err))
	}
	if err == nil {
		if err := checkPassword(password); err != nil {
			return nil, err
		}
		c.password = password
	}
	return c, nil
}

// checkPassword requires the 64 lowercase hex characters that Open writes.
func checkPassword(password []byte) error {
	if len(password) != 64 || strings.Trim(string(password), "0123456789abcdef") != "" {
		return errors.New("client: existing password is not 64 lowercase hex characters")
	}
	return nil
}

// Open creates or resumes the state for one scope and writes its password once.
func Open(stateDir string, repo RepoRef, escrow Escrower, options ...Option) (*Client, error) {
	if strings.TrimSpace(stateDir) == "" || strings.TrimSpace(repo.Scope) == "" || strings.TrimSpace(repo.Repository) == "" {
		return nil, errors.New("client: state directory, scope and repository are required")
	}
	if escrow == nil {
		return nil, errors.New("client: escrow is required")
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil { //nolint:gosec // operator supplies the private state directory
		return nil, fmt.Errorf("client: create state directory: %w", withoutPath(err))
	}
	if err := os.Chmod(stateDir, 0o700); err != nil { //nolint:gosec // directory execute permission is required for traversal
		return nil, fmt.Errorf("client: protect state directory: %w", withoutPath(err))
	}
	c := newClient(stateDir, repo, escrow, options...)
	if strings.TrimSpace(c.scratch) == "" {
		return nil, errors.New("client: scratch directory is required")
	}
	if err := os.MkdirAll(c.scratch, 0o700); err != nil {
		return nil, fmt.Errorf("client: create scratch directory: %w", withoutPath(err))
	}
	if err := os.Chmod(c.scratch, 0o700); err != nil { //nolint:gosec // Directory traversal requires execute permission.
		return nil, fmt.Errorf("client: protect scratch directory: %w", withoutPath(err))
	}
	if err := c.replayDeltas(); err != nil {
		return nil, err
	}
	if err := c.openJournal(); err != nil {
		return nil, err
	}
	password, err := c.readOrCreatePassword()
	if err != nil {
		return nil, err
	}
	c.password = password
	return c, nil
}
func (c *Client) readOrCreatePassword() (result []byte, resultErr error) {
	path := filepath.Join(c.stateDir, "password")
	password, err := os.ReadFile(path) //nolint:gosec // Password is in the private state directory.
	if err == nil {
		if err := checkPassword(password); err != nil {
			return nil, err
		}
		return password, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("client: read password: %w", withoutPath(err))
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return nil, fmt.Errorf("client: generate password: %w", err)
	}
	password = []byte(hex.EncodeToString(random))

	file, err := os.CreateTemp(c.stateDir, ".password-*")
	if err != nil {
		return nil, fmt.Errorf("client: create password: %w", withoutPath(err))
	}
	defer func() {
		if err := os.Remove(file.Name()); err != nil && !errors.Is(err, os.ErrNotExist) { //nolint:gosec // G703: file.Name() is the temp file this call just created in the operator-configured stateDir.
			resultErr = errors.Join(resultErr, fmt.Errorf("client: remove temporary password: %w", withoutPath(err)))
		}
	}()
	if _, err := file.Write(password); err != nil {
		closeErr := file.Close()
		return nil, errors.Join(fmt.Errorf("client: write password: %w", withoutPath(err)), withoutPath(closeErr))
	}
	if err := file.Sync(); err != nil {
		closeErr := file.Close()
		return nil, errors.Join(fmt.Errorf("client: sync password: %w", withoutPath(err)), withoutPath(closeErr))
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("client: close password: %w", withoutPath(err))
	}
	if err := os.Link(file.Name(), path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return c.readOrCreatePassword()
		}
		return nil, fmt.Errorf("client: install password: %w", withoutPath(err))
	}
	if err := syncDir(c.stateDir); err != nil {
		return nil, err
	}
	if err := c.appendEvent(journalEvent{Event: "password-created", At: time.Now().UTC()}); err != nil {
		return nil, err
	}
	return password, nil
}
