package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/RigelBuild/nebula/tools/windows-agent-sync/internal/envstore"
	"github.com/samber/oops"
)

type secretRef struct {
	Var          string `json:"var"`
	Key          string `json:"key"`
	PersonalOnly bool   `json:"personalOnly"`
}

type SecretMap struct {
	Team             []secretRef       `json:"team"`
	LiteLLM          liteLLMMap        `json:"litellm"`
	Linear           secretRef         `json:"linear"`
	Search           []secretRef       `json:"search"`
	FileExports      []secretRef       `json:"fileExports"`
	Literals         map[string]string `json:"literals"`
	ExportedVarNames []string          `json:"exportedVarNames"`
}

type liteLLMMap struct {
	BaseURL secretRef `json:"baseUrl"`
	APIKey  secretRef `json:"apiKey"`
}

func toWSLPath(win string) (string, error) {
	pathName := strings.ReplaceAll(win, `\`, "/")
	if len(pathName) < 3 || pathName[1] != ':' || pathName[2] != '/' || !isASCIILetter(pathName[0]) {
		return "", oops.In("windows-agent-sync").Errorf("path must be an absolute drive path")
	}
	return path.Join("/mnt", strings.ToLower(pathName[:1]), pathName[3:]), nil
}

func isASCIILetter(value byte) bool {
	return value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
}

func loadMap(ctx context.Context, runner Runner, distro, mapPath string) (SecretMap, error) {
	output, err := runner(ctx, "wsl.exe", "-d", distro, "--exec", "/run/current-system/sw/bin/nix", "eval", "--json", "--file", mapPath)
	if err != nil {
		return SecretMap{}, oops.In("windows-agent-sync").Wrapf(err, "evaluating secret map")
	}
	var result SecretMap
	if err := json.Unmarshal(output, &result); err != nil {
		return SecretMap{}, oops.In("windows-agent-sync").Wrapf(err, "decoding secret map")
	}
	return result, nil
}

func resolve(ctx context.Context, runner Runner, distro string, secretMap SecretMap) (map[string]string, map[string]string, error) {
	vars := make(map[string]string)
	files := make(map[string]string)
	refs := make([]secretRef, 0, len(secretMap.Team)+len(secretMap.Search)+len(secretMap.FileExports)+1)
	refs = append(refs, secretMap.Team...)
	refs = append(refs, secretMap.Linear)
	refs = append(refs, secretMap.Search...)
	refs = append(refs, secretMap.FileExports...)
	for _, ref := range refs {
		value, err := readSecret(ctx, runner, distro, ref)
		if err != nil {
			return nil, nil, err
		}
		if err := addResolved(vars, files, secretMap.FileExports, ref.Var, value); err != nil {
			return nil, nil, err
		}
	}
	baseURL, baseErr := readSecret(ctx, runner, distro, secretMap.LiteLLM.BaseURL)
	apiKey, keyErr := readSecret(ctx, runner, distro, secretMap.LiteLLM.APIKey)
	if baseErr != nil || keyErr != nil {
		return nil, nil, errors.Join(baseErr, keyErr)
	}
	if err := addResolved(vars, files, secretMap.FileExports, secretMap.LiteLLM.BaseURL.Var, baseURL); err != nil {
		return nil, nil, err
	}
	if err := addResolved(vars, files, secretMap.FileExports, secretMap.LiteLLM.APIKey.Var, apiKey); err != nil {
		return nil, nil, err
	}
	if _, exists := vars["LITELLM_MCP_URL"]; exists {
		return nil, nil, oops.In("windows-agent-sync").Errorf("secret map repeats variable LITELLM_MCP_URL")
	}
	vars["LITELLM_MCP_URL"] = mcpURL(baseURL)
	for name, value := range secretMap.Literals {
		if err := addResolved(vars, files, secretMap.FileExports, name, value); err != nil {
			return nil, nil, err
		}
	}
	actual := make([]string, 0, len(vars)+len(files))
	for name := range vars {
		actual = append(actual, name)
	}
	for name := range files {
		actual = append(actual, name)
	}
	sort.Strings(actual)
	expected := append([]string(nil), secretMap.ExportedVarNames...)
	sort.Strings(expected)
	if !sameStrings(actual, expected) {
		return nil, nil, oops.In("windows-agent-sync").Errorf("secret map exportedVarNames do not match resolved variables")
	}
	return vars, files, nil
}

var errMissingSecret = errors.New("secret is missing")

// secretsDir is the NixOS-WSL directory that holds one file per secret key.
const secretsDir = "/run/secrets"

func readSecret(ctx context.Context, runner Runner, distro string, ref secretRef) (string, error) {
	if ref.Key == "" || ref.Var == "" {
		return "", oops.In("windows-agent-sync").Errorf("secret map contains an empty key or variable name")
	}
	output, err := runner(ctx, "wsl.exe", "-d", distro, "--exec", "/run/current-system/sw/bin/cat", path.Join(secretsDir, ref.Key))
	if err != nil {
		return "", oops.In("windows-agent-sync").Wrapf(errors.Join(errMissingSecret, err), "reading key %s", ref.Key)
	}
	value := strings.TrimRight(string(output), "\r\n")
	if strings.TrimSpace(value) == "" {
		return "", oops.In("windows-agent-sync").Wrapf(errMissingSecret, "key %s is missing or blank", ref.Key)
	}
	return value, nil
}

func addResolved(vars, files map[string]string, fileExports []secretRef, name, value string) error {
	if name == "" {
		return oops.In("windows-agent-sync").Errorf("secret map contains an empty variable name")
	}
	if _, exists := vars[name]; exists {
		return oops.In("windows-agent-sync").Errorf("secret map repeats variable %s", name)
	}
	if _, exists := files[name]; exists {
		return oops.In("windows-agent-sync").Errorf("secret map repeats variable %s", name)
	}
	if isFileExport(fileExports, name) {
		files[name] = value
	} else {
		vars[name] = value
	}
	return nil
}

func isFileExport(exports []secretRef, name string) bool {
	for _, ref := range exports {
		if ref.Var == name {
			return true
		}
	}
	return false
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func mcpURL(base string) string {
	base = strings.TrimSuffix(strings.TrimSuffix(base, "/"), "/v1")
	return base + "/mcp/"
}

func applySecrets(store EnvStore, vars, files map[string]string, fileDir string, managed []string) ([]string, error) {
	fileNames := sortedKeys(files)
	staged, err := stageExports(files, fileNames, fileDir)
	if err != nil {
		return nil, err
	}
	newSet := make(map[string]struct{}, len(vars)+len(files))
	for name := range vars {
		newSet[name] = struct{}{}
	}
	for name := range files {
		newSet[name] = struct{}{}
	}
	retired := make([]string, 0)
	for _, name := range managed {
		if _, found := newSet[name]; !found {
			retired = append(retired, name)
		}
	}
	sort.Strings(retired)
	if err := retireSecrets(store, retired, fileDir, staged); err != nil {
		return nil, err
	}
	if err := commitStagedExports(staged, fileDir); err != nil {
		return nil, err
	}
	if err := setSecretVariables(store, vars, fileNames, fileDir, staged); err != nil {
		return nil, err
	}
	if err := store.Broadcast(); err != nil {
		return nil, oops.In("windows-agent-sync").Wrapf(err, "broadcasting environment change")
	}
	return sortedKeys(newSet), nil
}

func stageExports(files map[string]string, fileNames []string, fileDir string) (map[string]string, error) {
	staged := make(map[string]string, len(files))
	for _, name := range fileNames {
		if err := stageExport(staged, fileDir, name, files[name]); err != nil {
			return nil, err
		}
	}
	return staged, nil
}

func stageExport(staged map[string]string, fileDir, name, value string) error {
	if err := os.MkdirAll(fileDir, 0o700); err != nil {
		cleanupErr := removeStaged(staged)
		return oops.In("windows-agent-sync").Wrapf(errors.Join(err, cleanupErr), "creating export directory")
	}
	target := filepath.Join(fileDir, exportFileName(name))
	current, readErr := os.ReadFile(target) //nolint:gosec // target is a file export under %LOCALAPPDATA%/rigel/secrets at runtime.
	if readErr == nil && bytes.Equal(current, []byte(value)) {
		return nil
	}
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		cleanupErr := removeStaged(staged)
		return oops.In("windows-agent-sync").Wrapf(errors.Join(readErr, cleanupErr), "checking export %s", name)
	}
	file, err := os.CreateTemp(fileDir, ".windows-agent-sync-*.tmp")
	if err != nil {
		cleanupErr := removeStaged(staged)
		return oops.In("windows-agent-sync").Wrapf(errors.Join(err, cleanupErr), "staging export %s", name)
	}
	temp := file.Name()
	writeErr := file.Chmod(0o600)
	if writeErr == nil {
		_, writeErr = file.WriteString(value)
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		removeErr := os.Remove(temp)
		cleanupErr := removeStaged(staged)
		return oops.In("windows-agent-sync").Wrapf(errors.Join(writeErr, closeErr, removeErr, cleanupErr), "staging export %s", name)
	}
	staged[name] = temp
	return nil
}

func retireSecrets(store EnvStore, retired []string, fileDir string, staged map[string]string) error {
	for _, name := range retired {
		if err := retireSecret(store, name, fileDir, staged); err != nil {
			return err
		}
	}
	return nil
}

func retireSecret(store EnvStore, name, fileDir string, staged map[string]string) error {
	if err := store.Delete(name); err != nil {
		cleanupErr := removeStaged(staged)
		return oops.In("windows-agent-sync").Wrapf(errors.Join(err, cleanupErr), "deleting retired variable %s", name)
	}
	if isFileExportName(name) {
		if err := os.Remove(filepath.Join(fileDir, exportFileName(name))); err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanupErr := removeStaged(staged)
			return oops.In("windows-agent-sync").Wrapf(errors.Join(err, cleanupErr), "deleting retired export %s", name)
		}
	}
	return nil
}

var (
	errExportReplacementFailed   = errors.New("replacing staged export failed")
	errStagedExportCleanupFailed = errors.New("removing staged export failed")
)

// sanitizeExportReplacementError keeps only the errno class from a failed replace.
// The replace never carries a secret value; this is defense in depth, so a future
// caller that wraps a value-bearing error still cannot leak it into the log.
func sanitizeExportReplacementError(err error) error {
	causes := []error{errExportReplacementFailed}
	for _, safeCause := range []error{os.ErrExist, os.ErrNotExist, os.ErrPermission} {
		if errors.Is(err, safeCause) {
			causes = append(causes, safeCause)
		}
	}
	return errors.Join(causes...)
}

func sanitizeStagedExportCleanupError(err error) error {
	if err == nil {
		return nil
	}
	return errStagedExportCleanupFailed
}

func commitStagedExports(staged map[string]string, fileDir string) error {
	return commitStagedExportsWithReplace(staged, fileDir, envstore.ReplaceFile)
}

func commitStagedExportsWithReplace(staged map[string]string, fileDir string, replace func(src, dst string) error) error {
	for _, name := range sortedKeys(staged) {
		target := filepath.Join(fileDir, exportFileName(name))
		if err := replace(staged[name], target); err != nil {
			cleanupErr := removeStaged(staged)
			return oops.In("windows-agent-sync").Wrapf(errors.Join(sanitizeExportReplacementError(err), sanitizeStagedExportCleanupError(cleanupErr)), "committing export %s", name)
		}
		delete(staged, name)
	}
	return nil
}

func setSecretVariables(store EnvStore, vars map[string]string, fileNames []string, fileDir string, staged map[string]string) error {
	for _, name := range sortedKeys(vars) {
		if err := setIfChanged(store, name, vars[name]); err != nil {
			cleanupErr := removeStaged(staged)
			return oops.In("windows-agent-sync").Wrapf(errors.Join(err, cleanupErr), "setting variable %s", name)
		}
	}
	for _, name := range fileNames {
		if err := setIfChanged(store, name, filepath.Join(fileDir, exportFileName(name))); err != nil {
			cleanupErr := removeStaged(staged)
			return oops.In("windows-agent-sync").Wrapf(errors.Join(err, cleanupErr), "setting variable %s", name)
		}
	}
	return nil
}

func setIfChanged(store EnvStore, name, value string) error {
	current, err := store.Get(name)
	if err != nil {
		return oops.In("windows-agent-sync").Wrapf(err, "reading variable %s", name)
	}
	if current == value {
		return nil
	}
	return store.Set(name, value)
}

// fileExportSuffix marks a variable whose value is stored in a file. The file
// name is the variable name without the suffix, lowercased, with hyphens.
const fileExportSuffix = "_FILE"

func exportFileName(name string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSuffix(name, fileExportSuffix), "_", "-"))
}

func isFileExportName(name string) bool {
	return strings.HasSuffix(name, fileExportSuffix)
}

func removeStaged(staged map[string]string) error {
	var removeErr error
	for name, temp := range staged {
		if err := os.Remove(temp); err != nil && !errors.Is(err, os.ErrNotExist) {
			removeErr = errors.Join(removeErr, err)
			continue
		}
		delete(staged, name)
	}
	return removeErr
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
