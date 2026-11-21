package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func installHookFixture(t *testing.T, home string, parallel bool) {
	t.Helper()
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(self, filepath.Join(bin, "jj-hp")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(self, filepath.Join(bin, "moon")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(self, filepath.Join(bin, "hk")); err != nil {
		t.Fatal(err)
	}
	if !parallel {
		t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		t.Setenv("RJC_TEST_HELPER", "1")
	}
}

func failChangedHookFile(out *HookOutcome) error {
	file := os.Getenv("RJC_TEST_HOOK_FAIL_FILE")
	if file == "" {
		return nil
	}
	changed, err := exec.Command("git", "diff", "--name-only", out.From, out.To).Output()
	if err != nil {
		return err
	}
	for path := range strings.SplitSeq(string(changed), "\n") {
		if path == file {
			out.Exit = 1
		}
	}
	return nil
}

func hookToolHelper() int {
	args := os.Args[1:]
	get := func(flag string) string {
		for i := 0; i+1 < len(args); i++ {
			if args[i] == flag {
				return args[i+1]
			}
		}
		return ""
	}
	if path := os.Getenv("RJC_TEST_HOOK_ENV"); path != "" {
		data, err := json.Marshal(map[string]string{"base": os.Getenv("MOON_BASE"), "head": os.Getenv("MOON_HEAD"), "targets": os.Getenv("RJC_MOON_TARGETS"), "hkFile": os.Getenv("HK_FILE")})
		if err != nil || os.WriteFile(path, data, 0o600) != nil {
			return 89
		}
	}
	phase := os.Getenv("RJC_TEST_HOOK_PHASE")
	if phase == "" {
		phase = "runner-completed"
	}
	config := os.Getenv("RJC_TEST_HOOK_CONFIG")
	if config == "" {
		config = hkConfigName
	}
	out := HookOutcome{Phase: HookPhase(phase), From: get("--from"), To: get("--to"), Config: config}
	if os.Getenv("RJC_TEST_HOOK_FAIL") == "1" {
		out.Exit = 1
	}
	if err := failChangedHookFile(&out); err != nil {
		return 93
	}
	if os.Getenv("RJC_TEST_HOOK_MISMATCH") == "1" {
		out.From = strings.Repeat("a", 40)
	}
	if phase == "fixup" {
		out.Fixup = strings.Repeat("f", 40)
	}
	data, err := json.Marshal(out)
	if err != nil || os.WriteFile(get("--result-json"), data, 0o600) != nil {
		return 90
	}
	if err := copyHookReport(); err != nil {
		return 91
	}
	if path := os.Getenv("RJC_MOON_REPORT"); path != "" && os.Getenv("RJC_TEST_MISSING_REPORT") != "1" && os.Getenv("RJC_TEST_MOON_REPORT_FILE") == "" {
		actions := []map[string]any{}
		for target := range strings.SplitSeq(os.Getenv("RJC_MOON_TARGETS"), ",") {
			if target != "" {
				status := "passed"
				if target == os.Getenv("RJC_TEST_FAILED_TARGET") {
					status = "failed"
				}
				actions = append(actions, map[string]any{"status": status, "node": map[string]any{"action": "run-task", "params": map[string]string{"target": target}}})
			}
		}
		if os.Getenv("RJC_TEST_MOON_FAIL") == "1" {
			actions = append(actions, map[string]any{"status": "failed", "node": map[string]any{"action": "run-task", "params": map[string]string{"target": "a:test"}}})
		}
		report, err := json.Marshal(map[string]any{"actions": actions})
		if err != nil || os.WriteFile(path, report, 0o600) != nil {
			return 91
		}
	}
	if path := os.Getenv("RJC_GATE_REPORT"); path != "" {
		gateReport := `[]`
		if os.Getenv("RJC_TEST_GATE_FAIL") == "1" {
			gateReport = `[{"name":"gate-a","exit":1}]`
		}
		if err := os.WriteFile(path, []byte(gateReport), 0o600); err != nil {
			return 92
		}
	}
	return out.Exit
}

func copyHookReport() error {
	source := os.Getenv("RJC_TEST_MOON_REPORT_FILE")
	if source == "" {
		return nil
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return os.WriteFile(os.Getenv("RJC_MOON_REPORT"), data, 0o600)
}

//nolint:nestif // The fixture models both moon query and recheck execution.
func moonToolHelper() int {
	if len(os.Args) >= 3 && os.Args[1] == "run" {
		status := os.Getenv("RJC_TEST_RECHECK_STATUS")
		if expected := os.Getenv("RJC_TEST_FAIL_AT"); expected != "" {
			output, err := exec.Command("jj", "log", "--no-graph", "-r", "@-", "-T", "commit_id").Output()
			if err != nil {
				return 90
			}
			status = "passed"
			if strings.TrimSpace(string(output)) == expected {
				status = "failed"
			}
		}
		if status == "" {
			status = "passed"
		}
		data, err := json.Marshal(map[string]any{"actions": []any{map[string]any{"status": status, "node": map[string]any{"action": "run-task", "params": map[string]string{"target": os.Args[2]}}}}})
		if err != nil {
			return 90
		}
		if err := os.MkdirAll(".moon/cache", 0o700); err != nil {
			return 90
		}
		if err := os.WriteFile(".moon/cache/runReport.json", data, 0o600); err != nil {
			return 90
		}
		if status == "failed" {
			return 1
		}
		return 0
	}
	if len(os.Args) > 1 && os.Args[1] == "query" {
		if query := os.Getenv("RJC_TEST_MOON_QUERY"); query != "" {
			if _, err := os.Stdout.WriteString(query); err != nil {
				return 90
			}
			return 0
		}
		_, err := os.Stdout.WriteString(`{"tasks":{"a":{"test":{"target":"a:test","options":{"runInCI":true}},"other":{"target":"a:other","options":{"runInCI":true}}}}}`)
		if err != nil {
			return 90
		}
		return 0
	}
	_, err := os.Stdout.WriteString(`{"tasks":{"a":{"test":{"target":"a:test"},"other":{"target":"a:other"}}}}`)
	if err != nil {
		return 90
	}
	return 0
}

func hkToolHelper() int {
	if path := os.Getenv("RJC_TEST_HK_ARGS"); path != "" {
		text := strings.Join(os.Args[1:], "\n") + "\n" + os.Getenv("MOON_BASE") + "\n" + os.Getenv("MOON_HEAD") + "\n" + os.Getenv("HK_FILE")
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			return 90
		}
	}
	// RJC_TEST_HK_TARGET names the one moon target this run reports on. RJC_TEST_HK_TARGET_FAILED=1 marks it
	// failed, independent of the exit code. readMoonReport then reads the report as it reads a real hk run's.
	if target := os.Getenv("RJC_TEST_HK_TARGET"); target != "" && os.Getenv("RJC_MOON_REPORT") != "" {
		status := "passed"
		if os.Getenv("RJC_TEST_HK_TARGET_FAILED") == "1" {
			status = "failed"
		}
		report, err := json.Marshal(map[string]any{"actions": []any{map[string]any{"status": status, "node": map[string]any{"action": "run-task", "params": map[string]string{"target": target}}}}})
		if err != nil || os.WriteFile(os.Getenv("RJC_MOON_REPORT"), report, 0o600) != nil {
			return 91
		}
	}
	if os.Getenv("RJC_TEST_HK_FAIL") == "1" {
		return 1
	}
	return 0
}
