package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

type importOptions struct {
	Name           string
	Fork           string
	Ref            string
	Upstream       string
	UpstreamBranch string
	Base           string
}

type upstreamRecord struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	Commit string `json:"commit"`
}

var safeName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// readUpstreamRecord loads a fork's upstream.json and checks it. Its repo is passed to git,
// so it is validated here, before any fetch.
func readUpstreamRecord(path string) (upstreamRecord, error) {
	var record upstreamRecord
	data, err := os.ReadFile(path)
	if err != nil {
		return record, fmt.Errorf("read upstream record: %w", err)
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return record, fmt.Errorf("decode upstream record: %w", err)
	}
	if record.Repo == "" || record.Branch == "" || record.Commit == "" {
		return record, errors.New("upstream record must contain repo, branch and commit")
	}
	return record, validateRepo(record.Repo)
}

func importFork(ctx context.Context, root string, options importOptions) (resultErr error) {
	if err := validateName(options.Name); err != nil {
		return err
	}
	if options.Fork == "" || options.Upstream == "" || options.UpstreamBranch == "" {
		return errors.New("--fork, --upstream and --upstream-branch are required")
	}
	if err := validateRepo(options.Upstream); err != nil {
		return err
	}
	if options.Ref == "" {
		options.Ref = "rigel-release"
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("create nebula worktree root: %w", err)
	}
	if _, err := git(ctx, root, nil, "rev-parse", "--show-toplevel"); err != nil {
		return fmt.Errorf("import requires a git worktree: %w", err)
	}
	if err := ensureClean(ctx, root); err != nil {
		return fmt.Errorf("import requires a clean worktree: %w", err)
	}
	if _, err := os.Stat(filepath.Join(root, "forks", options.Name)); err == nil {
		return fmt.Errorf("fork %q is already imported", options.Name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check fork destination: %w", err)
	}
	temporary, err := os.MkdirTemp("", "nebula-fork-import-")
	if err != nil {
		return fmt.Errorf("create temporary clone: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, os.RemoveAll(temporary)) }()
	forkURL := forkRepoURL(options.Fork)
	if err := validateRepo(forkURL); err != nil {
		return fmt.Errorf("--fork: %w", err)
	}
	if _, err := git(ctx, temporary, nil, "init", "--bare", "-q"); err != nil {
		return fmt.Errorf("initialize temporary repository: %w", err)
	}
	if _, err := git(ctx, temporary, nil, "fetch", "--quiet", "--", forkURL, options.Ref); err != nil {
		return fmt.Errorf("fetch fork ref %s: %w", options.Ref, err)
	}
	forkBytes, err := git(ctx, temporary, nil, "rev-parse", "FETCH_HEAD^{commit}")
	if err != nil {
		return fmt.Errorf("resolve fork ref: %w", err)
	}
	forkCommit := strings.TrimSpace(string(forkBytes))
	if _, err := git(ctx, temporary, nil, "fetch", "--quiet", "--", options.Upstream, "refs/heads/"+options.UpstreamBranch); err != nil {
		return fmt.Errorf("fetch upstream branch: %w", err)
	}
	upstreamBytes, err := git(ctx, temporary, nil, "rev-parse", "FETCH_HEAD^{commit}")
	if err != nil {
		return fmt.Errorf("resolve upstream branch: %w", err)
	}
	upstreamTip := strings.TrimSpace(string(upstreamBytes))
	base := options.Base
	if base == "" {
		baseBytes, err := git(ctx, temporary, nil, "merge-base", forkCommit, upstreamTip)
		if err != nil {
			if gitExitCode(err) == 1 {
				return errors.New("fork and upstream have no merge-base; pass --base <sha> to select the upstream base explicitly")
			}
			return fmt.Errorf("compute upstream merge-base: %w", err)
		}
		base = strings.TrimSpace(string(baseBytes))
	}
	if _, err := git(ctx, temporary, nil, "fetch", "--quiet", "--", options.Upstream, base); err != nil {
		return fmt.Errorf("fetch upstream base %q: %w", base, err)
	}
	if _, err := git(ctx, temporary, nil, "cat-file", "-e", base+"^{commit}"); err != nil {
		return fmt.Errorf("upstream base %q is not available in fetched history: %w", base, err)
	}
	if _, err := git(ctx, temporary, nil, "merge-base", "--is-ancestor", base, upstreamTip); err != nil {
		return fmt.Errorf("upstream base %q is not an ancestor of branch tip %s: %w", base, short(upstreamTip), err)
	}
	if err := os.MkdirAll(filepath.Join(root, "forks"), 0o755); err != nil {
		return fmt.Errorf("create forks directory: %w", err)
	}
	destination := filepath.Join(root, "forks", options.Name)
	if err := os.Mkdir(destination, 0o755); err != nil {
		return fmt.Errorf("create fork tree: %w", err)
	}
	index, err := writeForkTree(ctx, root, temporary, forkCommit, destination, "forks/"+options.Name+"/")
	if err != nil {
		return fmt.Errorf("write fork tree: %w", err)
	}
	record := upstreamRecord{Repo: options.Upstream, Branch: options.UpstreamBranch, Commit: base}
	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("encode upstream record: %w", err)
	}
	moon := "# Generated by fork-sync import; fill in the fork toolchain tasks before enabling CI.\nproject:\n  name: \"" + options.Name + "\"\n\n# The upstream tree is forks/" + options.Name + "/.\n"
	generated := map[string][]byte{
		"upstream.json": append(encoded, '\n'),
		"moon.yml":      []byte(moon),
	}
	for path, content := range generated {
		if err := os.WriteFile(filepath.Join(destination, path), content, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		records, err := stageRecords(ctx, root, "forks/"+options.Name+"/"+path, "100644", content)
		if err != nil {
			return fmt.Errorf("stage %s: %w", path, err)
		}
		index = append(index, records...)
	}
	if _, err := git(ctx, root, index, "update-index", "-z", "--index-info"); err != nil {
		return fmt.Errorf("stage fork import: %w", err)
	}
	message := fmt.Sprintf("feat(forks): import %s at %s (upstream %s)", options.Name, short(forkCommit), short(base))
	commit := exec.CommandContext(ctx, "git", "commit", "-m", message) //nolint:gosec // Fixed command with generated message.
	commit.Dir = root
	commit.Env = commitIdentityEnv()
	if output, err := commit.CombinedOutput(); err != nil {
		return fmt.Errorf("commit fork import: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// stageRecords stores content in nebula's object store with no filters and returns its NUL-terminated index record.
func stageRecords(ctx context.Context, root, path, mode string, content []byte) ([]byte, error) {
	object, err := git(ctx, root, content, "hash-object", "-w", "--no-filters", "--stdin")
	if err != nil {
		return nil, fmt.Errorf("store %s: %w", path, err)
	}
	return []byte(fmt.Sprintf("%s %s\t%s\x00", mode, strings.TrimSpace(string(object)), path)), nil
}

// writeForkTree writes the fork tree into destination and returns the index records that stage it.
// Each blob is read in one cat-file --batch stream, written to the worktree, and stored in nebula's object store with --no-filters.
func writeForkTree(ctx context.Context, nebula, repository, commit, destination, prefix string) ([]byte, error) {
	listing, err := git(ctx, repository, nil, "ls-tree", "-r", "-z", commit)
	if err != nil {
		return nil, fmt.Errorf("list fork tree: %w", err)
	}
	entries, err := parseTreeListing(listing)
	if err != nil {
		return nil, err
	}
	var blobIDs []string
	for _, entry := range entries {
		if entry.mode != "160000" {
			blobIDs = append(blobIDs, entry.object)
		}
	}
	contents, err := readBlobs(ctx, repository, blobIDs)
	if err != nil {
		return nil, err
	}
	tree, err := os.OpenRoot(destination)
	if err != nil {
		return nil, fmt.Errorf("open fork tree: %w", err)
	}
	defer tree.Close()
	var index bytes.Buffer
	for _, entry := range entries {
		if err := tree.MkdirAll(filepath.Dir(entry.path), 0o755); err != nil {
			return nil, fmt.Errorf("create directory for %s: %w", entry.path, err)
		}
		switch entry.mode {
		case "160000":
			// A gitlink has no blob in this store. Stage it as a gitlink and create its directory empty.
			if err := tree.MkdirAll(entry.path, 0o755); err != nil {
				return nil, fmt.Errorf("create submodule directory %s: %w", entry.path, err)
			}
		case "120000":
			if err := tree.Symlink(string(contents[entry.object]), entry.path); err != nil {
				return nil, fmt.Errorf("write symlink %s: %w", entry.path, err)
			}
		default:
			if err := writeFile(tree, entry.path, entry.mode, contents[entry.object]); err != nil {
				return nil, err
			}
		}
		fmt.Fprintf(&index, "%s %s\t%s%s\x00", entry.mode, entry.object, prefix, entry.path)
	}
	if err := storeBlobs(ctx, nebula, destination, entries, contents); err != nil {
		return nil, err
	}
	return index.Bytes(), nil
}

// storeBlobs stores each non-gitlink entry with --no-filters and checks its ID against the fork's.
// A symlink is hashed from its target bytes, since hash-object on its path would follow the link.
func storeBlobs(ctx context.Context, nebula, destination string, entries []treeEntry, contents map[string][]byte) error {
	var files []treeEntry
	var paths bytes.Buffer
	for _, entry := range entries {
		switch entry.mode {
		case "160000":
		case "120000":
			if err := storeLinkTarget(ctx, nebula, contents[entry.object], entry); err != nil {
				return err
			}
		default:
			files = append(files, entry)
			paths.WriteString(filepath.Join(destination, entry.path) + "\n")
		}
	}
	if len(files) == 0 {
		return nil
	}
	output, err := git(ctx, nebula, paths.Bytes(), "hash-object", "-w", "--no-filters", "--stdin-paths")
	if err != nil {
		return fmt.Errorf("store fork blobs: %w", err)
	}
	ids := strings.Fields(string(output))
	if len(ids) != len(files) {
		return fmt.Errorf("store fork blobs: got %d IDs for %d files", len(ids), len(files))
	}
	for i, entry := range files {
		if ids[i] != entry.object {
			return fmt.Errorf("store %s: blob %s differs from fork blob %s", entry.path, ids[i], entry.object)
		}
	}
	return nil
}

// storeLinkTarget stores one symlink's target text and checks its ID against the fork's.
func storeLinkTarget(ctx context.Context, nebula string, target []byte, entry treeEntry) error {
	id, err := git(ctx, nebula, target, "hash-object", "-w", "--no-filters", "--stdin")
	if err != nil {
		return fmt.Errorf("store link %s: %w", entry.path, err)
	}
	if got := strings.TrimSpace(string(id)); got != entry.object {
		return fmt.Errorf("store link %s: blob %s differs from fork blob %s", entry.path, got, entry.object)
	}
	return nil
}

// treeEntry is one ls-tree -r -z record.
type treeEntry struct {
	mode, object, path string
}

func parseTreeListing(listing []byte) ([]treeEntry, error) {
	var entries []treeEntry
	for _, record := range strings.Split(string(listing), "\x00") {
		if record == "" {
			continue
		}
		meta, path, found := strings.Cut(record, "\t")
		fields := strings.Fields(meta)
		if !found || len(fields) != 3 {
			return nil, fmt.Errorf("parse fork tree entry %q", record)
		}
		entries = append(entries, treeEntry{mode: fields[0], object: fields[2], path: path})
	}
	return entries, nil
}

// readBlobs returns each blob's bytes, keyed by object ID, from one cat-file --batch stream.
func readBlobs(ctx context.Context, repository string, ids []string) (map[string][]byte, error) {
	var request bytes.Buffer
	for _, id := range ids {
		request.WriteString(id + "\n")
	}
	output, err := git(ctx, repository, request.Bytes(), "cat-file", "--batch")
	if err != nil {
		return nil, fmt.Errorf("read fork blobs: %w", err)
	}
	contents := make(map[string][]byte, len(ids))
	rest := output
	for _, id := range ids {
		header, after, found := bytes.Cut(rest, []byte("\n"))
		if !found {
			return nil, fmt.Errorf("read fork blob %s: missing header", id)
		}
		var gotID, kind string
		var size int
		if _, err := fmt.Sscanf(string(header), "%s %s %d", &gotID, &kind, &size); err != nil || gotID != id || kind != "blob" {
			return nil, fmt.Errorf("read fork blob %s: unexpected header %q", id, header)
		}
		if size > len(after) {
			return nil, fmt.Errorf("read fork blob %s: truncated", id)
		}
		contents[id] = after[:size]
		rest = bytes.TrimPrefix(after[size:], []byte("\n"))
	}
	return contents, nil
}

// writeFile creates path under tree with content and the executable bit from mode.
func writeFile(tree *os.Root, path, mode string, content []byte) error {
	perm := os.FileMode(0o644)
	if mode == "100755" {
		perm = 0o755
	}
	file, err := tree.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if _, err := file.Write(content); err != nil {
		return errors.Join(fmt.Errorf("write %s: %w", path, err), file.Close())
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}

// forkRepoURL maps the RigelBuild/<repo> shorthand to its GitHub https URL. Any other value is returned unchanged for validateRepo to judge.
func forkRepoURL(fork string) string {
	if strings.HasPrefix(fork, "RigelBuild/") {
		return "https://github.com/" + fork + ".git"
	}
	return fork
}

func validateName(name string) error {
	if !safeName.MatchString(name) || strings.Contains(name, "..") {
		return fmt.Errorf("invalid fork name %q", name)
	}
	return nil
}

func short(value string) string {
	if len(value) < 8 {
		return value
	}
	return value[:8]
}
