package client

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/inventory"
	"github.com/oklog/ulid/v2"
)

func (c *Client) escrowPassword(ctx context.Context) error {
	if c.escrowed {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	digest, err := c.escrow.Put(ctx, c.repo.Scope, append([]byte(nil), c.password...))
	if err != nil {
		return fmt.Errorf("client: escrow password: %w", err)
	}
	if err := c.appendEvent(journalEvent{Event: eventEscrowed, At: time.Now().UTC(), Digest: hex.EncodeToString(digest[:])}); err != nil {
		return err
	}
	c.escrowed = true
	return nil
}

func (c *Client) privateList(name string, values []string) (string, error) {
	path := filepath.Join(c.stateDir, name+"-"+ulid.Make().String()+".txt")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // List name is generated inside the state directory.
	if err != nil {
		return "", fmt.Errorf("client: create private list: %w", withoutPath(err))
	}
	for _, value := range values {
		if value == "" || strings.ContainsAny(value, "\r\n") {
			closeErr := file.Close()
			removeErr := os.Remove(path) //nolint:gosec // path is built inside the client's own state directory.
			return "", errors.Join(errors.New("client: invalid path in private list"), withoutPath(closeErr), withoutPath(removeErr))
		}
		if _, err := file.WriteString(value + "\n"); err != nil {
			closeErr := file.Close()
			removeErr := os.Remove(path) //nolint:gosec // path is built inside the client's own state directory.
			return "", errors.Join(fmt.Errorf("client: write private list: %w", withoutPath(err)), withoutPath(closeErr), withoutPath(removeErr))
		}
	}
	if err := file.Sync(); err != nil {
		closeErr := file.Close()
		removeErr := os.Remove(path) //nolint:gosec // path is built inside the client's own state directory.
		return "", errors.Join(fmt.Errorf("client: sync private list: %w", withoutPath(err)), withoutPath(closeErr), withoutPath(removeErr))
	}
	if err := file.Close(); err != nil {
		removeErr := os.Remove(path) //nolint:gosec // List name is generated inside the state directory.
		return "", errors.Join(fmt.Errorf("client: close private list: %w", withoutPath(err)), withoutPath(removeErr))
	}
	return path, nil
}

func removePrivateFile(path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) { //nolint:gosec // Removes the generated private list.
		return
	}
}

func statLine(path string) (inventory.Line, error) {
	info, err := os.Lstat(path) //nolint:gosec // Path is a source selected for the pass.
	if err != nil {
		return inventory.Line{}, fmt.Errorf("client: stat source file: %w", withoutPath(err))
	}
	return inventory.Line{Path: path, Type: "file", Size: info.Size()}, nil
}

func appendUnique(values []string, value string) []string {
	if slices.Contains(values, value) {
		return values
	}
	return append(values, value)
}

// pendingIn returns the listed paths that are pending; pass-acked clears only these.
func (c *Client) pendingIn(list []string) []string {
	pending := make([]string, 0, len(c.pending))
	for _, path := range list {
		if _, ok := c.pending[pathKey(path)]; ok {
			pending = append(pending, path)
		}
	}
	return pending
}

// ackPath moves one pending path to acked. Open drops a torn final line; lost path-acked lines leave paths pending for the next pass.
func (c *Client) ackPath(path string) {
	key := pathKey(path)
	if pending, ok := c.pending[key]; ok {
		c.acked[key] = pending.line
		delete(c.pending, key)
	}
}

func withoutPath(err error) error {
	if pathErr, ok := errors.AsType[*os.PathError](err); ok {
		return pathErr.Err
	}
	return err
}
