package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"time"
)

// JournalSnapshot identifies a snapshot and its durable pass outcome.
type JournalSnapshot struct {
	SnapshotID string
	Outcome    OutcomeKind
}

// Snapshots returns all journaled snapshots usable as history.
func (c *Client) Snapshots() []JournalSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	snapshots := make([]JournalSnapshot, 0, len(c.passes))
	for _, pass := range c.passes {
		if pass.SnapshotID == "" {
			continue
		}
		var outcome OutcomeKind
		switch pass.Event {
		case eventPassAcked:
			outcome = Acked
		case eventPassIncomplete:
			outcome = Incomplete
		case eventPassStored:
			outcome = Stored
		default:
			continue
		}
		snapshots = append(snapshots, JournalSnapshot{SnapshotID: pass.SnapshotID, Outcome: outcome})
	}
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].SnapshotID < snapshots[j].SnapshotID })
	return snapshots
}

// Recover resolves passes that started without a recorded outcome.
func (c *Client) Recover(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.readOnly {
		return ErrReadOnly
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.ensureRepository(ctx); err != nil {
		return err
	}
	passIDs := make([]string, 0, len(c.passes))
	for passID, event := range c.passes {
		if event.Event == eventPassStart && !c.outcomes[passID] {
			passIDs = append(passIDs, passID)
		}
	}
	sort.Strings(passIDs)
	for _, passID := range passIDs {
		if err := ctx.Err(); err != nil {
			return err
		}
		args := c.commonArgs("snapshots", false)
		args = append(args, "--tag", "pass-"+passID)
		output, err := c.runJSON(ctx, args...)
		if err != nil {
			return fmt.Errorf("client: recover pass: %w", err)
		}
		snapshotID, err := taggedSnapshot(output, passID)
		if err != nil {
			return err
		}
		kind, event := Stored, eventPassStored
		if snapshotID == "" {
			kind, event = Abandoned, eventPassAbandoned
		}
		if err := c.appendEvent(journalEvent{Event: event, PassID: passID, SnapshotID: snapshotID, At: time.Now().UTC()}); err != nil {
			return err
		}
		if kind == Stored {
			continue
		}
	}
	return nil
}

func taggedSnapshot(output []byte, passID string) (string, error) {
	var snapshots []struct {
		ID   string   `json:"id"`
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(output), &snapshots); err != nil {
		return "", fmt.Errorf("client: parse snapshot listing: %w", err)
	}
	for _, snapshot := range snapshots {
		if slices.Contains(snapshot.Tags, "pass-"+passID) {
			return snapshot.ID, nil
		}
	}
	return "", nil
}

// Pending reports the oldest pending-since time and the pending byte total.
func (c *Client) Pending() (oldest time.Time, bytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, pending := range c.pending {
		if oldest.IsZero() || pending.at.Before(oldest) {
			oldest = pending.at
		}
		bytes += pending.line.Size
	}
	return oldest, bytes
}
