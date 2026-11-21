package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/samber/oops"
)

type Pin struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	File   string `json:"file"`
}

func installPins(ctx context.Context, get func(ctx context.Context, url string) (io.ReadCloser, error), pins map[string]Pin, binDir string) error {
	if len(pins) == 0 {
		return nil
	}
	names := make([]string, 0, len(pins))
	for name := range pins {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := installPin(ctx, get, name, pins[name], binDir); err != nil {
			return err
		}
	}
	return nil
}

func installPin(ctx context.Context, get func(context.Context, string) (io.ReadCloser, error), name string, pin Pin, binDir string) error {
	if filepath.Base(pin.File) != pin.File || pin.File == "." || pin.File == ".." {
		return oops.In("windows-agent-sync").Errorf("invalid filename for pin %s", name)
	}
	if len(pin.SHA256) != sha256.Size*2 {
		return oops.In("windows-agent-sync").Errorf("invalid SHA-256 for pin %s", name)
	}
	if _, err := hex.DecodeString(pin.SHA256); err != nil {
		return oops.In("windows-agent-sync").Wrapf(err, "invalid SHA-256 for pin %s", name)
	}
	target := filepath.Join(binDir, pin.File)
	if matches, err := fileMatchesSHA256(target, pin.SHA256); err != nil {
		return oops.In("windows-agent-sync").Wrapf(err, "checking pin %s", name)
	} else if matches {
		return nil
	}
	if err := os.MkdirAll(binDir, 0o750); err != nil {
		return oops.In("windows-agent-sync").Wrapf(err, "creating binary directory")
	}
	response, err := get(ctx, pin.URL)
	if err != nil {
		return oops.In("windows-agent-sync").Wrapf(err, "downloading pin %s", name)
	}
	file, err := os.CreateTemp(binDir, ".pin-*.tmp")
	if err != nil {
		return oops.In("windows-agent-sync").Wrapf(errors.Join(err, response.Close()), "staging pin %s", name)
	}
	temp := file.Name()
	_, copyErr := io.Copy(file, response)
	responseErr := response.Close()
	if copyErr == nil {
		copyErr = responseErr
	} else {
		copyErr = errors.Join(copyErr, responseErr)
	}
	if copyErr == nil {
		copyErr = file.Sync()
	}
	closeErr := file.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		removeErr := os.Remove(temp)
		return oops.In("windows-agent-sync").Wrapf(errors.Join(err, removeErr), "staging pin %s", name)
	}
	matches, err := fileMatchesSHA256(temp, pin.SHA256)
	if err != nil {
		removeErr := os.Remove(temp)
		return oops.In("windows-agent-sync").Wrapf(errors.Join(err, removeErr), "verifying pin %s", name)
	}
	if !matches {
		removeErr := os.Remove(temp)
		return oops.In("windows-agent-sync").Wrapf(errors.Join(errors.New("SHA-256 mismatch"), removeErr), "verifying pin %s", name)
	}
	if err := replaceBinary(temp, target); err != nil {
		return oops.In("windows-agent-sync").Wrapf(errors.Join(err, removeMissing(temp)), "installing pin %s", name)
	}
	return nil
}

// replaceBinary moves an existing target to <target>.old, replacing any prior .old,
// so a running Windows .exe can be swapped; then it renames temp into place.
func replaceBinary(temp, target string) error {
	return replaceBinaryWithRename(temp, target, os.Rename)
}

func replaceBinaryWithRename(temp, target string, rename func(string, string) error) error {
	old := target + ".old"
	if err := removeMissing(old); err != nil {
		return fmt.Errorf("removing previous binary backup %s: %w", filepath.Base(old), err)
	}
	if err := rename(target, old); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("moving current binary to %s: %w", filepath.Base(old), err)
		}
		return rename(temp, target)
	}
	if err := rename(temp, target); err != nil {
		return errors.Join(err, rename(old, target))
	}
	return nil
}

func removeMissing(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func fileMatchesSHA256(path, expected string) (bool, error) {
	file, err := os.Open(path) //nolint:gosec // path is the per-user pin target or a temp file created by this process.
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return false, errors.Join(err, file.Close())
	}
	if err := file.Close(); err != nil {
		return false, err
	}
	return strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), expected), nil
}

// builtPins are the cargo and bun tool versions pinned in pins.json, so a
// reviewed diff is the only way they move.
type builtPins struct {
	JJHooks   string `json:"jj-hooks"`
	LinearCLI string `json:"linear-cli"`
}

// readPins returns the binary download pins and the built-tool versions from
// pins.json. The "built" key is versions, not downloads, so it is split off
// before the binary pins are decoded.
func readPins(path string) (map[string]Pin, builtPins, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is the fixed pins.json the layout names under --repo-root.
	if err != nil {
		return nil, builtPins{}, fmt.Errorf("reading pins file: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, builtPins{}, fmt.Errorf("decoding pins file: %w", err)
	}
	rawBuilt, ok := raw["built"]
	if !ok {
		return nil, builtPins{}, errors.New("pins file has no built key; jj-hooks and linear-cli must be pinned")
	}
	var built builtPins
	if err := json.Unmarshal(rawBuilt, &built); err != nil {
		return nil, builtPins{}, fmt.Errorf("decoding built pins: %w", err)
	}
	if built.JJHooks == "" || built.LinearCLI == "" {
		return nil, builtPins{}, errors.New("built pins must set jj-hooks and linear-cli versions")
	}
	delete(raw, "built")
	pins := make(map[string]Pin, len(raw))
	for name, value := range raw {
		var pin Pin
		if err := json.Unmarshal(value, &pin); err != nil {
			return nil, builtPins{}, fmt.Errorf("decoding pin %s: %w", name, err)
		}
		pins[name] = pin
	}
	return pins, built, nil
}
