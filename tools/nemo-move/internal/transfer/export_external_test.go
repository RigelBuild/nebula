package transfer_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RigelBuild/nebula/tools/nemo-move/internal/layout"
	"github.com/RigelBuild/nebula/tools/nemo-move/internal/macside"
	"github.com/RigelBuild/nebula/tools/nemo-move/internal/transfer"
)

type exportRemote struct {
	inventoryRemote
	noSudo bool
}

func (r exportRemote) Run(ctx context.Context, name string, args ...string) (string, error) {
	command := args[len(args)-1]
	if command == transfer.HomeCommand || strings.Contains(command, " mac-side inventory ") {
		return r.inventoryRemote.Run(ctx, name, args...)
	}
	if strings.Contains(command, "printf exists") {
		if _, err := os.Stat(filepath.Join(r.home, ".local/state/nemo-move-staging")); err == nil {
			return "exists", nil
		}
		return "absent", nil
	}
	if strings.Contains(command, "printf fresh") {
		fresh, err := macside.StagingFresh(r.home)
		if fresh {
			return "fresh", err
		}
		return "", err
	}
	if !r.noSudo && strings.Contains(command, "sudo -n") {
		stage := filepath.Join(r.home, ".local/state/nemo-move-staging")
		if err := os.RemoveAll(stage); err != nil {
			return "", err
		}
		text, err := exampleLayoutJSON()
		if err != nil {
			return "", err
		}
		lay, err := layout.FromJSON(text)
		if err != nil {
			return "", err
		}
		for _, account := range lay.Google.Accounts {
			if err := os.MkdirAll(filepath.Join(stage, account, "credentials"), 0o700); err != nil {
				return "", err
			}
		}
		return "", nil
	}
	if r.noSudo && strings.Contains(command, "sudo -n") {
		return "", errors.New("sudo unavailable")
	}
	return "", errors.New("unexpected remote command")
}

