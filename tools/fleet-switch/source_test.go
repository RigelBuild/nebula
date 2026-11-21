package main

import (
	"context"
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const testRev = "0123456789abcdef0123456789abcdef01234567"

var localRepo = deploySource{root: "/repo"}

// lsRemoteRunner answers the single ls-remote with out and fails any other command.
func lsRemoteRunner(t *testing.T, out string, calls *int) commandRunner {
	t.Helper()
	return func(_ context.Context, name string, args []string, stdout, _ io.Writer) error {
		if name != "git" || !slices.Equal(args, []string{"ls-remote", repoURL, "refs/heads/main"}) {
			t.Fatalf("unexpected command: %s %v", name, args)
		}
		*calls++
		_, err := io.WriteString(stdout, out)
		return err
	}
}

func TestResolveSourceTrunkPinsOneRevision(t *testing.T) {
	calls := 0
	deps := newDependencies(io.Discard, io.Discard)
	deps.runner = lsRemoteRunner(t, testRev+"\trefs/heads/main\n", &calls)
	src, err := resolveSource(t.Context(), deps, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if src.rev != testRev || src.isLocal() || calls != 1 {
		t.Fatalf("source %#v, ls-remote calls %d", src, calls)
	}
	for _, fleetHost := range fleetHosts {
		if fleetHost.platform == platformWindows {
			continue // Windows hosts have no flake.
		}
		dir, attr, _ := strings.Cut(fleetHost.flake, "#")
		want := "git+" + repoURL + "?ref=main&rev=" + testRev + "&dir=" + dir + "#" + attr
		if got := src.flakeRef(fleetHost); got != want {
			t.Fatalf("%s: flake %q, want %q", fleetHost.name, got, want)
		}
	}
}

func TestTrunkRevRejectsOtherOutput(t *testing.T) {
	for _, out := range []string{
		"",
		"abc\trefs/heads/main\n",
		testRev + "\trefs/heads/other\n",
		testRev + "\trefs/heads/main\n" + testRev + "\trefs/heads/main\n",
	} {
		calls := 0
		deps := newDependencies(io.Discard, io.Discard)
		deps.runner = lsRemoteRunner(t, out, &calls)
		if rev, err := trunkRev(t.Context(), deps); err == nil {
			t.Fatalf("output %q accepted as %q", out, rev)
		}
	}
}

func TestTrunkRevRequiresRepoURL(t *testing.T) {
	saved := repoURL
	repoURL = ""
	t.Cleanup(func() { repoURL = saved })
	calls := 0
	deps := newDependencies(io.Discard, io.Discard)
	deps.runner = lsRemoteRunner(t, testRev+"\trefs/heads/main\n", &calls)
	if _, err := trunkRev(t.Context(), deps); err == nil || !strings.Contains(err.Error(), "--repo-url is required") || calls != 0 {
		t.Fatalf("error %v, ls-remote calls %d", err, calls)
	}
}

func TestResolveSourceLocalKeepsPath(t *testing.T) {
	calls := 0
	deps := newDependencies(io.Discard, io.Discard)
	deps.runner = lsRemoteRunner(t, "", &calls)
	want, err := filepath.Abs("tree")
	if err != nil {
		t.Fatal(err)
	}
	src, err := resolveSource(t.Context(), deps, "tree", true)
	if err != nil {
		t.Fatal(err)
	}
	if src.root != want || src.rev != "" || calls != 0 {
		t.Fatalf("source %#v, ls-remote calls %d", src, calls)
	}
	if got := src.flakeRef(mustHost(t, "wsl1")); got != filepath.Join(want, "example/wsl")+"#wsl1" {
		t.Fatalf("flake %q", got)
	}
	src, err = resolveSource(t.Context(), deps, "/abs/repo", true)
	if err != nil || src.root != "/abs/repo" {
		t.Fatalf("absolute source %#v: %v", src, err)
	}
}

func TestResolveSourceEmptyFlagRefused(t *testing.T) {
	calls := 0
	deps := newDependencies(io.Discard, io.Discard)
	deps.runner = lsRemoteRunner(t, "", &calls)
	if _, err := resolveSource(t.Context(), deps, "", true); err == nil || !strings.Contains(err.Error(), "--flake PATH is empty") || calls != 0 {
		t.Fatalf("error %v, ls-remote calls %d", err, calls)
	}
}

func TestCheckSourceLocalNeedsAllowLocal(t *testing.T) {
	darwin, nixos1, wsl, windows := fleetHosts[0], fleetHosts[1], mustHost(t, "wsl1"), mustHost(t, "win1")
	for _, tc := range []struct {
		name    string
		src     deploySource
		hosts   []host
		allow   bool
		wantErr string
	}{
		{name: "trunk_any_host", src: deploySource{rev: testRev}, hosts: []host{nixos1, darwin}},
		{name: "local_wsl1", src: localRepo, hosts: []host{wsl}},
		{name: "local_other_refused", src: localRepo, hosts: []host{nixos1}, wantErr: "--allow-local"},
		{name: "local_all_refused", src: localRepo, hosts: []host{darwin, nixos1, wsl}, wantErr: "--allow-local"},
		{name: "local_windows_needs_allow_local", src: localRepo, hosts: []host{windows}, wantErr: "--allow-local"},
		{name: "local_other_allowed", src: localRepo, hosts: []host{nixos1}, allow: true},
		{name: "local_wsl1_and_other_allowed", src: localRepo, hosts: []host{wsl, nixos1}, allow: true},
		{name: "local_darwin_refused_even_allowed", src: localRepo, hosts: []host{darwin}, allow: true, wantErr: "darwin1 cannot deploy a local checkout"},
		{name: "local_windows_refused_even_allowed", src: localRepo, hosts: []host{windows}, allow: true, wantErr: "win1 cannot deploy a local checkout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkSource(tc.hosts, tc.src, tc.allow)
			if tc.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("error %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestSwitchAllLocalRefusedBeforeTouchingHosts(t *testing.T) {
	calls := 0
	var out strings.Builder
	deps := newDependencies(&out, io.Discard)
	deps.runner = func(context.Context, string, []string, io.Writer, io.Writer) error { calls++; return nil }
	err := newRootCommand(t.Context(), []string{"switch", "--all", "--flake", ".", "--dry-run", "--hosts", exampleInventory}, deps).Execute()
	if err == nil || !strings.Contains(err.Error(), "--allow-local") || calls != 0 || strings.Contains(out.String(), "==>") {
		t.Fatalf("error %v, calls %d, output %q", err, calls, out.String())
	}
}

func TestSwitchAllTrunkPinsOneRevision(t *testing.T) {
	calls := 0
	var out strings.Builder
	deps := newDependencies(&out, io.Discard)
	deps.runner = lsRemoteRunner(t, testRev+"\trefs/heads/main\n", &calls)
	if err := newRootCommand(t.Context(), []string{"switch", "--all", "--dry-run", "--hosts", exampleInventory, "--repo-url", testRepoURL}, deps).Execute(); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if calls != 1 || !strings.Contains(out.String(), "Deploying "+repoURL+" main at "+testRev) {
		t.Fatalf("ls-remote calls %d\n%s", calls, out.String())
	}
	src := deploySource{rev: testRev}
	for _, fleetHost := range fleetHosts {
		if fleetHost.explicitOnly {
			continue // not in --all
		}
		// The darwin dry run prints the pinned prefetch, not the flake ref.
		want := src.flakeRef(fleetHost)
		if fleetHost.platform == platformDarwin {
			want = formatCommand(prefetchCommand(src.treeRef()))
		}
		if !strings.Contains(out.String(), want) {
			t.Fatalf("%s not pinned to %s, want %q:\n%s", fleetHost.name, testRev, want, out.String())
		}
	}
	// Word-bounded: darwin1 is not win1; the "==> win1 " header and "(skipped)" line both match.
	win1Mention := regexp.MustCompile(`\bwin1\b`)
	if strings.Contains(out.String(), "==> win1 ") || win1Mention.MatchString(out.String()) {
		t.Fatalf("--all dry run includes win1:\n%s", out.String())
	}
}

func TestPrintSourceLocalWarnsOfDrift(t *testing.T) {
	var out strings.Builder
	if err := printSource(&out, localRepo); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "warning:") != 1 || !strings.Contains(out.String(), "drift from main") {
		t.Fatalf("local output %q", out.String())
	}
	out.Reset()
	if err := printSource(&out, deploySource{rev: testRev}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "warning:") {
		t.Fatalf("trunk output %q warns", out.String())
	}
}
