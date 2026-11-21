//go:build linux || darwin

package transfer

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec" //nolint:depguard // Runs the verifier as the selected runtime user.
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

type CommandRunner func(ctx context.Context, name string, args ...string) (string, error)

func Verify(ctx context.Context, runtimeUser, root, manifestPath string) error {
	if root == "" || manifestPath == "" {
		return errors.New("verify requires --root and --manifest")
	}
	manifest, err := readManifest(manifestPath)
	if err != nil {
		return err
	}
	if err := validateManifestRoots(manifest); err != nil {
		return err
	}
	if err := validateManifestPaths(manifest); err != nil {
		return err
	}
	if err := compareManifest(root, manifest); err != nil {
		return err
	}
	uid, gid, err := runtimeCredential(runtimeUser)
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve verifier executable: %w", err)
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	command := exec.CommandContext(ctx, executable, "verify-as-user", root) //nolint:gosec // Re-executes this binary with a fixed subcommand.
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: gid}}
	command.Env = []string{"HOME=/opt/data", "PATH=" + os.Getenv("PATH")}
	command.Stdin = bytes.NewReader(data)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("verify as %s: %w: %s", runtimeUser, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func readManifest(path string) (Manifest, error) {
	file, err := os.Open(path) //nolint:gosec // The operator selects the manifest path.
	if err != nil {
		return Manifest{}, fmt.Errorf("open source manifest: %w", err)
	}
	data, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return Manifest{}, fmt.Errorf("read source manifest: %w", err)
	}
	manifest, err := decodeManifest(data)
	if err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func compareManifest(root string, want Manifest) error {
	got, err := scanTree(root, want.Roots)
	if err != nil {
		return err
	}
	got.Entries = slices.DeleteFunc(got.Entries, func(entry ManifestEntry) bool {
		return entry.Path == pageRoot+"/.initialized" || entry.Path == rewriteTempRoot || strings.HasPrefix(entry.Path, rewriteTempRoot+"/")
	})
	if len(got.Entries) != len(want.Entries) {
		return fmt.Errorf("manifest has %d entries, target has %d", len(want.Entries), len(got.Entries))
	}
	for i, expected := range want.Entries {
		if got.Entries[i] != expected {
			return fmt.Errorf("manifest mismatch at %s: got %+v, want %+v", got.Entries[i].Path, got.Entries[i], expected)
		}
		path := filepath.Join(root, filepath.FromSlash(expected.Path))
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("stat owner for %s: %w", expected.Path, err)
		}
		stat, ok := info.Sys().(*fileStat)
		uid, gid := uint32(10000), uint32(10000)
		if expected.Path == pageRoot {
			uid, gid = 0, 0
		}
		if !ok || stat.Uid != uid || stat.Gid != gid {
			return fmt.Errorf("owner mismatch at %s: want %d:%d", expected.Path, uid, gid)
		}
		mode := uint32(info.Mode().Perm())
		if expected.Path == pageRoot && mode != 0o755 {
			return fmt.Errorf("mode mismatch at %s: want 0755", expected.Path)
		}
		if strings.HasPrefix(expected.Path, "opt/data/.local/state/nemo-google-mcp/") && mode&0o077 != 0 {
			return fmt.Errorf("google credentials at %s are accessible by group or other", expected.Path)
		}
	}
	return nil
}

func runtimeCredential(value string) (uint32, uint32, error) {
	if uid, err := strconv.ParseUint(value, 10, 32); err == nil {
		return uint32(uid), uint32(uid), nil
	}
	account, err := user.Lookup(value)
	if err != nil {
		return 0, 0, fmt.Errorf("resolve runtime user %q: %w", value, err)
	}
	uid, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("parse runtime uid: %w", err)
	}
	gid, err := strconv.ParseUint(account.Gid, 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("parse runtime gid: %w", err)
	}
	return uint32(uid), uint32(gid), nil
}

// VerifyAsUser runs in the credential-switched child.
func VerifyAsUser(ctx context.Context, root string, input io.Reader) error {
	data, err := io.ReadAll(input)
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	manifest, err := decodeManifest(data)
	if err != nil {
		return err
	}
	if err := validateManifestRoots(manifest); err != nil {
		return err
	}
	if err := validateManifestPaths(manifest); err != nil {
		return err
	}
	return verifyAsRuntimeUser(ctx, root, manifest, localCommand)
}

