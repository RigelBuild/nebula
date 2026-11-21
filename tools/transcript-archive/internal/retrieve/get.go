package retrieve

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec" //nolint:depguard // restic runs directly, without a shell
	"path/filepath"
	"regexp"
	"strings"

	"filippo.io/age"
	"filippo.io/age/armor"
	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/resticenv"
)

const resticGet = "restic"

var blobReference = regexp.MustCompile(`blob:sha256:([0-9a-f]{64})`)

type snapshotNode struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Path string `json:"path"`
}

// Options identifies the repository, escrow and record to restore.
type Options struct {
	Repo, Escrow, Identity   string
	Snapshot, Record, Output string
}

// Get restores one record from an escrow-protected restic snapshot.
func Get(ctx context.Context, options Options) error {
	repo, escrowPath, identityPath := options.Repo, options.Escrow, options.Identity
	snapshot, record, output := options.Snapshot, options.Record, options.Output
	if err := validateGetInputs(repo, escrowPath, identityPath, snapshot, record, output); err != nil {
		return err
	}
	password, err := decryptEscrow(escrowPath, identityPath)
	if err != nil {
		return err
	}
	passwordPath, err := privateSecret(password)
	if err != nil {
		return err
	}
	defer removeSecret(passwordPath)
	if err := os.MkdirAll(output, 0o700); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	return restoreRecord(ctx, repo, passwordPath, snapshot, record, output)
}

func validateGetInputs(repo, escrow, identity, snapshot, record, output string) error {
	if repo == "" || escrow == "" || identity == "" || snapshot == "" || record == "" || output == "" {
		return errors.New("repo, escrow, identity, snapshot, record and out are required")
	}
	if strings.ContainsAny(record, "/\\\r\n") || record == "." || record == ".." {
		return errors.New("record must be a path-free stem")
	}
	return nil
}

func decryptEscrow(escrowPath, identityPath string) (body []byte, resultErr error) {
	identityFile, err := os.Open(identityPath) //nolint:gosec // identityPath is supplied by the operator
	if err != nil {
		return nil, fmt.Errorf("open age identity: %w", err)
	}
	identities, err := age.ParseIdentities(identityFile)
	closeErr := identityFile.Close()
	if err != nil {
		return nil, fmt.Errorf("parse age identity: %w", errors.Join(err, closeErr))
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close age identity: %w", closeErr)
	}
	ciphertext, err := os.Open(escrowPath) //nolint:gosec // escrowPath is supplied by the operator
	if err != nil {
		return nil, fmt.Errorf("open age escrow: %w", err)
	}
	defer func() {
		if closeErr := ciphertext.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close age escrow: %w", closeErr))
		}
	}()
	buffer := bufio.NewReader(ciphertext)
	prefix, err := buffer.Peek(5)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read escrow header: %w", err)
	}
	var source io.Reader = buffer
	if bytes.Equal(prefix, []byte("-----")) {
		source = armor.NewReader(buffer)
	}
	plaintext, err := age.Decrypt(source, identities...)
	if err != nil {
		return nil, fmt.Errorf("decrypt age escrow: %w", err)
	}
	body, err = io.ReadAll(io.LimitReader(plaintext, 65))
	if err != nil {
		return nil, fmt.Errorf("read decrypted escrow: %w", err)
	}
	if len(body) == 0 || len(body) > 64 {
		return nil, errors.New("decrypted escrow must contain a password of at most 64 bytes")
	}
	return body, nil
}

func privateSecret(value []byte) (string, error) {
	file, err := os.CreateTemp("", "transcript-archive-password-*")
	if err != nil {
		return "", fmt.Errorf("create private password file: %w", err)
	}
	path := file.Name()
	if err := file.Chmod(0o600); err != nil {
		closeErr := file.Close()
		removeErr := os.Remove(path)
		return "", errors.Join(fmt.Errorf("protect private password file: %w", err), closeErr, removeErr)
	}
	if _, err := file.Write(value); err != nil {
		closeErr := file.Close()
		removeErr := os.Remove(path)
		return "", errors.Join(fmt.Errorf("write private password file: %w", err), closeErr, removeErr)
	}
	if err := file.Sync(); err != nil {
		closeErr := file.Close()
		removeErr := os.Remove(path)
		return "", errors.Join(fmt.Errorf("sync private password file: %w", err), closeErr, removeErr)
	}
	if err := file.Close(); err != nil {
		removeErr := os.Remove(path)
		return "", errors.Join(fmt.Errorf("close private password file: %w", err), removeErr)
	}
	return path, nil
}

func restoreRecord(ctx context.Context, repo, passwordPath, snapshot, record, output string) (resultErr error) {
	listing, err := runRestic(ctx, repo, passwordPath, "ls", "--no-lock", snapshot, "--json")
	if err != nil {
		return fmt.Errorf("list snapshot: %w", err)
	}
	paths, prefix, err := recordPaths(listing, record)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return fmt.Errorf("snapshot has no paths for record %q", record)
	}
	includePath, err := privateList(paths)
	if err != nil {
		return err
	}
	defer removeSecret(includePath)
	target, err := os.MkdirTemp(output, ".transcript-archive-restore-*")
	if err != nil {
		return fmt.Errorf("create restore directory: %w", err)
	}
	defer func() {
		if removeErr := os.RemoveAll(target); removeErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("remove restore directory: %w", removeErr))
		}
	}()
	if _, err := runRestic(ctx, repo, passwordPath, "restore", "--no-lock", snapshot, "--target", target, "--include-file", includePath); err != nil {
		return fmt.Errorf("restore transcript record: %w", err)
	}
	blobs, err := findBlobPaths(target)
	if err != nil {
		return err
	}
	for i := range blobs {
		blobs[i] = prefix + "/" + blobs[i]
	}
	if len(blobs) > 0 {
		blobList, err := privateList(blobs)
		if err != nil {
			return err
		}
		defer removeSecret(blobList)
		if _, err := runRestic(ctx, repo, passwordPath, "restore", "--no-lock", snapshot, "--target", target, "--include-file", blobList); err != nil {
			return fmt.Errorf("restore referenced transcript blobs: %w", err)
		}
	}
	return moveRestored(target, output, prefix, append(paths, blobs...))
}

