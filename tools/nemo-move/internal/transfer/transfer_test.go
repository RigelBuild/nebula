package transfer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
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

const testSourceHome = "/Users/example"

var testOptions = Options{SourceHome: testSourceHome, MacUser: "example"}
