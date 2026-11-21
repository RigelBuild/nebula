package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RigelBuild/nebula/tools/nemo-move/internal/layout"
	"github.com/RigelBuild/nebula/tools/nemo-move/internal/macside"
	"github.com/RigelBuild/nebula/tools/nemo-move/internal/transfer"
)

type stopExportRemote struct{ home string }

func (r stopExportRemote) Run(_ context.Context, _ string, args ...string) (string, error) {
	cmd := args[len(args)-1]
	if cmd == transfer.HomeCommand {
		return testHome, nil
	}
	if strings.Contains(cmd, " mac-side inventory ") {
		lay, err := layoutFromCommand(cmd)
		if err != nil {
			return "", err
		}
		report, err := macside.Inventory(r.home, lay, time.Now())
		if err != nil {
			return "", err
		}
		data, err := json.Marshal(report)
		return string(data), err
	}
	if strings.Contains(cmd, "sudo -n") {
		lay, err := testLayoutFile()
		if err != nil {
			return "", err
		}
		stage := filepath.Join(r.home, layout.StagingDir)
		for _, account := range lay.Google.Accounts {
			if err := os.MkdirAll(filepath.Join(stage, account, "credentials"), 0o700); err != nil {
				return "", err
			}
		}
		return "", nil
	}
	if strings.Contains(cmd, "printf exists") {
		return "exists", nil
	}
	if strings.Contains(cmd, "printf fresh") {
		fresh, err := macside.StagingFresh(r.home)
		if fresh {
			return "fresh", err
		}
		return "", err
	}
	return "", errors.New("unexpected command")
}

// layoutFromCommand decodes the layout JSON that the mac-side command carries
// as its last single-quoted argument.
func layoutFromCommand(cmd string) (layout.Layout, error) {
	_, arg, ok := strings.Cut(cmd, " mac-side ")
	if !ok {
		return layout.Layout{}, errors.New("command has no mac-side subcommand")
	}
	_, quoted, ok := strings.Cut(arg, " ")
	if !ok || !strings.HasPrefix(quoted, "'") || !strings.HasSuffix(quoted, "'") {
		return layout.Layout{}, errors.New("command has no layout argument")
	}
	return layout.FromJSON(strings.ReplaceAll(quoted[1:len(quoted)-1], `'\''`, "'"))
}

// testLayoutFile is the made-up deployment every source-touching test loads.
// layout.Load expands its Google accounts into staging rows, as production does.
func testLayoutFile() (layout.Layout, error) {
	return layout.Load(testInventory)
}

// testLayoutFor loads the made-up layout, failing the test if it is invalid.
func testLayoutFor(t *testing.T) layout.Layout {
	t.Helper()
	lay, err := testLayoutFile()
	if err != nil {
		t.Fatal(err)
	}
	return lay
}
func (r stopExportRemote) StreamReader(context.Context, string, ...string) (io.ReadCloser, error) {
	var archive bytes.Buffer
	lay, err := testLayoutFile()
	if err != nil {
		return nil, err
	}
	manifest, err := macside.Export(r.home, lay, &archive)
	if err != nil {
		return nil, err
	}
	var framed bytes.Buffer
	encoder := json.NewEncoder(&framed)
	if err := encoder.Encode(struct {
		Roots []string `json:"roots"`
	}{manifest.Roots}); err != nil {
		return nil, err
	}
	for _, entry := range manifest.Entries {
		if err := encoder.Encode(entry); err != nil {
			return nil, err
		}
	}
	if _, err := framed.WriteString("null\n"); err != nil {
		return nil, err
	}
	if _, err := framed.Write(archive.Bytes()); err != nil {
		return nil, err
	}
	return io.NopCloser(&framed), nil
}
func (stopExportRemote) Upload(context.Context, string, string, string, string, os.FileMode) error {
	return nil
}
func (stopExportRemote) Hash(context.Context, string, string, string) (string, error) {
	digest := sha256.Sum256([]byte("binary"))
	return hex.EncodeToString(digest[:]), nil
}

