package config

import (
	"os"
	"path/filepath"
	"testing"
)

// fixtureConfig is the synthetic archive config under testdata/.
const fixtureConfig = "../../testdata/archive.cue"

func TestLoadAndWalk(t *testing.T) {
	t.Parallel()
	loaded, err := Load(fixtureConfig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loaded.Walk("example-host"); err != nil {
		t.Fatalf("example-host is missing: %v", err)
	}
	configPath := writeConfigFile(t, `package archive
hosts: fixture: {
	state: "unmeasured"
	sources: [{name: "sessions", path: "sessions"}]
	watch: [".omp"]
	exclude: []
	finalOnly: []
}
`)
	loaded, err = Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	walk, err := loaded.Walk("fixture")
	if err != nil {
		t.Fatal(err)
	}
	if len(walk.Sources) != 1 || walk.Sources[0].Name != "sessions" || len(walk.Watch) != 1 {
		t.Fatalf("Walk fixture = %#v", walk)
	}
	if _, err := loaded.Walk("missing"); err == nil {
		t.Fatal("unknown host accepted")
	}
}

func TestLoadRejectsSchemaViolation(t *testing.T) {
	t.Parallel()
	configPath := writeConfigFile(t, `package archive
hosts: fixture: {
  state: "unmeasured"
  sources: [{name: "sessions"}]
  watch: []
  exclude: []
  finalOnly: []
}
`)
	if _, err := Load(configPath); err == nil {
		t.Fatal("host missing required source path accepted")
	}
}

func TestLoadRejectsHostWithoutState(t *testing.T) {
	t.Parallel()
	configPath := writeConfigFile(t, `package archive
hosts: fixture: {
  sources: [{name: "sessions", path: "sessions"}]
  watch: []
  exclude: []
  finalOnly: []
}
`)
	if _, err := Load(configPath); err == nil {
		t.Fatal("host without state accepted")
	}
}

func TestFixtureHostsCarryState(t *testing.T) {
	t.Parallel()
	loaded, err := Load(fixtureConfig)
	if err != nil {
		t.Fatal(err)
	}
	for host, want := range map[string]string{
		"example-host":  "unmeasured",
		"example-empty": "empty",
	} {
		walk, err := loaded.Walk(host)
		if err != nil {
			t.Fatalf("host %s: %v", host, err)
		}
		if walk.State != want {
			t.Errorf("host %s state = %q, want %q", host, walk.State, want)
		}
	}
}

// writeConfigFile writes content as an archive config in a temp directory.
// Load validates against the schema embedded in the package, so no sibling schema is written.
func writeConfigFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "archive.cue")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
