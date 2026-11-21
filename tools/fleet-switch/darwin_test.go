package main

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
)

const testStorePath = "/nix/store/0123456789abcdfghijklmnpqrsvwxyz-example-src"

// darwinRunner records each command, answers prefetch with prefetchOut, and fails the step named failStep.
func darwinRunner(calls *[][]string, prefetchOut, failStep string) commandRunner {
	return func(_ context.Context, name string, args []string, stdout, _ io.Writer) error {
		*calls = append(*calls, append([]string{name}, args...))
		switch {
		case slices.Equal(args, []string{"flake", "prefetch", "--json", treeRefFor(testRev)}):
			if failStep == "fetch" {
				return errors.New("prefetch failed")
			}
			_, err := io.WriteString(stdout, prefetchOut)
			return err
		case name == "nix" && len(args) > 0 && args[0] == "copy":
			if failStep == "copy" {
				return errors.New("copy failed")
			}
		case name == sshCommand:
			if failStep == "switch" {
				return errors.New("switch failed")
			}
		}
		return nil
	}
}

func treeRefFor(rev string) string {
	return "git+" + repoURL + "?ref=main&rev=" + rev
}

const prefetchJSON = `{"storePath":"` + testStorePath + `","hash":"sha256-x"}`

func TestSwitchDarwinRunsFetchCopySwitchInOrder(t *testing.T) {
	var calls [][]string
	var out strings.Builder
	deps := newDependencies(&out, io.Discard)
	deps.runner = darwinRunner(&calls, prefetchJSON, "")
	failed, err := switchDarwin(t.Context(), deps, deploySource{rev: testRev}, fleetHosts[0], false)
	if err != nil || failed {
		t.Fatalf("failed=%v err=%v", failed, err)
	}
	want := [][]string{
		{"nix", "flake", "prefetch", "--json", treeRefFor(testRev)},
		{"nix", "copy", "--to", "ssh-ng://darwin1.example.com", testStorePath},
		{"ssh", "-t", "darwin1", `zsh -lic 'nh-switch darwin "path:` + testStorePath + `?dir=example/nix#darwin1"'`},
	}
	if !slices.EqualFunc(calls, want, slices.Equal) {
		t.Fatalf("calls:\n%v\nwant:\n%v", calls, want)
	}
}

func TestSwitchDarwinCopyFailureSkipsSwitch(t *testing.T) {
	var calls [][]string
	var errOut strings.Builder
	deps := newDependencies(io.Discard, &errOut)
	deps.runner = darwinRunner(&calls, prefetchJSON, "copy")
	failed, err := switchDarwin(t.Context(), deps, deploySource{rev: testRev}, fleetHosts[0], false)
	if err != nil || !failed {
		t.Fatalf("failed=%v err=%v; want failed host, nil error", failed, err)
	}
	for _, call := range calls {
		if call[0] == "ssh" {
			t.Fatalf("nh-switch ran after a failed copy: %v", calls)
		}
	}
	if !strings.Contains(errOut.String(), "darwin1: copy failed") {
		t.Fatalf("stderr %q", errOut.String())
	}
}

func TestSwitchDarwinBadPrefetchStopsBeforeCopy(t *testing.T) {
	for _, out := range []string{`not json`, `{"storePath":"/tmp/evil"}`, `{"storePath":"/nix/store/x; rm -rf ~"}`} {
		var calls [][]string
		deps := newDependencies(io.Discard, io.Discard)
		deps.runner = darwinRunner(&calls, out, "")
		failed, err := switchDarwin(t.Context(), deps, deploySource{rev: testRev}, fleetHosts[0], false)
		if err != nil || !failed {
			t.Fatalf("%q: failed=%v err=%v", out, failed, err)
		}
		if len(calls) != 1 {
			t.Fatalf("%q: calls after bad prefetch: %v", out, calls)
		}
	}
}

func TestSwitchDarwinDryRunPrintsPlanWithoutRunning(t *testing.T) {
	calls := 0
	var out strings.Builder
	deps := newDependencies(&out, io.Discard)
	deps.runner = func(context.Context, string, []string, io.Writer, io.Writer) error { calls++; return nil }
	if _, err := switchDarwin(t.Context(), deps, deploySource{rev: testRev}, fleetHosts[0], true); err != nil {
		t.Fatal(err)
	}
	if calls != 0 || !strings.Contains(out.String(), "nix copy --to ssh-ng://darwin1.example.com '<store-path>'") {
		t.Fatalf("calls %d, output:\n%s", calls, out.String())
	}
}
