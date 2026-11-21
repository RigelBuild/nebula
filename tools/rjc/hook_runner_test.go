package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestHookReports(t *testing.T) {

	dir := t.TempDir()
	moon := filepath.Join(dir, "moon.json")
	gates := filepath.Join(dir, "gates.json")
	content := `{"actions":[{"status":"failed","node":{"action":"run-task","params":{"target":"a:test"}}},{"status":"passed","node":{"action":"run-task","params":{"target":"b:test"}}}]}`
	if err := os.WriteFile(moon, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gates, []byte(`[{"name":"gate-a","exit":1},{"name":"gate-b","exit":0}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	failed, err := readMoonReport(moon, []string{"a:test", "b:test"}, nil)
	if err != nil || !reflect.DeepEqual(failed, []string{"a:test"}) {
		t.Fatalf("moon failed=%v err=%v", failed, err)
	}
	failed, err = readGateReport(gates)
	if err != nil || !reflect.DeepEqual(failed, []string{"gate-a"}) {
		t.Fatalf("gates failed=%v err=%v", failed, err)
	}
	if _, err := readMoonReport(moon, []string{"missing:test"}, nil); err == nil {
		t.Fatal("missing scheduled target accepted")
	}
	if _, err := readMoonReport(moon, nil, []SkippedTarget{{Target: "a:test"}}); err == nil {
		t.Fatal("skipped target ran")
	}
	for status, wantFailed := range map[string]bool{"aborted": true, "timed-out": true, "invalid": true, "skipped": true, "running": true, "cached": false, "cached-from-remote": false} {
		report := `{"actions":[{"status":"` + status + `","node":{"action":"run-task","params":{"target":"a:test"}}}]}`
		if err := os.WriteFile(moon, []byte(report), 0o600); err != nil {
			t.Fatal(err)
		}
		failed, err = readMoonReport(moon, []string{"a:test"}, nil)
		if err != nil || (len(failed) == 1) != wantFailed {
			t.Fatalf("%s: failed=%v err=%v", status, failed, err)
		}
	}
}
func TestHookRunnerRangeEnvironment(t *testing.T) {
	f := fixtureRepo(t)
	path := filepath.Join(t.TempDir(), "environment.json")
	t.Setenv("RJC_TEST_HOOK_ENV", path)
	t.Setenv("HK_FILE", "hk.local.pkl")
	base, head := strings.Repeat("1", 40), strings.Repeat("2", 40)
	if _, _, err := (localHookRunner{repo: f.repo}).Run(f.ctx, base, head, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]string
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatal(err)
	}
	if env["base"] != base || env["head"] != head {
		t.Fatalf("range environment: %v", env)
	}
	if env["hkFile"] != "hk.pkl" {
		t.Fatalf("hook config environment: %v", env)
	}
}
