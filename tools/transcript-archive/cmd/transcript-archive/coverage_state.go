package main

import (
	"archive/tar"
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/coverage"
	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/inventory"
)

// verifiedName holds archived digests already proven, so a check restores only new or changed files.
const verifiedName = "verified.jsonl"

// archivedNode is restic's change-detection metadata for one file in a snapshot.
// restic reuses a parent's content only when these match, so equal nodes mean bytes already proven.
type archivedNode struct {
	Size  int64     `json:"size"`
	Mtime time.Time `json:"mtime"`
	Ctime time.Time `json:"ctime"`
	Inode uint64    `json:"inode"`
}

// same compares instants, so a time-zone change between passes still hits the cache.
func (n archivedNode) same(o archivedNode) bool {
	return n.Size == o.Size && n.Inode == o.Inode && n.Mtime.Equal(o.Mtime) && n.Ctime.Equal(o.Ctime)
}

// verifiedEntry records that path, at this local version and archived node, matched the archive in snapshot.
type verifiedEntry struct {
	Path     string       `json:"path"`
	Size     int64        `json:"size"`
	MtimeNs  int64        `json:"mtimeNs"`
	SHA256   [32]byte     `json:"sha256"`
	Node     archivedNode `json:"node"`
	Snapshot string       `json:"snapshot"`
}

// runStateCoverage checks that every live normal file is in the latest acked snapshot with matching bytes.
// Each pass backs up the whole tree, so the latest snapshot alone decides whether a delete is safe.
func runStateCoverage(out io.Writer, opts coverageOptions) error {
	dir, err := filepath.Abs(opts.stateDir)
	if err != nil {
		return fmt.Errorf("resolve state directory: %w", err)
	}
	opts.passwordFile = filepath.Join(dir, "password")
	opts.cacheDir = filepath.Join(dir, "restic-cache")
	final, err := readInventory(filepath.Join(dir, "scan-current.jsonl"))
	if err != nil {
		return fmt.Errorf("read final inventory: %w", err)
	}
	snapshot, err := latestAcked(filepath.Join(dir, "journal.jsonl"))
	if err != nil {
		return err
	}
	wanted := make(map[string]inventory.Line, len(final.Lines))
	for _, line := range final.Lines {
		if line.Type == "file" && line.Class == "normal" {
			wanted[line.Path] = line
		}
	}
	archived, totalBytes, err := listedNodes(opts, snapshot, wanted)
	if err != nil {
		return err
	}
	verified, err := readVerified(filepath.Join(dir, verifiedName))
	if err != nil {
		return err
	}
	report := coverage.Report{Missing: []string{}, Extra: []string{}, Unexplained: []string{}, Truncated: []string{}, Rewritten: []string{}, HashMismatch: []string{}, History: []coverage.HistoryEntry{}}
	next := make([]verifiedEntry, 0, len(wanted))
	var unverified []verifiedEntry
	var unverifiedBytes int64
	for _, path := range sortedKeys(wanted) {
		line := wanted[path]
		node, ok := archived[path]
		if !ok {
			report.Missing = append(report.Missing, path)
			continue
		}
		if node.Size != line.Size {
			report.HashMismatch = append(report.HashMismatch, path)
			continue
		}
		entry := verifiedEntry{Path: path, Size: line.Size, MtimeNs: line.MtimeNs, SHA256: line.SHA256, Node: node, Snapshot: snapshot}
		if old, ok := verified[path]; ok && old.Size == entry.Size && old.MtimeNs == entry.MtimeNs && old.SHA256 == entry.SHA256 && old.Node.same(node) {
			next = append(next, old)
			continue
		}
		unverified = append(unverified, entry)
		unverifiedBytes += entry.Size
	}
	var digests map[string][32]byte
	// A first run is mostly unverified: one tar stream walks the tree and reads each byte once.
	if len(unverified) > 0 && 2*unverifiedBytes >= totalBytes {
		digests, err = streamedDigests(opts, snapshot, unverified)
	} else {
		digests, err = restoredDigests(opts, filepath.Join(dir, "coverage-scratch"), snapshot, unverified)
	}
	if err != nil {
		return err
	}
	for _, entry := range unverified {
		if digests[entry.Path] != entry.SHA256 {
			report.HashMismatch = append(report.HashMismatch, entry.Path)
			continue
		}
		next = append(next, entry)
	}
	for _, item := range final.Errors {
		report.Unexplained = append(report.Unexplained, item.Path)
	}
	if err := writeVerified(filepath.Join(dir, verifiedName), next); err != nil {
		return err
	}
	report.Pass = len(report.Missing)+len(report.HashMismatch)+len(report.Unexplained) == 0
	return writeCoverageReport(out, report)
}