func verifyAsRuntimeUser(ctx context.Context, root string, manifest Manifest, run CommandRunner) error {
	for _, entry := range manifest.Entries {
		if entry.Path == rewriteTempRoot {
			continue
		}
		path := filepath.Join(root, filepath.FromSlash(entry.Path))
		switch entry.Type {
		case fileType:
			file, err := os.Open(path) //nolint:gosec // The manifest paths are validated.
			if err != nil {
				return fmt.Errorf("open file %s: %w", entry.Path, err)
			}
			_, readErr := io.Copy(io.Discard, file)
			if err := errors.Join(readErr, file.Close()); err != nil {
				return fmt.Errorf("read file %s: %w", entry.Path, err)
			}
			if entry.Mode&0o111 != 0 {
				if err := syscall.Access(path, 1); err != nil {
					return fmt.Errorf("executable check at %s: %w", entry.Path, err)
				}
			}
		case directoryType:
			if _, err := os.ReadDir(path); err != nil {
				return fmt.Errorf("list directory %s: %w", entry.Path, err)
			}
		case symlinkType:
			if err := verifyLink(root, path, manifest.Roots); err != nil {
				return fmt.Errorf("resolve symlink %s: %w", entry.Path, err)
			}
		}
	}
	if !hasRoot(manifest, dataRoot) {
		return nil
	}
	if err := verifyGoogleTokens(root); err != nil {
		return err
	}
	return verifyJujutsuRepositories(ctx, filepath.Join(root, "opt/data/nemo/repos"), run)
}

func verifyGoogleTokens(root string) error {
	for _, account := range []string{"workspace", "personal"} {
		path := filepath.Join(root, "opt/data/.local/state/nemo-google-mcp", account, "credentials")
		files, err := os.ReadDir(path)
		if err != nil {
			return fmt.Errorf("list %s Google credentials: %w", account, err)
		}
		found := false
		for _, file := range files {
			if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") {
				continue
			}
			found = true
			credentialPath := filepath.Join(path, file.Name())
			data, err := os.ReadFile(credentialPath) //nolint:gosec // Directory is the validated credential tree.
			if err != nil {
				return fmt.Errorf("read %s Google token: %w", account, err)
			}
			var value map[string]json.RawMessage
			if err := json.Unmarshal(data, &value); err != nil {
				return fmt.Errorf("parse %s Google token %s: %w", account, file.Name(), err)
			}
		}
		if !found {
			return fmt.Errorf("missing %s Google token", account)
		}
	}
	return nil
}

func hasRoot(manifest Manifest, root string) bool {
	return slices.Contains(manifest.Roots, root)
}

func localCommand(ctx context.Context, name string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, name, args...) //nolint:gosec // Command paths are fixed by the verifier.
	var output bytes.Buffer
	command.Stdout = &output
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return output.String(), nil
}

func verifyJujutsuRepositories(ctx context.Context, root string, run CommandRunner) error {
	if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("stat Nemo repositories: %w", err)
	}
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("walk Nemo repositories: %w", walkErr)
		}
		if !entry.IsDir() {
			return nil
		}
		if _, err := os.Stat(filepath.Join(path, ".jj")); err == nil {
			if _, err := run(ctx, "jj", "status", "--repository", path); err != nil {
				return fmt.Errorf("jj status in %s: %w", path, err)
			}
			return filepath.SkipDir
		}
		return nil
	})
}

func ReadArchiveManifest(reader io.Reader) (Manifest, error) {
	archive := tar.NewReader(reader)
	header, err := archive.Next()
	if err != nil {
		return Manifest{}, fmt.Errorf("read archive manifest header: %w", err)
	}
	if header.Name != manifestName || header.Typeflag != tar.TypeReg || header.Size < 0 {
		return Manifest{}, errors.New("archive does not begin with a valid manifest")
	}
	data, err := io.ReadAll(io.LimitReader(archive, header.Size+1))
	if err != nil {
		return Manifest{}, fmt.Errorf("read archive manifest: %w", err)
	}
	if int64(len(data)) != header.Size {
		return Manifest{}, errors.New("archive manifest size does not match its header")
	}
	manifest, err := decodeManifest(data)
	if err != nil {
		return Manifest{}, err
	}
	if err := validateManifestRoots(manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, validateManifestPaths(manifest)
}

func decodeManifest(data []byte) (Manifest, error) {
	var manifest Manifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Manifest{}, errors.New("manifest has trailing JSON values")
	}
	for _, entry := range manifest.Entries {
		if err := validateManifestEntry(entry); err != nil {
			return Manifest{}, err
		}
	}
	return manifest, nil
}

func validateManifestEntry(entry ManifestEntry) error {
	if err := validateRelativePath(entry.Path); err != nil {
		return err
	}
	switch entry.Type {
	case fileType, directoryType, symlinkType:
		return nil
	default:
		return fmt.Errorf("manifest has unsupported type %q at %s", entry.Type, entry.Path)
	}
}
