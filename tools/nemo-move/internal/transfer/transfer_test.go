package transfer

import (
	"bytes"
	"context"
	"errors"
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

const testSourceHome = "/Users/example"

// testLayout is a made-up deployment with the same shape as testdata/inventory.cue.
var testLayout = layout.Layout{
	Rows: []layout.Row{
		{Path: ".example-agent", Kind: layout.Copy, Dest: "/opt/data"},
	},
	Google:    layout.Google{Source: "/var/lib/example-google/accounts", Dest: "/opt/data/.local/state/example-google", Accounts: []string{"workspace", "personal"}},
	Journals:  layout.Journals{Dir: "/var/lib/example-journals", Names: []string{"pages.jsonl", "events.jsonl"}},
	Processes: []string{"example-agent --serve"},
}

var testOptions = Options{SourceHome: testSourceHome, MacUser: "example", Layout: testLayout}
