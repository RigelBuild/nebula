package hookshell

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestSelectUsesRootLockedDevenvNixpkgs(t *testing.T) {
	data, err := os.ReadFile("../../../../devenv.lock")
	if err != nil {
		t.Fatal(err)
	}
	var lock struct {
		Root  string `json:"root"`
		Nodes map[string]struct {
			Inputs map[string]json.RawMessage `json:"inputs"`
			Locked struct {
				Owner string `json:"owner"`
				Repo  string `json:"repo"`
				Rev   string `json:"rev"`
			} `json:"locked"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(data, &lock); err != nil {
		t.Fatal(err)
	}
	root, ok := lock.Nodes[lock.Root]
	if !ok {
		t.Fatalf("devenv.lock has no root node %q", lock.Root)
	}
	input, ok := root.Inputs["nixpkgs"]
	if !ok {
		t.Fatal("devenv.lock root has no nixpkgs input")
	}
	var nixpkgs string
	if err := json.Unmarshal(input, &nixpkgs); err != nil {
		t.Fatalf("decode devenv.lock root nixpkgs input: %v", err)
	}
	locked, ok := lock.Nodes[nixpkgs]
	if !ok {
		t.Fatalf("devenv.lock root nixpkgs input points to missing node %q", nixpkgs)
	}
	parts := strings.Split(strings.TrimPrefix(DevenvNixpkgs, "github:"), "/")
	if len(parts) != 3 || locked.Locked.Owner != parts[0] || locked.Locked.Repo != parts[1] || locked.Locked.Rev != parts[2] {
		t.Fatalf("devenv.lock root nixpkgs = %s/%s/%s, DevenvNixpkgs = %q", locked.Locked.Owner, locked.Locked.Repo, locked.Locked.Rev, DevenvNixpkgs)
	}
	if !strings.Contains(Select, DevenvNixpkgs+"#devenv") {
		t.Fatalf("shell selection does not use pinned devenv ref %q", DevenvNixpkgs+"#devenv")
	}
	if strings.Contains(Select, Nixpkgs+"#devenv") {
		t.Fatalf("shell selection uses the shared nixpkgs pin for devenv")
	}
}
