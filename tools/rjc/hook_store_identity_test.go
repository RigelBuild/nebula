package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestHookStoreIgnoresWrongRepository(t *testing.T) {
	store := localPrePush{root: t.TempDir()}
	head := strings.Repeat("1", 40)
	path, err := store.path("owner/repo", head)
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(path, "{}"); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(hookFile{Latest: HookRecord{Repo: "other/repo", Head: head, Result: hookPass}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, found, err := store.Read(t.Context(), "owner/repo", 0, head)
	if err != nil || found {
		t.Fatalf("foreign record accepted: %v %v", found, err)
	}
}
