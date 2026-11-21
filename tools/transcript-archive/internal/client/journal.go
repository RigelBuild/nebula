package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/inventory"
)

const (
	eventPending        = "pending"
	eventPendingCleared = "pending-cleared"
	eventPassStart      = "pass-start"
	eventPassAcked      = "pass-acked"
	eventPassIncomplete = "pass-incomplete"
	eventPassFailed     = "pass-failed"
	eventPassStored     = "pass-stored"
	eventPassAbandoned  = "pass-abandoned"
	eventVanished       = "vanished"
	eventEscrowed       = "escrowed"
	eventInit           = "init"
	// eventPathAcked follows pass-acked, one per pending path, so no journal line grows with the pass.
	eventPathAcked = "path-acked"
)

type journalEvent struct {
	Event         string          `json:"event"`
	At            time.Time       `json:"at,omitzero"`
	ScanStartedAt time.Time       `json:"scanStartedAt,omitzero"`
	PassID        string          `json:"passId,omitempty"`
	SnapshotID    string          `json:"snapshotId,omitempty"`
	Path          string          `json:"path,omitempty"`
	Paths         []string        `json:"paths,omitempty"`
	Code          int             `json:"exitCode,omitempty"`
	Digest        string          `json:"sha256,omitempty"`
	Line          *inventory.Line `json:"line,omitempty"`
}

func (c *Client) openJournal() error {
	flags := os.O_CREATE | os.O_RDWR
	if c.readOnly {
		flags = os.O_RDONLY
	}
	file, err := os.OpenFile(filepath.Join(c.stateDir, "journal.jsonl"), flags, 0o600) //nolint:gosec // G703: stateDir is the operator-configured state directory passed to Open, not request input.
	if errors.Is(err, os.ErrNotExist) && c.readOnly {
		return nil
	}
	if err != nil {
		return fmt.Errorf("client: open journal: %w", withoutPath(err))
	}
	if !c.readOnly {
		if err := file.Chmod(0o600); err != nil {
			closeErr := file.Close()
			return errors.Join(fmt.Errorf("client: protect journal: %w", withoutPath(err)), withoutPath(closeErr))
		}
	}
	reader := bufio.NewReader(file)
	var offset int64
	for {
		line, readErr := reader.ReadBytes('\n')
		// Every event ends in a newline, so a final line without one is a torn append: drop it (read-only: skip, never truncate).
		if errors.Is(readErr, io.EOF) && len(line) > 0 {
			if c.readOnly {
				break
			}
			if err := file.Truncate(offset); err != nil {
				return errors.Join(fmt.Errorf("client: truncate torn journal tail: %w", withoutPath(err)), withoutPath(file.Close()))
			}
			if err := file.Sync(); err != nil {
				return errors.Join(fmt.Errorf("client: sync journal: %w", withoutPath(err)), withoutPath(file.Close()))
			}
			break
		}
		offset += int64(len(line))
		if len(line) > 0 {
			var event journalEvent
			if err := json.Unmarshal(bytes.TrimSpace(line), &event); err != nil {
				closeErr := file.Close()
				return errors.Join(fmt.Errorf("client: decode journal event: %w", withoutPath(err)), withoutPath(closeErr))
			}
			c.observePassID(event.PassID)
			if event.PassID != "" && (event.Event == eventPending || event.Event == eventPendingCleared || event.Event == eventVanished) && !c.committed[event.PassID] {
				continue
			}
			c.applyEvent(event)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			closeErr := file.Close()
			return errors.Join(fmt.Errorf("client: read journal: %w", withoutPath(readErr)), withoutPath(closeErr))
		}
	}
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		closeErr := file.Close()
		return errors.Join(fmt.Errorf("client: seek journal: %w", withoutPath(err)), withoutPath(closeErr))
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("client: close journal: %w", withoutPath(err))
	}
	return nil
}

func (c *Client) appendEvent(event journalEvent) error {
	return c.appendEvents([]journalEvent{event})
}

func (c *Client) appendEvents(events []journalEvent) error {
	if err := c.writeEvents(events); err != nil {
		return err
	}
	for _, event := range events {
		c.applyEvent(event)
	}
	return nil
}

