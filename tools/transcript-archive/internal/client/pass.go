package client

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec" //nolint:depguard // restic runs directly, without a shell
	"path/filepath"
	"strings"
	"time"

	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/inventory"
	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/resticenv"
	"github.com/oklog/ulid/v2"
)

type resticMessage struct {
	MessageType     string `json:"message_type"`
	ID              string `json:"id"`
	FilesUnmodified int    `json:"files_unmodified"`
}

type exitError interface {
	ExitCode() int
}

// Pass backs up the supplied paths and the current scan delta. Each scan permits one pass.
func (c *Client) Pass(ctx context.Context, files []string, tags []string) (Outcome, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.readOnly {
		return Outcome{}, ErrReadOnly
	}
	return c.pass(ctx, files, tags)
}

// PassFinal backs up the complete lifecycle delta and final-only files.
func (c *Client) PassFinal(ctx context.Context, tags []string) (outcome Outcome, resultErr error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.readOnly {
		return Outcome{}, ErrReadOnly
	}
	if c.scanID == "" {
		return Outcome{}, ErrNoScan
	}
	if _, exists := c.passes[c.scanID]; exists {
		return Outcome{}, ErrPassDone
	}
	file, err := os.Open(filepath.Join(c.stateDir, "scan-"+c.scanID+".jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return Outcome{}, ErrNoScan
	}
	if err != nil {
		return Outcome{}, fmt.Errorf("client: open current scan: %w", withoutPath(err))
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("client: close current scan: %w", withoutPath(closeErr)))
		}
	}()
	files := []string{}
	decoder := json.NewDecoder(file)
	for {
		var line inventory.Line
		if err := decoder.Decode(&line); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return Outcome{}, fmt.Errorf("client: decode current scan: %w", err)
		}
		if !c.chained || c.baseline == nil || !baselineHas(c.baseline.Lines, line) || line.Class == "final-only" {
			files = append(files, line.Path)
		}
	}
	return c.pass(ctx, files, tags)
}

func (c *Client) pass(ctx context.Context, files []string, tags []string) (Outcome, error) {
	if err := ctx.Err(); err != nil {
		return Outcome{}, err
	}
	if c.newestDelta == "" {
		return Outcome{}, ErrNoScan
	}
	if _, exists := c.passes[c.scanID]; exists {
		return Outcome{}, ErrPassDone
	}
	if err := c.ensureRepository(ctx); err != nil {
		return Outcome{}, err
	}
	started := c.now().UTC()
	list, vanished, err := c.filterPresent(files, started)
	if err != nil {
		return Outcome{}, err
	}
	for _, path := range list {
		if _, exists := c.scanned[pathKey(path)]; !exists {
			line, err := statLine(path)
			if err != nil {
				return Outcome{}, err
			}
			if err := c.appendEvent(journalEvent{Event: eventPending, Line: &line, At: started}); err != nil {
				return Outcome{}, err
			}
		}
	}
	list = appendUnique(list, c.newestDelta)
	listPath, err := c.privateList("pass-files", list)
	if err != nil {
		return Outcome{}, err
	}
	defer removePrivateFile(listPath)
	passID := c.scanID
	// Pending paths stay in memory; the ack journals them one per line, so no line grows with the walk set.
	start := journalEvent{Event: eventPassStart, PassID: passID, ScanStartedAt: c.scanStarted, At: started}
	if err := c.writeEvents([]journalEvent{start}); err != nil {
		return Outcome{}, err
	}
	start.Paths = c.pendingIn(list)
	c.applyEvent(start)
	return c.runPass(ctx, list, listPath, tags, passID, started, vanished)
}

func (c *Client) filterPresent(files []string, started time.Time) ([]string, []string, error) {
	present := make([]string, 0, len(files))
	seen := make(map[string]struct{}, len(files))
	vanished := make([]string, 0)
	for _, path := range files {
		info, err := os.Lstat(path) //nolint:gosec // Checks a source selected for the pass.
		if errors.Is(err, os.ErrNotExist) {
			vanished = append(vanished, path)
			if err := c.appendEvent(journalEvent{Event: eventVanished, Path: path, At: started}); err != nil {
				return nil, nil, err
			}
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("client: lstat pending file: %w", withoutPath(err))
		}
		if info.IsDir() {
			return nil, nil, errors.New("client: pass file list contains a directory")
		}
		if _, dup := seen[path]; !dup {
			seen[path] = struct{}{}
			present = append(present, path)
		}
	}
	return present, vanished, nil
}