// latestAcked streams the journal (no line-length cap) and returns the newest acked snapshot.
func latestAcked(path string) (snapshot string, err error) {
	file, err := os.Open(path) //nolint:gosec // Journal inside the operator-supplied state directory.
	if err != nil {
		return "", fmt.Errorf("open journal: %w", err)
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	reader := bufio.NewReader(file)
	for {
		line, readErr := reader.ReadBytes('\n')
		// A final line without a newline is a torn append; skip it, never truncate.
		if len(line) > 0 && line[len(line)-1] == '\n' {
			var event struct {
				Event      string `json:"event"`
				SnapshotID string `json:"snapshotId"`
			}
			if err := json.Unmarshal(line, &event); err != nil {
				return "", fmt.Errorf("decode journal: %w", err)
			}
			if event.Event == "pass-acked" {
				snapshot = event.SnapshotID
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return "", fmt.Errorf("read journal: %w", readErr)
		}
	}
	if snapshot == "" {
		return "", errors.New("state journal has no acked pass")
	}
	return snapshot, nil
}

// listedNodes streams `restic ls` for one snapshot, keeps the nodes of wanted files, and sums every file's size.
func listedNodes(opts coverageOptions, snapshot string, wanted map[string]inventory.Line) (nodes map[string]archivedNode, totalBytes int64, err error) {
	cmd, err := resticCommand(opts, "ls", "--json", "--no-lock", snapshot)
	if err != nil {
		return nil, 0, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, 0, fmt.Errorf("restic ls: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, 0, fmt.Errorf("restic ls: %w", err)
	}
	nodes = make(map[string]archivedNode, len(wanted))
	decoder := json.NewDecoder(stdout)
	for {
		var node struct {
			archivedNode
			StructType string `json:"struct_type"`
			Type       string `json:"type"`
			Path       string `json:"path"`
		}
		if err := decoder.Decode(&node); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, 0, errors.Join(fmt.Errorf("decode restic listing: %w", err), cmd.Wait())
		}
		if node.StructType != "node" || node.Type != "file" {
			continue
		}
		totalBytes += node.Size
		if _, ok := wanted[node.Path]; ok {
			nodes[node.Path] = node.archivedNode
		}
	}
	if err := cmd.Wait(); err != nil {
		return nil, 0, fmt.Errorf("restic ls: %w", err)
	}
	return nodes, totalBytes, nil
}

// streamedDigests hashes the unverified files from one `restic dump --archive tar` of the whole snapshot.
// Missing files keep the zero digest, which reports a mismatch.
func streamedDigests(opts coverageOptions, snapshot string, entries []verifiedEntry) (digests map[string][32]byte, err error) {
	want := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		want[entry.Path] = struct{}{}
	}
	cmd, err := resticCommand(opts, "dump", "--no-lock", "--archive", "tar", snapshot, "/")
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("restic dump: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("restic dump: %w", err)
	}
	digests = make(map[string][32]byte, len(entries))
	archive := tar.NewReader(stdout)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, errors.Join(fmt.Errorf("read restic dump: %w", err), killAndWait(cmd))
		}
		path := "/" + strings.TrimPrefix(strings.TrimPrefix(header.Name, "./"), "/")
		if _, ok := want[path]; !ok || header.Typeflag != tar.TypeReg {
			continue
		}
		hash := sha256.New()
		if _, err := io.Copy(hash, archive); err != nil {
			return nil, errors.Join(fmt.Errorf("read restic dump: %w", err), killAndWait(cmd))
		}
		digests[path] = [32]byte(hash.Sum(nil))
	}
	// Drain any trailing padding so restic exits cleanly.
	if _, err := io.Copy(io.Discard, stdout); err != nil {
		return nil, errors.Join(fmt.Errorf("read restic dump: %w", err), killAndWait(cmd))
	}
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("restic dump: %w", err)
	}
	return digests, nil
}

// killAndWait stops a restic child we stopped reading; a live restic blocks on the full pipe, so Wait alone can hang.
func killAndWait(cmd *exec.Cmd) error {
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return errors.Join(fmt.Errorf("kill restic: %w", err), cmd.Wait())
	}
	return cmd.Wait()
}

// Restore batches cap scratch disk use at about restoreBytes (or one file, if larger).
// Include matching costs patterns × snapshot nodes, so batches stay near 5000 patterns (~41 s on 200k nodes).
const (
	restoreFiles = 5000
	restoreBytes = 8 << 30
)

// restoredDigests restores only the unverified files, batch by batch, so restic fetches only their blobs.
func restoredDigests(opts coverageOptions, scratch, snapshot string, entries []verifiedEntry) (map[string][32]byte, error) {
	digests := make(map[string][32]byte, len(entries))
	for start := 0; start < len(entries); {
		end, size := start, int64(0)
		for end < len(entries) && end-start < restoreFiles && (end == start || size+entries[end].Size <= restoreBytes) {
			size += entries[end].Size
			end++
		}
		if err := restoreBatch(opts, scratch, snapshot, entries[start:end], digests); err != nil {
			return nil, err
		}
		start = end
	}
	return digests, nil
}

