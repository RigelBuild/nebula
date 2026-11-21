package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/inventory"
)

func TestInventoryRowsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inventory")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	want := inventory.Inventory{Lines: []inventory.Line{{Path: "/file"}}, Errors: []inventory.AssignError{{Path: "/unknown", Reason: "unknown"}}, Dangling: []inventory.Dangling{{Path: "/file", Reason: "missing"}}, Excluded: []inventory.Excluded{{Count: 2, Bytes: 3}}, OutsideTargets: 4}
	if err := writeInventory(file, want); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := readInventory(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Lines) != 1 || len(got.Errors) != 1 || len(got.Dangling) != 1 || len(got.Excluded) != 1 || got.OutsideTargets != 4 {
		t.Fatalf("incomplete inventory: %+v", got)
	}
}