func (c *Client) runPass(ctx context.Context, files []string, listPath string, tags []string, passID string, started time.Time, vanished []string) (Outcome, error) {
	args := c.commonArgs("backup", true)
	args = append(args, "--host", c.repo.Scope, "--tag", "pass-"+passID, "--files-from-verbatim", filepath.Base(listPath))
	if parent := c.latestAcked(); parent != "" {
		args = append(args, "--parent", parent)
	}
	for _, tag := range tags {
		args = append(args, "--tag", tag)
	}
	output, err := c.runJSON(ctx, args...)
	if err == nil {
		outcome, ackErr := c.ackPass(passID, started, snapshotID(output), vanished)
		outcome.FilesUnmodified = filesUnmodified(output)
		return outcome, ackErr
	}
	return c.failPass(err, output, files, passID, started, vanished)
}

func (c *Client) ackPass(passID string, started time.Time, id string, vanished []string) (Outcome, error) {
	if id == "" {
		return Outcome{}, errors.New("client: successful backup returned no snapshot id")
	}
	paths := c.passes[passID].Paths
	events := make([]journalEvent, 0, 1+len(paths))
	events = append(events, journalEvent{Event: eventPassAcked, PassID: passID, SnapshotID: id, At: time.Now().UTC()})
	for _, path := range paths {
		events = append(events, journalEvent{Event: eventPathAcked, PassID: passID, Path: path})
	}
	if err := c.appendEvents(events); err != nil {
		return Outcome{}, err
	}
	return Outcome{Kind: Acked, PassID: passID, SnapshotID: id, ScanStartedAt: started, Vanished: vanished}, nil
}

func filesUnmodified(output []byte) int {
	for line := range bytes.SplitSeq(output, []byte{'\n'}) {
		var message resticMessage
		if json.Unmarshal(line, &message) == nil && message.MessageType == "summary" {
			return message.FilesUnmodified
		}
	}
	return 0
}

func (c *Client) failPass(runErr error, output []byte, files []string, passID string, started time.Time, vanished []string) (Outcome, error) {
	var exit exitError
	if !errors.As(runErr, &exit) {
		return Outcome{}, fmt.Errorf("client: run backup: %w", runErr)
	}
	outcome := Outcome{PassID: passID, ScanStartedAt: started, ExitCode: exit.ExitCode(), Vanished: vanished}
	if outcome.ExitCode == 3 {
		outcome.Kind = Incomplete
		outcome.SnapshotID = snapshotID(output)
		if err := c.appendEvent(journalEvent{Event: eventPassIncomplete, PassID: passID, SnapshotID: outcome.SnapshotID, Code: outcome.ExitCode, At: time.Now().UTC()}); err != nil {
			return Outcome{}, err
		}
		return c.vanishAfterPass(outcome, files)
	}
	outcome.Kind = Failed
	if err := c.appendEvent(journalEvent{Event: eventPassFailed, PassID: passID, Code: outcome.ExitCode, At: time.Now().UTC()}); err != nil {
		return Outcome{}, err
	}
	return outcome, nil
}

func (c *Client) vanishAfterPass(outcome Outcome, files []string) (Outcome, error) {
	for _, path := range files {
		_, err := os.Lstat(path) //nolint:gosec // Rechecks a listed source after an incomplete pass.
		if errors.Is(err, os.ErrNotExist) {
			outcome.Vanished = appendUnique(outcome.Vanished, path)
			if err := c.appendEvent(journalEvent{Event: eventVanished, Path: path, At: time.Now().UTC()}); err != nil {
				return Outcome{}, err
			}
		} else if err != nil {
			return Outcome{}, fmt.Errorf("client: lstat after incomplete backup: %w", withoutPath(err))
		}
	}
	return outcome, nil
}

func (c *Client) ensureRepository(ctx context.Context) error {
	if c.initialized {
		return nil
	}
	if err := c.escrowPassword(ctx); err != nil {
		return err
	}
	_, err := c.runJSON(ctx, append(c.commonArgs("cat", false), "config")...)
	if err == nil {
		return c.journalInit([]byte("{}"))
	}
	var exit exitError
	if !errors.As(err, &exit) {
		return fmt.Errorf("client: inspect repository: %w", err)
	}
	if exit.ExitCode() == 12 {
		return c.brokenInit()
	}
	if exit.ExitCode() != 10 {
		return fmt.Errorf("client: inspect repository: %w", err)
	}
	args := c.commonArgs("init", true)
	output, initErr := c.runJSON(ctx, args...)
	if initErr == nil {
		return c.journalInit(output)
	}
	_, err = c.runJSON(ctx, append(c.commonArgs("cat", false), "config")...)
	if err == nil {
		return c.journalInit([]byte("{}"))
	}
	if !errors.As(err, &exit) {
		return fmt.Errorf("client: inspect repository: %w", err)
	}
	if exit.ExitCode() == 12 {
		return c.brokenInit()
	}
	if exit.ExitCode() != 10 {
		return fmt.Errorf("client: inspect repository: %w", err)
	}
	config, keys, err := localRepositoryState(c.repo.Repository)
	if err != nil {
		return err
	}
	if config || keys {
		return c.brokenInit()
	}
	output, err = c.runJSON(ctx, args...)
	if err != nil {
		return fmt.Errorf("client: retry repository init: %w", err)
	}
	return c.journalInit(output)
}