func TestStopCreatesMissingQuietThenExport(t *testing.T) {
	home := t.TempDir()
	fake := newMacStopFake(false)
	for _, job := range testLaunchdJobs {
		fake.loaded[job.label] = false
		fake.disabled[job.label] = true
	}
	runner := func(ctx context.Context, name string, args ...string) (string, error) {
		cmd := args[len(args)-1]
		if strings.Contains(cmd, "nemo-move/last-quiet") {
			// The Linux gate host resolves macOS utility paths through PATH.
			cmd = strings.ReplaceAll(cmd, "/usr/bin/touch", "touch")
			cmd = strings.ReplaceAll(cmd, "/bin/mkdir", "mkdir")
			process := exec.CommandContext(ctx, "/bin/sh", "-c", cmd)
			process.Env = append(os.Environ(), "HOME="+home)
			output, err := process.CombinedOutput()
			return string(output), err
		}
		return fake.run(ctx, name, args...)
	}
	if err := stopMac(t.Context(), "example@192.0.2.10", testSSHPath, testStopConfig, runner); err != nil {
		t.Fatal(err)
	}
	lay := testLayoutFor(t)
	for _, row := range lay.Rows {
		if row.Kind == layout.Copy && !filepath.IsAbs(row.Path) {
			if err := os.MkdirAll(filepath.Join(home, row.Path), 0o700); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := stopMac(t.Context(), "example@192.0.2.10", testSSHPath, testStopConfig, runner); err != nil {
		t.Fatal(err)
	}
	binaryPath := filepath.Join(t.TempDir(), "binary")
	if err := os.WriteFile(binaryPath, []byte("binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	var output, stderr bytes.Buffer
	if err := transfer.Export(t.Context(), transfer.Options{SourceHome: testHome, MacUser: "example", Layout: testLayoutFor(t)}, "example@192.0.2.10", testSSHPath, binaryPath, stopExportRemote{home: home}, &output, &stderr); err != nil {
		t.Fatal(err)
	}
	if _, err := transfer.ReadArchiveManifest(&output); err != nil {
		t.Fatal(err)
	}
}

func TestStopActivityAfterStagingRefusesExport(t *testing.T) {
	home := t.TempDir()
	fake := newMacStopFake(false)
	for _, job := range testLaunchdJobs {
		fake.loaded[job.label] = false
		fake.disabled[job.label] = true
	}
	runner := func(ctx context.Context, name string, args ...string) (string, error) {
		cmd := args[len(args)-1]
		if strings.Contains(cmd, "nemo-move/last-quiet") {
			// The Linux gate host resolves macOS utility paths through PATH.
			cmd = strings.ReplaceAll(cmd, "/usr/bin/touch", "touch")
			cmd = strings.ReplaceAll(cmd, "/bin/mkdir", "mkdir")
			process := exec.CommandContext(ctx, "/bin/sh", "-c", cmd)
			process.Env = append(os.Environ(), "HOME="+home)
			output, err := process.CombinedOutput()
			return string(output), err
		}
		return fake.run(ctx, name, args...)
	}
	if err := stopMac(t.Context(), "example@192.0.2.10", testSSHPath, testStopConfig, runner); err != nil {
		t.Fatal(err)
	}
	for _, row := range testLayoutFor(t).Rows {
		if row.Kind == layout.Copy && !filepath.IsAbs(row.Path) {
			if err := os.MkdirAll(filepath.Join(home, row.Path), 0o700); err != nil {
				t.Fatal(err)
			}
		}
	}
	fake.loaded[testLaunchdJobs[0].label] = true
	if err := stopMac(t.Context(), "example@192.0.2.10", testSSHPath, testStopConfig, runner); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "binary")
	if err := os.WriteFile(bin, []byte("binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	var output, stderr bytes.Buffer
	err := transfer.Export(t.Context(), transfer.Options{SourceHome: testHome, MacUser: "example", Layout: testLayoutFor(t)}, "example@192.0.2.10", testSSHPath, bin, stopExportRemote{home: home}, &output, &stderr)
	if err == nil || output.Len() != 0 {
		t.Fatalf("stale export: error %v, output %d", err, output.Len())
	}
}