func (r exportRemote) StreamReader(_ context.Context, _ string, _ ...string) (io.ReadCloser, error) {
	var archive bytes.Buffer
	text, err := exampleLayoutJSON()
	if err != nil {
		return nil, err
	}
	lay, err := layout.FromJSON(text)
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

func exportHome(t *testing.T) (string, string) {
	t.Helper()
	home := t.TempDir()
	for _, row := range exampleLayout(t).Rows {
		if row.Kind == layout.Copy && !filepath.IsAbs(row.Path) && !strings.Contains(row.Path, "nemo-move-staging") {
			if err := os.MkdirAll(filepath.Join(home, row.Path), 0o700); err != nil {
				t.Fatal(err)
			}
		}
	}
	bin := filepath.Join(t.TempDir(), "binary")
	if err := os.WriteFile(bin, []byte("binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	return home, bin
}

func setQuiet(t *testing.T, home string, when time.Time) {
	t.Helper()
	path := filepath.Join(home, ".local/state/nemo-move/last-quiet")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

func stageHome(t *testing.T, home string) {
	t.Helper()
	for _, account := range exampleLayout(t).Google.Accounts {
		dir := filepath.Join(home, ".local/state/nemo-move-staging", account, "credentials")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "token.json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func runExport(t *testing.T, home, bin string) (bytes.Buffer, bytes.Buffer, error) {
	t.Helper()
	var out, stderr bytes.Buffer
	err := transfer.Export(t.Context(), transfer.Options{SourceHome: "/Users/example", MacUser: "example", Layout: exampleLayout(t)}, "example@192.0.2.10", "/ssh", bin, exportRemote{inventoryRemote: inventoryRemote{home: home}, noSudo: true}, &out, &stderr)
	return out, stderr, err
}

func TestExportNoSudoThenStagedRerun(t *testing.T) {
	home, bin := exportHome(t)
	setQuiet(t, home, time.Now().Add(-time.Hour))
	out, stderr, err := runExport(t, home, bin)
	if err == nil || out.Len() != 0 || strings.Count(stderr.String(), "sudo /bin/sh -c") != 1 {
		t.Fatalf("initial export: error %v, output %d, stderr %q", err, out.Len(), stderr.String())
	}
	stageHome(t, home)
	out, _, err = runExport(t, home, bin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transfer.ReadArchiveManifest(&out); err != nil {
		t.Fatal(err)
	}
}

func TestExportJobAfterStagingRefuses(t *testing.T) {
	home, bin := exportHome(t)
	stageHome(t, home)
	setQuiet(t, home, time.Now().Add(time.Hour))
	out, _, err := runExport(t, home, bin)
	if err == nil || out.Len() != 0 {
		t.Fatalf("stale export: error %v, output %d", err, out.Len())
	}
}

func TestExportAfterMissingQuietMarkerCreated(t *testing.T) {
	home, bin := exportHome(t)
	setQuiet(t, home, time.Now().Add(-time.Hour))
	stageHome(t, home)
	out, _, err := runExport(t, home, bin)
	if err != nil || out.Len() == 0 {
		t.Fatalf("export after quiet marker: error %v, output %d", err, out.Len())
	}
}

func TestExportRestagesWhenSudoAvailable(t *testing.T) {
	home, bin := exportHome(t)
	stageHome(t, home)
	setQuiet(t, home, time.Now())
	var out, stderr bytes.Buffer
	err := transfer.Export(t.Context(), transfer.Options{SourceHome: "/Users/example", MacUser: "example", Layout: exampleLayout(t)}, "example@192.0.2.10", "/ssh", bin, exportRemote{inventoryRemote: inventoryRemote{home: home}}, &out, &stderr)
	if err != nil || out.Len() == 0 {
		t.Fatalf("restaged export: error %v, output %d, stderr %q", err, out.Len(), stderr.String())
	}
}

func TestExportMissingQuietRefuses(t *testing.T) {
	home, bin := exportHome(t)
	stageHome(t, home)
	out, stderr, err := runExport(t, home, bin)
	if err == nil || out.Len() != 0 || strings.Count(stderr.String(), "sudo /bin/sh -c") != 1 {
		t.Fatalf("missing quiet: error %v, output %d, stderr %q", err, out.Len(), stderr.String())
	}
}

const exampleHome = "/Users/example"

// inventoryRemote serves a local home as the Mac. remoteHome is the $HOME it
// reports; empty means exampleHome.
type inventoryRemote struct {
	home       string
	remoteHome string
}

func (r inventoryRemote) Run(_ context.Context, _ string, args ...string) (string, error) {
	if args[len(args)-1] == transfer.HomeCommand {
		if r.remoteHome == "" {
			return exampleHome, nil
		}
		return r.remoteHome, nil
	}
	lay, err := layoutFromCommand(args[len(args)-1])
	if err != nil {
		return "", err
	}
	report, inventoryErr := macside.Inventory(r.home, lay, time.Now())
	data, err := json.Marshal(report)
	return string(data), errors.Join(inventoryErr, err)
}
func (inventoryRemote) StreamReader(context.Context, string, ...string) (io.ReadCloser, error) {
	return nil, errors.New("unexpected stream")
}
func (inventoryRemote) Upload(context.Context, string, string, string, string, os.FileMode) error {
	return nil
}
func (inventoryRemote) Hash(context.Context, string, string, string) (string, error) {
	digest := sha256.Sum256([]byte("binary"))
	return hex.EncodeToString(digest[:]), nil
}

func TestInventoryMissingCopyRow(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(t.TempDir(), "mac-binary")
	if err := os.WriteFile(bin, []byte("binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	rows, err := transfer.Inventory(t.Context(), "example@192.0.2.10", "/ssh", bin, inventoryRemote{home: home}, exampleLayout(t))
	if err == nil || len(rows) == 0 {
		t.Fatalf("missing Copy row: error %v, rows %d", err, len(rows))
	}
}

func TestCheckSourceHome(t *testing.T) {
	opts := transfer.Options{SourceHome: exampleHome, MacUser: "example", Layout: exampleLayout(t)}
	for _, tc := range []struct {
		name       string
		remoteHome string
		wantErr    bool
	}{
		{name: "match", remoteHome: exampleHome},
		{name: "mismatch", remoteHome: "/Users/other", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := transfer.CheckSourceHome(t.Context(), opts, "example@192.0.2.10", "/ssh", inventoryRemote{remoteHome: tc.remoteHome})
			if (err != nil) != tc.wantErr {
				t.Fatalf("CheckSourceHome error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestExportRefusesMismatchedRemoteHome(t *testing.T) {
	home, bin := exportHome(t)
	stageHome(t, home)
	setQuiet(t, home, time.Now().Add(-time.Hour))
	var out, stderr bytes.Buffer
	remote := exportRemote{inventoryRemote: inventoryRemote{home: home, remoteHome: "/Users/other"}}
	err := transfer.Export(t.Context(), transfer.Options{SourceHome: exampleHome, MacUser: "example", Layout: exampleLayout(t)}, "example@192.0.2.10", "/ssh", bin, remote, &out, &stderr)
	if err == nil || !strings.Contains(err.Error(), "does not match source home") || out.Len() != 0 {
		t.Fatalf("mismatched home: error %v, output %d", err, out.Len())
	}
}
