package main

import (
	"path/filepath"
	"runtime"
	"testing"
)

// loadExampleConfig loads the generic example profile under testdata/. Tests use it so no
// deployment value appears in test source. The path is resolved from this file, because some tests chdir.
func loadExampleConfig(t *testing.T) Config {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test source directory")
	}
	cfg, err := loadConfig(filepath.Join(filepath.Dir(file), "testdata", "profile.cue"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// exampleKeys returns the issue-key rules for the example profile's prefix (EX).
func exampleKeys(t *testing.T) issueKeys {
	t.Helper()
	keys, err := newIssueKeys(loadExampleConfig(t).Repos["example/app"].IssuePrefix)
	if err != nil {
		t.Fatal(err)
	}
	return keys
}