func (c *Client) brokenInit() error {
	if err := c.appendEvent(journalEvent{Event: "init-broken", At: time.Now().UTC()}); err != nil {
		return err
	}
	return ErrInitBroken
}

// localRepositoryState probes a local repository's files; remote backends report nothing.
func localRepositoryState(repository string) (bool, bool, error) {
	if resticenv.IsBackend(repository) {
		return false, false, nil
	}
	repository = strings.TrimPrefix(repository, "local:")
	_, configErr := os.Stat(filepath.Join(repository, "config")) //nolint:gosec // Inspects the configured local repository.
	if configErr != nil && !errors.Is(configErr, os.ErrNotExist) {
		return false, false, fmt.Errorf("client: inspect repository config state: %w", configErr)
	}
	entries, keysErr := os.ReadDir(filepath.Join(repository, "keys"))
	if keysErr != nil && !errors.Is(keysErr, os.ErrNotExist) {
		return false, false, fmt.Errorf("client: inspect repository key state: %w", keysErr)
	}
	return configErr == nil, len(entries) > 0, nil
}

func (c *Client) journalInit(output []byte) error {
	var message resticMessage
	if len(bytes.TrimSpace(output)) > 0 {
		if err := json.Unmarshal(bytes.TrimSpace(output), &message); err != nil {
			return fmt.Errorf("client: parse init output: %w", err)
		}
	}
	if c.initialized {
		return nil
	}
	if err := c.appendEvent(journalEvent{Event: eventInit, SnapshotID: message.ID, At: time.Now().UTC()}); err != nil {
		return err
	}
	c.initialized = true
	return nil
}
func snapshotID(output []byte) string {
	var id string
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		var message struct {
			MessageType string `json:"message_type"`
			ID          string `json:"id"`
			SnapshotID  string `json:"snapshot_id"`
		}
		if json.Unmarshal(scanner.Bytes(), &message) != nil {
			continue
		}
		if message.SnapshotID != "" {
			id = message.SnapshotID
		}
		if message.MessageType == "snapshot" && message.ID != "" {
			id = message.ID
		}
	}
	return id
}

func (c *Client) commonArgs(command string, write bool) []string {
	args := []string{"--repo", c.repo.Repository, "--password-file", "password", "--json", "--no-cache"}
	if !write {
		args = append(args, "--no-lock")
	}
	return append(args, command)
}

func (c *Client) runJSON(ctx context.Context, args ...string) ([]byte, error) {
	tmpDir := c.scratch
	if err := os.MkdirAll(tmpDir, 0o700); err != nil { //nolint:gosec // Scratch is selected by the operator.
		return nil, fmt.Errorf("client: create restic scratch: %w", withoutPath(err))
	}
	command := exec.CommandContext(ctx, c.restic, args...) //nolint:gosec // command resolves the operator-selected restic binary
	command.Dir = c.stateDir
	command.Env = resticenv.Filter(tmpDir)
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return output.Bytes(), err
	}
	return output.Bytes(), nil
}

func (c *Client) nextPassID(started time.Time) (string, error) {
	ms := ulid.Timestamp(started)
	id := c.lastPass
	if id == (ulid.ULID{}) || ms > id.Time() {
		fresh, err := ulid.New(ms, rand.Reader)
		if err != nil {
			return "", fmt.Errorf("client: generate pass id: %w", err)
		}
		id = fresh
	} else {
		entropy := id.Entropy()
		if err := incrementEntropy(entropy); err != nil {
			return "", fmt.Errorf("client: generate pass id: %w", err)
		}
		if err := id.SetEntropy(entropy); err != nil {
			return "", fmt.Errorf("client: generate pass id: %w", err)
		}
	}
	c.lastPass = id
	return strings.ToLower(id.String()), nil
}

// observePassID raises lastPass to id when id is newer. Call it for every
// pass ID read from disk or the journal, including uncommitted ones, so a
// crashed scan's ID is never minted again. A string that is not a ULID
// cannot equal a minted ID, so it is ignored.
func (c *Client) observePassID(id string) {
	parsed, err := ulid.ParseStrict(id)
	if err != nil {
		return
	}
	if parsed.Compare(c.lastPass) > 0 {
		c.lastPass = parsed
	}
}

// incrementEntropy adds one to the big-endian entropy bytes of a pass ID.
func incrementEntropy(entropy []byte) error {
	for i := len(entropy) - 1; i >= 0; i-- {
		entropy[i]++
		if entropy[i] != 0 {
			return nil
		}
	}
	return ulid.ErrMonotonicOverflow
}

func (c *Client) latestAcked() string {
	var newest time.Time
	id := ""
	for _, event := range c.passes {
		if event.Event == "pass-acked" && event.ScanStartedAt.After(newest) {
			newest = event.ScanStartedAt
			id = event.SnapshotID
		}
	}
	return id
}
