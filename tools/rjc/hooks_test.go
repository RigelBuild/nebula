package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestSSHHookHKFetchSelection(t *testing.T) {
	for _, managed := range []bool{false, true} {
		t.Run(strconv.FormatBool(managed), func(t *testing.T) {
			f, runner := sshGateFixture(t)
			if !managed {
				if err := os.Remove(filepath.Join(f.repo.root, "devenv.nix")); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Remove(filepath.Join(os.Getenv("HOME"), "bin", "hk")); err != nil {
				t.Fatal(err)
			}
			path := hostPathWithout(t, "hk")
			f.repo.env = append(f.repo.env, "PATH="+path)
			t.Setenv("RJC_TEST_NIX_BUILD_FAIL", "1")
			f.commit(t, "hk.pkl")
			base, head := sshRange(t, f)
			if _, _, err := runner.Run(f.ctx, base, head, nil, io.Discard); err == nil {
				t.Fatal("missing hk must fail")
			}
			log, err := os.ReadFile(os.Getenv("RJC_TEST_NIX_LOG"))
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(string(log), "store prefetch-file"); got == managed {
				t.Fatalf("managed=%t prefetch=%t log=%s", managed, got, log)
			}
		})
	}
}

func TestLocalHookRunnerPhases(t *testing.T) {
	for _, tc := range []struct {
		name, phase    string
		fail, mismatch bool
		wantError      bool
	}{
		{name: "pass", phase: "runner-completed"},
		{name: "fail", phase: "runner-completed", fail: true},
		{name: "no config", phase: "no-config"},
		{name: "setup failed", phase: "setup-failed", wantError: true},
		{name: "fixup", phase: "fixup"},
		{name: "wrong base", phase: "runner-completed", mismatch: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fixtureRepo(t)
			t.Setenv("RJC_TEST_HOOK_PHASE", tc.phase)
			if tc.fail {
				t.Setenv("RJC_TEST_HOOK_FAIL", "1")
			}
			if tc.mismatch {
				t.Setenv("RJC_TEST_HOOK_MISMATCH", "1")
			}
			out, _, err := (localHookRunner{repo: f.repo}).Run(f.ctx, strings.Repeat("1", 40), strings.Repeat("2", 40), nil, io.Discard)
			if (err != nil) != tc.wantError {
				t.Fatalf("got error %v", err)
			}
			if tc.mismatch {
				var detail *failure
				if !errors.As(err, &detail) || detail.code != 3 {
					t.Fatalf("wrong base exit code: %v", err)
				}
			}
			if !tc.wantError && (string(out.Phase) != tc.phase || (out.Exit != 0) != tc.fail) {
				t.Fatalf("unexpected outcome %+v", out)
			}
		})
	}
}

func TestHookIdentityConfigTable(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"hk.pkl":       "hk",
		"lefthook.yml": "lefthook", "lefthook.yaml": "lefthook", "lefthook.json": "lefthook", "lefthook.jsonc": "lefthook", "lefthook.toml": "lefthook",
		".lefthook.yml": "lefthook", ".lefthook.yaml": "lefthook", ".lefthook.json": "lefthook", ".lefthook.jsonc": "lefthook", ".lefthook.toml": "lefthook",
		".config/lefthook.yml": "lefthook", ".config/lefthook.yaml": "lefthook", ".config/lefthook.json": "lefthook", ".config/lefthook.jsonc": "lefthook", ".config/lefthook.toml": "lefthook",
		".pre-commit-config.yaml": "pre-commit", ".pre-commit-config.yml": "pre-commit",
		"prek.toml": "prek", ".prek.toml": "prek",
	}
	if len(hookConfigs) != len(want) {
		t.Fatalf("config table has %d entries, want %d", len(hookConfigs), len(want))
	}
	for _, config := range hookConfigs {
		t.Run(config.path, func(t *testing.T) {
			f := parallelFixtureRepo(t)
			f.commit(t, config.path)
			base, head := sshRange(t, f)
			rec, got, err := f.repo.hookIdentityConfig(f.ctx, Change{Base: base, Tip: head})
			if err != nil || got.path != config.path || got.kind != want[config.path] || rec.HookConfigBlob == "" {
				t.Fatalf("config = %+v record=%+v err=%v", got, rec, err)
			}
		})
	}
}

func TestHookIdentityConfigPrecedence(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	f.commit(t, "lefthook.yml")
	f.commit(t, "hk.pkl")
	base, head := sshRange(t, f)
	_, config, err := f.repo.hookIdentityConfig(f.ctx, Change{Base: base, Tip: head})
	if err != nil || config != (hookConfig{path: "hk.pkl", kind: "hk"}) {
		t.Fatalf("config = %+v err=%v", config, err)
	}
}

func TestHookRecordCurrent(t *testing.T) {
	rec := HookRecord{Repo: "a/b", Head: "h", Base: "b", HookConfigBlob: "c"}
	if !rec.Current("a/b", "h", "b", "c") || rec.Current("other/repo", "h", "b", "c") || rec.Current("a/b", "h", "other", "c") || rec.Current("a/b", "h", "b", "other") {
		t.Fatal("hook identity not enforced")
	}
}

func TestHookStoreHistory(t *testing.T) {
	s := localPrePush{root: t.TempDir()}
	rec := HookRecord{Repo: "a/b", Head: strings.Repeat("1", 40), Result: "fail"}
	if err := s.Save(rec); err != nil {
		t.Fatal(err)
	}
	rec.Result = "partial"
	if err := s.Save(rec); err != nil {
		t.Fatal(err)
	}
	rec.Result = "skipped"
	if err := s.Save(rec); err != nil {
		t.Fatal(err)
	}
	history, err := s.History(rec.Repo, rec.Head)
	if err != nil || len(history) != 2 {
		t.Fatalf("history %v, error %v", history, err)
	}
	rec.Result = "pass"
	if err := s.Save(rec); err != nil {
		t.Fatal(err)
	}
	history, err = s.History(rec.Repo, rec.Head)
	if err != nil || len(history) != 0 {
		t.Fatalf("pass did not clear history: %v, %v", history, err)
	}
}

func TestLedger(t *testing.T) {
	l := Ledger{root: t.TempDir()}
	e := LedgerEntry{Repo: "owner/repo", PR: 42, Bookmark: "test/bookmark", Codename: "test"}
	if err := l.Save(e); err != nil {
		t.Fatal(err)
	}
	got, ok, err := l.Find(e.Repo, e.Bookmark)
	if err != nil || !ok || got.PR != e.PR {
		t.Fatalf("find: %+v %v %v", got, ok, err)
	}
}

func TestMissingReportsFailClosed(t *testing.T) {
	_, err := readMoonReport(t.TempDir()+"/missing", nil, nil)
	if err == nil {
		t.Fatal("missing moon report accepted")
	}
	_, err = readGateReport(t.TempDir() + "/missing")
	if err == nil {
		t.Fatal("missing gate report accepted")
	}
}
