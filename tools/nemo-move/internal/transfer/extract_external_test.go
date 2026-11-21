package transfer_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/RigelBuild/nebula/tools/nemo-move/internal/macside"
	"github.com/RigelBuild/nebula/tools/nemo-move/internal/transfer"
)

func TestExportFrameExtractRoundTrip(t *testing.T) {
	home, _ := exportHome(t)
	setQuiet(t, home, time.Now().Add(-time.Hour))
	stageHome(t, home)
	journals := t.TempDir()
	journal := filepath.Join(journals, "pages.jsonl")
	if err := os.WriteFile(journal, []byte("journal\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(journal, 0o644); err != nil {
		t.Fatal(err)
	}
	var source bytes.Buffer
	lay := exampleLayout(t)
	lay.Journals.Dir = journals
	manifest, err := macside.Export(home, lay, &source)
	if err != nil {
		t.Fatal(err)
	}
	var framed, archive bytes.Buffer
	encoder := json.NewEncoder(&framed)
	if err := encoder.Encode(struct {
		Roots []string `json:"roots"`
	}{manifest.Roots}); err != nil {
		t.Fatal(err)
	}
	for _, entry := range manifest.Entries {
		if err := encoder.Encode(entry); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := framed.WriteString("null\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := framed.Write(source.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := transfer.FrameForTest(&framed, &archive); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if _, err := transfer.ExtractForTest(&archive, root); err != nil {
		t.Fatal(err)
	}
	for _, account := range lay.Google.Accounts {
		path := filepath.Join(root, lay.GoogleRoot(), account, "credentials/token.json")
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Stat(filepath.Join(root, "control/nemo-page/pages.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("journal mode = %o", info.Mode().Perm())
	}
}