func runRestic(ctx context.Context, repo, passwordPath string, args ...string) ([]byte, error) {
	global := make([]string, 0, 5+len(args))
	global = append(global, "--repo", repo, "--password-file", passwordPath, "--no-cache")
	global = append(global, args...)
	command := exec.CommandContext(ctx, resticGet, global...) //nolint:gosec // command resolves the fixed restic binary
	command.Env = resticenv.Filter("")
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return output.Bytes(), errors.New("restic command failed")
	}
	return output.Bytes(), nil
}

func recordPaths(listing []byte, record string) ([]string, string, error) {
	paths := make([]string, 0)
	prefix := ""
	scanner := bufio.NewScanner(bytes.NewReader(listing))
	for scanner.Scan() {
		var node snapshotNode
		if err := json.Unmarshal(scanner.Bytes(), &node); err != nil {
			return nil, "", fmt.Errorf("parse snapshot listing: %w", err)
		}
		if node.Type == "dir" || !strings.HasPrefix(node.Path, "/") {
			continue
		}
		index := strings.Index(node.Path, "/sessions/")
		if index >= 0 && recordMatch(node.Path[index:], record) {
			candidate := node.Path[:index]
			if len(paths) > 0 && prefix != candidate {
				return nil, "", errors.New("record matches multiple watch roots")
			}
			prefix = candidate
			paths = append(paths, node.Path)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, "", fmt.Errorf("read snapshot listing: %w", err)
	}
	return paths, prefix, nil
}

func recordMatch(path, record string) bool {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) < 3 || parts[0] != "sessions" {
		return false
	}
	rootName := record + ".jsonl"
	if parts[2] == rootName {
		return true
	}
	return len(parts) > 3 && parts[2] == record
}

func moveRestored(target, output, prefix string, paths []string) error {
	for _, path := range uniqueStrings(paths) {
		relative := strings.TrimPrefix(path, prefix+"/")
		source := filepath.Join(target, strings.TrimPrefix(path, "/"))
		if _, err := os.Lstat(source); err != nil {
			return fmt.Errorf("inspect restored file: %w", err)
		}
		dest := filepath.Join(output, relative)
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return fmt.Errorf("create restored directory: %w", err)
		}
		if err := os.Rename(source, dest); err != nil {
			return fmt.Errorf("move restored file: %w", err)
		}
	}
	return nil
}

func findBlobPaths(root string) ([]string, error) {
	paths := make([]string, 0)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || filepath.Ext(path) != ".jsonl" {
			return nil
		}
		file, err := os.Open(path) //nolint:gosec // path is inside the private restore directory
		if err != nil {
			return fmt.Errorf("open restored transcript: %w", err)
		}
		body, readErr := io.ReadAll(file)
		closeErr := file.Close()
		if readErr != nil || closeErr != nil {
			return errors.Join(fmt.Errorf("read restored transcript: %w", readErr), closeErr)
		}
		for _, match := range blobReference.FindAllSubmatch(body, -1) {
			paths = append(paths, "blobs/"+string(match[1]))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return uniqueStrings(paths), nil
}

func privateList(values []string) (string, error) {
	file, err := os.CreateTemp("", "transcript-archive-paths-*")
	if err != nil {
		return "", fmt.Errorf("create private restore list: %w", err)
	}
	path := file.Name()
	if err := file.Chmod(0o600); err != nil {
		closeErr := file.Close()
		removeErr := os.Remove(path)
		return "", errors.Join(fmt.Errorf("protect private restore list: %w", err), closeErr, removeErr)
	}
	for _, value := range values {
		if value == "" || strings.ContainsAny(value, "\r\n") {
			closeErr := file.Close()
			removeErr := os.Remove(path)
			return "", errors.Join(errors.New("restore path is invalid"), closeErr, removeErr)
		}
		if _, err := file.WriteString(value + "\n"); err != nil {
			closeErr := file.Close()
			removeErr := os.Remove(path)
			return "", errors.Join(fmt.Errorf("write private restore list: %w", err), closeErr, removeErr)
		}
	}
	if err := file.Sync(); err != nil {
		closeErr := file.Close()
		removeErr := os.Remove(path)
		return "", errors.Join(fmt.Errorf("sync private restore list: %w", err), closeErr, removeErr)
	}
	if err := file.Close(); err != nil {
		removeErr := os.Remove(path)
		return "", errors.Join(fmt.Errorf("close private restore list: %w", err), removeErr)
	}
	return path, nil
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	unique := make([]string, 0, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		unique = append(unique, value)
	}
	return unique
}

func removeSecret(path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return
	}
}