func (c *Client) writeEvents(events []journalEvent) error {
	if c.readOnly {
		return ErrReadOnly
	}
	if len(events) == 0 {
		return nil
	}
	var data bytes.Buffer
	enc := json.NewEncoder(&data)
	for _, event := range events {
		if err := enc.Encode(event); err != nil {
			return fmt.Errorf("client: encode journal event: %w", withoutPath(err))
		}
	}
	file, err := os.OpenFile(filepath.Join(c.stateDir, "journal.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // Journal resides in the state directory.
	if err != nil {
		return fmt.Errorf("client: append journal: %w", withoutPath(err))
	}
	if _, err := file.Write(data.Bytes()); err != nil {
		closeErr := file.Close()
		return errors.Join(fmt.Errorf("client: write journal: %w", withoutPath(err)), withoutPath(closeErr))
	}
	if err := file.Sync(); err != nil {
		closeErr := file.Close()
		return errors.Join(fmt.Errorf("client: sync journal: %w", withoutPath(err)), withoutPath(closeErr))
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("client: close journal: %w", withoutPath(err))
	}
	return nil
}

func (c *Client) applyEvent(event journalEvent) {
	switch event.Event {
	case eventPending:
		if event.Line != nil {
			c.pending[pathKey(event.Line.Path)] = pendingEntry{line: *event.Line, at: event.At}
		}
	case eventPendingCleared:
		delete(c.pending, pathKey(event.Path))
	case eventPassStart:
		c.passes[event.PassID] = event
	case eventPassAcked:
		c.outcomes[event.PassID] = true
		start := c.passes[event.PassID]
		start.Event = event.Event
		start.SnapshotID = event.SnapshotID
		c.passes[event.PassID] = start
		for _, path := range start.Paths {
			c.ackPath(path)
		}
		start.Paths = nil
		c.passes[event.PassID] = start
	case eventPathAcked:
		// Only a journaled pass-acked makes its path lines count.
		if c.passes[event.PassID].Event == eventPassAcked {
			c.ackPath(event.Path)
		}
	case eventPassIncomplete, eventPassFailed, eventPassStored, eventPassAbandoned:
		c.outcomes[event.PassID] = true
		start := c.passes[event.PassID]
		start.Event = event.Event
		start.SnapshotID = event.SnapshotID
		c.passes[event.PassID] = start
	case eventInit:
		c.initialized = true
	case eventEscrowed:
		c.escrowed = true
	case eventVanished:
		delete(c.pending, pathKey(event.Path))
	}
}

// Scan persists the current inventory and journals pending state transitions.
func (c *Client) Scan(ctx context.Context, now inventory.Inventory, delta inventory.Delta) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.readOnly {
		return ErrReadOnly
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	started := c.now().UTC()
	events := make([]journalEvent, 0, len(delta.Removed)+len(delta.Added)+len(delta.Changed))
	for _, line := range delta.Removed {
		if _, ok := c.pending[pathKey(line.Path)]; ok {
			events = append(events, journalEvent{Event: eventVanished, Path: line.Path, At: started})
		}
	}
	scanned := make(map[string]inventory.Line, len(now.Lines))
	for _, line := range now.Lines {
		scanned[pathKey(line.Path)] = line
		if event, ok := c.scanTransition(line, started); ok {
			events = append(events, event)
		}
	}
	if err := c.persistScan(now, delta, events, scanned, started); err != nil {
		return err
	}
	return nil
}

func (c *Client) scanTransition(line inventory.Line, started time.Time) (journalEvent, bool) {
	if line.Class == "final-only" || strings.HasSuffix(line.Path, ".db") || strings.HasSuffix(line.Path, ".db-wal") || strings.HasSuffix(line.Path, ".db-shm") {
		return journalEvent{}, false
	}
	key := pathKey(line.Path)
	pending, exists := c.pending[key]
	old, acked := c.acked[key]
	covered := c.chained && c.baseline != nil && baselineHas(c.baseline.Lines, line)
	if covered || (acked && sameLineVersion(old, line)) {
		return journalEvent{Event: eventPendingCleared, Path: line.Path, At: started}, exists
	}
	if exists && sameLineVersion(pending.line, line) {
		return journalEvent{}, false
	}
	return journalEvent{Event: eventPending, Line: &line, At: started}, true
}

func baselineHas(lines []inventory.Line, line inventory.Line) bool {
	for _, old := range lines {
		if old.Path == line.Path && old.Type == line.Type && old.Size == line.Size && old.SHA256 == line.SHA256 {
			return true
		}
	}
	return false
}

func sameLineVersion(left, right inventory.Line) bool {
	return left.Path == right.Path && left.Type == right.Type && left.Size == right.Size && left.SHA256 == right.SHA256
}

func pathKey(path string) string { return path }

func syncDir(path string) error {
	directory, err := os.Open(path) //nolint:gosec // Opens the state or delta directory for fsync.
	if err != nil {
		return fmt.Errorf("client: open state directory: %w", withoutPath(err))
	}
	if err := directory.Sync(); err != nil {
		closeErr := directory.Close()
		return errors.Join(fmt.Errorf("client: sync state directory: %w", withoutPath(err)), withoutPath(closeErr))
	}
	if err := directory.Close(); err != nil {
		return fmt.Errorf("client: close state directory: %w", withoutPath(err))
	}
	return nil
}
