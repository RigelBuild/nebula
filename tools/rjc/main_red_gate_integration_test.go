package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMainRedRechecksNamedGate(t *testing.T) {
	f := fixtureRepo(t)
	f.repo.profile.HookReports = true
	source := `package main
import ("os";"strings")
func main(){ p:=os.Getenv("RJC_TEST_GATE_ARGS"); if os.WriteFile(p,[]byte(strings.Join(os.Args[1:],"\n")),0600)!=nil {os.Exit(3)}; if os.WriteFile(os.Getenv("RJC_GATE_REPORT"),[]byte("[{\"name\":\"sea-ref\",\"exit\":1}]"),0600)!=nil {os.Exit(3)}; os.Exit(1) }
`
	writeTestFile(t, filepath.Join(f.repo.root, "tools/gate-runner/main.go"), source)
	writeTestFile(t, filepath.Join(f.repo.root, "go.mod"), "module example.test/fixture\n\ngo 1.23\n")
	jjTest(t, f.repo, "commit", "-m", "chore: add gate fixture")
	jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", "main")
	tip := jjTest(t, f.repo, "log", "--no-graph", "-r", mainOrigin, "-T", "commit_id")
	args := filepath.Join(t.TempDir(), "args")
	t.Setenv("RJC_TEST_GATE_ARGS", args)
	rec := HookRecord{Result: hookSkipped, Skip: "main-red:#1", FailedGates: []string{"sea-ref"}}
	passed, err := f.repo.recheckMainRed(f.ctx, f.repo.profile, rec, tip)
	if err != nil || len(passed) != 0 {
		t.Fatalf("passed %v error %v", passed, err)
	}
	data, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "--only\nsea-ref\n--full" {
		t.Fatalf("args %q", data)
	}
	if workspaces := jjTest(t, f.repo, "workspace", "list"); strings.Contains(workspaces, "rjc-main-red-") {
		t.Fatalf("workspace leaked: %s", workspaces)
	}
}
