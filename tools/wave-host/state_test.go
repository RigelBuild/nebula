package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run", "state.json")
	want := State{Version: 1, State: "ready", Host: "macbook", InstanceID: "one"}
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil || got != want {
		t.Fatalf("got %+v: %v", got, err)
	}
	file, err := os.Stat(path)
	if err != nil || file.Mode().Perm() != 0600 {
		t.Fatalf("file mode: %v %v", file, err)
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil || dir.Mode().Perm() != 0700 {
		t.Fatalf("directory mode: %v %v", dir, err)
	}
}

func TestStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	got := status(path, "host")
	if got.State != "stopped" || got.Host != "host" {
		t.Fatalf("missing: %+v", got)
	}
	if err := os.WriteFile(path, []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	got = status(path, "host")
	if got.State != "blocked" || got.Reason == "" {
		t.Fatalf("bad state: %+v", got)
	}
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result["state"] != "blocked" || result["host"] != "host" || result["reason"] == nil || result["instanceId"] != nil {
		t.Fatalf("json: %s", data)
	}
}
