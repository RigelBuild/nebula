package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/samber/oops"
)

func syncBinaries(ctx context.Context, run Runner, store EnvStore, co checkout, home string) error {
	pinsPath := co.path(co.layout.Pins)
	pins, built, err := readPins(pinsPath)
	if err != nil {
		return oops.In("windows-agent-sync").Wrapf(err, "reading Windows binary pins")
	}
	binDir := filepath.Join(home, ".local", "bin")
	if err := installPins(ctx, download, pins, binDir); err != nil {
		return err
	}
	lockPath := co.path(co.layout.NebulaLock)
	nebulaRev, err := readNebulaRevision(lockPath)
	if err != nil {
		return oops.In("windows-agent-sync").Wrapf(err, "reading nebula lock revision")
	}
	if err := installBuilt(ctx, run, nebulaRev, built); err != nil {
		return err
	}
	if err := ensurePath(store, binDir, filepath.Join(home, ".bun", "bin")); err != nil {
		return err
	}
	return nil
}

func download(ctx context.Context, url string) (io.ReadCloser, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating download request: %w", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", url, err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		closeErr := response.Body.Close()
		return nil, errors.Join(fmt.Errorf("downloading %s returned %s", url, response.Status), closeErr)
	}
	return response.Body, nil

}

// readNebulaRevision returns the locked nebula revision that jj-vine is built from.
func readNebulaRevision(path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is the flake lock the layout names under --repo-root.
	if err != nil {
		return "", fmt.Errorf("reading flake lock: %w", err)
	}
	var lock struct {
		Nodes map[string]struct {
			Locked struct {
				Rev string `json:"rev"`
			} `json:"locked"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(data, &lock); err != nil {
		return "", fmt.Errorf("decoding flake lock: %w", err)
	}
	revision := lock.Nodes["nebula"].Locked.Rev
	if revision == "" {
		return "", errors.New("flake lock has no nebula locked revision")
	}
	return revision, nil
}

func installBuilt(ctx context.Context, run Runner, nebulaRev string, built builtPins) error {
	previous, existed := os.LookupEnv("AWS_LC_SYS_PREBUILT_NASM")
	if err := os.Setenv("AWS_LC_SYS_PREBUILT_NASM", "1"); err != nil {
		return oops.In("windows-agent-sync").Wrapf(err, "setting jj-vine build environment")
	}
	_, installErr := run(ctx, "cargo", "install", "--locked", "--git", "https://github.com/RigelBuild/nebula", "--rev", nebulaRev, "jj-vine")
	var restoreErr error
	if existed {
		restoreErr = os.Setenv("AWS_LC_SYS_PREBUILT_NASM", previous)
	} else {
		restoreErr = os.Unsetenv("AWS_LC_SYS_PREBUILT_NASM")
	}
	if err := errors.Join(installErr, restoreErr); err != nil {
		return oops.In("windows-agent-sync").Wrapf(err, "installing jj-vine")
	}
	if _, err := run(ctx, "cargo", "install", "--locked", "jj-hooks", "--version", built.JJHooks); err != nil {
		return oops.In("windows-agent-sync").Wrapf(err, "installing jj-hooks")
	}
	if _, err := run(ctx, "bun", "install", "-g", "@schpet/linear-cli@"+built.LinearCLI); err != nil {
		return oops.In("windows-agent-sync").Wrapf(err, "installing Linear CLI")
	}
	return nil
}

func ensurePath(store EnvStore, dirs ...string) error {
	current, err := store.Get("Path")
	if err != nil {
		return oops.In("windows-agent-sync").Wrapf(err, "reading user PATH")
	}
	parts := []string{}
	for part := range strings.SplitSeq(current, ";") {
		if part != "" {
			parts = append(parts, part)
		}
	}
	added := false
	for _, dir := range dirs {
		found := false
		for _, part := range parts {
			if samePath(part, dir) {
				found = true
				break
			}
		}
		if !found {
			parts = append(parts, dir)
			added = true
		}
	}
	if !added {
		return nil
	}
	if err := store.Set("Path", strings.Join(parts, ";")); err != nil {
		return oops.In("windows-agent-sync").Wrapf(err, "writing user PATH")
	}
	if err := store.Broadcast(); err != nil {
		return oops.In("windows-agent-sync").Wrapf(err, "broadcasting user PATH change")
	}
	return nil
}

func samePath(left, right string) bool {
	clean := func(value string) string {
		value = strings.ReplaceAll(value, `\`, "/")
		return strings.ToLower(strings.TrimRight(filepath.Clean(value), "/"))
	}
	return clean(left) == clean(right)
}