func restoreBatch(opts coverageOptions, scratch, snapshot string, batch []verifiedEntry, digests map[string][32]byte) (resultErr error) {
	if err := os.RemoveAll(scratch); err != nil {
		return fmt.Errorf("clear coverage scratch: %w", err)
	}
	target := filepath.Join(scratch, "target")
	if err := os.MkdirAll(target, 0o700); err != nil {
		return fmt.Errorf("create coverage scratch: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(scratch); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("remove coverage scratch: %w", err))
		}
	}()
	var list strings.Builder
	var direct []verifiedEntry
	for _, entry := range batch {
		line, ok := includePattern(entry.Path)
		if !ok {
			direct = append(direct, entry)
			continue
		}
		list.WriteString(line + "\n")
	}
	for _, entry := range direct {
		digest, err := dumpDigest(opts, snapshot, entry.Path)
		if err != nil {
			return err
		}
		digests[entry.Path] = digest
	}
	if list.Len() == 0 {
		return nil
	}
	include := filepath.Join(scratch, "include.txt")
	if err := os.WriteFile(include, []byte(list.String()), 0o600); err != nil {
		return fmt.Errorf("write restore list: %w", err)
	}
	cmd, err := resticCommand(opts, "restore", "--no-lock", "--target", target, "--include-file", include, snapshot)
	if err != nil {
		return err
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("restic restore: %w", err)
	}
	for _, entry := range batch {
		if _, done := digests[entry.Path]; done {
			continue
		}
		digest, err := fileDigest(filepath.Join(target, entry.Path))
		if errors.Is(err, os.ErrNotExist) {
			continue // Not restored: the zero digest reports a mismatch.
		}
		if err != nil {
			return err
		}
		digests[entry.Path] = digest
	}
	return nil
}

// includePattern escapes path for a restic --include-file line; false means no escape exists.
func includePattern(path string) (string, bool) {
	if strings.ContainsAny(path, "\r\n") || strings.TrimSpace(path) != path {
		return "", false
	}
	var line strings.Builder
	for _, r := range path {
		switch r {
		case '\\', '*', '?', '[':
			line.WriteByte('\\')
		case '$':
			line.WriteByte('$')
		}
		line.WriteRune(r)
	}
	return line.String(), true
}

// dumpDigest streams one archived file through sha256, for names no include pattern can match.
func dumpDigest(opts coverageOptions, snapshot, path string) (digest [32]byte, err error) {
	cmd, err := resticCommand(opts, "dump", "--no-lock", snapshot, path)
	if err != nil {
		return digest, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return digest, fmt.Errorf("restic dump: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return digest, fmt.Errorf("restic dump: %w", err)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, stdout); err != nil {
		return digest, errors.Join(fmt.Errorf("read restic dump: %w", err), cmd.Wait())
	}
	if err := cmd.Wait(); err != nil {
		return digest, fmt.Errorf("restic dump: %w", err)
	}
	return [32]byte(hash.Sum(nil)), nil
}

func fileDigest(path string) (digest [32]byte, err error) {
	file, err := os.Open(path) //nolint:gosec // Restored file inside the coverage scratch directory.
	if err != nil {
		return digest, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return digest, fmt.Errorf("hash restored file: %w", err)
	}
	return [32]byte(hash.Sum(nil)), nil
}

func readVerified(path string) (entries map[string]verifiedEntry, err error) {
	entries = map[string]verifiedEntry{}
	file, err := os.Open(path) //nolint:gosec // Verified cache inside the state directory.
	if errors.Is(err, os.ErrNotExist) {
		return entries, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open verified digests: %w", err)
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	decoder := json.NewDecoder(file)
	for {
		var entry verifiedEntry
		if err := decoder.Decode(&entry); errors.Is(err, io.EOF) {
			return entries, nil
		} else if err != nil {
			return nil, fmt.Errorf("decode verified digests: %w", err)
		}
		entries[entry.Path] = entry
	}
}

// writeVerified replaces the cache atomically; a crash leaves the previous cache, which only costs re-dumps.
func writeVerified(path string, entries []verifiedEntry) (resultErr error) {
	temp, err := os.CreateTemp(filepath.Dir(path), ".verified-*")
	if err != nil {
		return fmt.Errorf("create verified digests: %w", err)
	}
	defer func() {
		if err := os.Remove(temp.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
			resultErr = errors.Join(resultErr, fmt.Errorf("remove verified temp: %w", err))
		}
	}()
	writer := bufio.NewWriter(temp)
	encoder := json.NewEncoder(writer)
	for _, entry := range entries {
		if err := encoder.Encode(entry); err != nil {
			return errors.Join(fmt.Errorf("encode verified digests: %w", err), temp.Close())
		}
	}
	if err := writer.Flush(); err != nil {
		return errors.Join(fmt.Errorf("write verified digests: %w", err), temp.Close())
	}
	if err := temp.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync verified digests: %w", err), temp.Close())
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close verified digests: %w", err)
	}
	if err := os.Rename(temp.Name(), path); err != nil {
		return fmt.Errorf("install verified digests: %w", err)
	}
	return nil
}

func sortedKeys(lines map[string]inventory.Line) []string {
	keys := make([]string, 0, len(lines))
	for key := range lines {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
