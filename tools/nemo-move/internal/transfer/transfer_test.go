package transfer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/RigelBuild/nebula/tools/nemo-move/internal/layout"
)

type fakeRemote struct{}

func (fakeRemote) Run(context.Context, string, ...string) (string, error) {
	return "", nil
}

func (fakeRemote) StreamReader(context.Context, string, ...string) (io.ReadCloser, error) {
	return nil, errors.New("unexpected stream")
}

func TestAtomicManifestWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	data := []byte(`{"roots":["opt/data"],"entries":[]}`)
	if err := WriteManifest(path, data); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("manifest = %s, want %s", got, data)
	}
}

const testSourceHome = "/src/home/example"

// testLayout is the made-up deployment in testdata/inventory.cue, loaded by
// TestMain so every transfer test runs against the same file operators pass
// with --inventory.
var testLayout layout.Layout

var testOptions Options

func TestMain(m *testing.M) {
	lay, err := layout.Load(filepath.Join("..", "..", "testdata", "inventory.cue"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "load testdata/inventory.cue:", err)
		os.Exit(1)
	}
	testLayout = lay
	testOptions = Options{SourceHome: testSourceHome, MacUser: "example", Layout: testLayout}
	os.Exit(m.Run())
}
