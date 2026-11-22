package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RigelBuild/nebula/tools/rjc/internal/hookshell"
)

const fakeNodeHK = `#!/bin/sh
if [ -n "$RJC_TEST_NODE_PATH" ]; then command -v node > "$RJC_TEST_NODE_PATH"; fi
` + fakeHK

func TestSSHHookRunnerBuildsRunnerWhenHostLacksIt(t *testing.T) {
	skipUnlessLinux(t)
	for _, tc := range []struct{ name, config, body, tool string }{
		{name: "lefthook", config: "lefthook.yml", body: "committed\n", tool: "lefthook"},
		{name: "pre-commit", config: ".pre-commit-config.yaml", body: "repos: []\n", tool: "pre-commit"},
		{name: "prek", config: "prek.toml", body: "repos = []\n", tool: "prek"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, runner := sshGateFixture(t)
			if err := os.Remove(filepath.Join(os.Getenv("HOME"), "bin", "hk")); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(f.repo.root, "devenv.nix")); err != nil {
				t.Fatal(err)
			}
			path := hostPathWithout(t, tc.tool)
			f.repo.env = append(f.repo.env, "PATH="+path)
			t.Setenv("RJC_TEST_FAKE_HK", filepath.Join(t.TempDir(), tc.tool+"-source"))
			writeFakeTool(t, os.Getenv("RJC_TEST_FAKE_HK"), fakeNodeHK)
			store := t.TempDir()
			t.Setenv("RJC_TEST_NIX_STORE", store)
			nodePathFile := filepath.Join(t.TempDir(), "node-path")
			wantsNode := tc.tool == "pre-commit" || tc.tool == "prek"
			if wantsNode {
				t.Setenv("RJC_TEST_NODE_PATH", nodePathFile)
			}
			f.commitFile(t, tc.config, tc.body)
			base, head := sshRange(t, f)
			var output bytes.Buffer
			out, _, err := runner.Run(f.ctx, base, head, nil, &output)
			if err != nil || out.Phase != phaseCompleted || out.Exit != 0 {
				t.Fatalf("fallback: %+v err=%v output=%s", out, err, output.String())
			}
			log, err := os.ReadFile(os.Getenv("RJC_TEST_NIX_LOG"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(log), "build --no-link --print-out-paths "+hookshell.Nixpkgs+"#"+tc.tool) {
				t.Fatalf("host without %s must nix build its fallback, got nix log %q", tc.tool, log)
			}
			gotNodeBuild := strings.Contains(string(log), "build --no-link --print-out-paths "+hookshell.Nixpkgs+"#nodejs_24")
			if gotNodeBuild != wantsNode {
				t.Fatalf("nodejs_24 build = %t, want %t", gotNodeBuild, wantsNode)
			}
			if wantsNode {
				got, err := os.ReadFile(nodePathFile)
				if err != nil {
					t.Fatalf("hook did not record node path: %v", err)
				}
				want := filepath.Join(store, "nodejs_24", "bin", "node") + "\n"
				if string(got) != want {
					t.Fatalf("hook node path = %q, want %q", got, want)
				}
			}
		})
	}
}

func TestSSHHookRunnerUsesPinnedNodeOnlyWhenNodeOrNpmMissing(t *testing.T) {
	skipUnlessLinux(t)
	for _, tc := range []struct {
		name      string
		managed   bool
		node      bool
		npm       bool
		wantBuild bool
	}{
		{name: "node without npm", node: true, wantBuild: true},
		{name: "npm without node", npm: true, wantBuild: true},
		{name: "node and npm", node: true, npm: true},
		{name: "devenv tree without node", managed: true, wantBuild: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, runner := sshGateFixture(t)
			if err := os.Remove(filepath.Join(os.Getenv("HOME"), "bin", "hk")); err != nil {
				t.Fatal(err)
			}
			if !tc.managed {
				if err := os.Remove(filepath.Join(f.repo.root, "devenv.nix")); err != nil {
					t.Fatal(err)
				}
			}
			path := hostPathWithout(t, "pre-commit")
			tools := t.TempDir()
			if tc.node {
				writeFakeTool(t, filepath.Join(tools, "node"), "#!/bin/sh\nexit 0\n")
			}
			if tc.npm {
				writeFakeTool(t, filepath.Join(tools, "npm"), "#!/bin/sh\nexit 0\n")
			}
			path = tools + string(os.PathListSeparator) + path
			f.repo.env = append(f.repo.env, "PATH="+path)
			t.Setenv("RJC_TEST_FAKE_HK", filepath.Join(t.TempDir(), "pre-commit-source"))
			writeFakeTool(t, filepath.Join(os.Getenv("HOME"), "bin", "pre-commit"), fakeNodeHK)
			writeFakeTool(t, os.Getenv("RJC_TEST_FAKE_HK"), fakeNodeHK)
			t.Setenv("RJC_TEST_NIX_LOG", filepath.Join(t.TempDir(), "nix-log"))
			store := t.TempDir()
			t.Setenv("RJC_TEST_NIX_STORE", store)
			nodePathFile := filepath.Join(t.TempDir(), "node-path")
			t.Setenv("RJC_TEST_NODE_PATH", nodePathFile)
			f.commitFile(t, ".pre-commit-config.yaml", "repos: []\n")
			base, head := sshRange(t, f)
			var output bytes.Buffer
			out, _, err := runner.Run(f.ctx, base, head, nil, &output)
			if err != nil || out.Phase != phaseCompleted || out.Exit != 0 {
				t.Fatalf("pre-commit run: %+v err=%v output=%s", out, err, output.String())
			}
			log, err := os.ReadFile(os.Getenv("RJC_TEST_NIX_LOG"))
			if err != nil {
				t.Fatal(err)
			}
			gotBuild := strings.Contains(string(log), "build --no-link --print-out-paths "+hookshell.Nixpkgs+"#nodejs_24")
			if gotBuild != tc.wantBuild {
				t.Fatalf("nodejs_24 build = %t, want %t; nix log: %q", gotBuild, tc.wantBuild, log)
			}
			if !tc.node {
				got, err := os.ReadFile(nodePathFile)
				if err != nil {
					t.Fatalf("hook did not record node path: %v", err)
				}
				if want := filepath.Join(store, "nodejs_24", "bin", "node") + "\n"; string(got) != want {
					t.Fatalf("hook node path = %q, want %q", got, want)
				}
			}
		})
	}
}

func TestSSHHookRunnerContinuesWhenPinnedNodeBuildFails(t *testing.T) {
	skipUnlessLinux(t)
	f, runner := sshGateFixture(t)
	if err := os.Remove(filepath.Join(os.Getenv("HOME"), "bin", "hk")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.repo.root, "devenv.nix")); err != nil {
		t.Fatal(err)
	}
	path := hostPathWithout(t, "pre-commit")
	f.repo.env = append(f.repo.env, "PATH="+path)
	t.Setenv("RJC_TEST_FAKE_HK", filepath.Join(t.TempDir(), "pre-commit-source"))
	writeFakeTool(t, os.Getenv("RJC_TEST_FAKE_HK"), fakeNodeHK)
	writeFakeTool(t, filepath.Join(os.Getenv("HOME"), "bin", "pre-commit"), fakeNodeHK)
	nodePathFile := filepath.Join(t.TempDir(), "node-path")
	t.Setenv("RJC_TEST_NODE_PATH", nodePathFile)
	t.Setenv("RJC_TEST_NIX_BUILD_FAIL_PACKAGE", "nodejs_24")
	t.Setenv("RJC_TEST_NIX_STORE", t.TempDir())
	f.commitFile(t, ".pre-commit-config.yaml", "repos: []\n")
	base, head := sshRange(t, f)
	var output bytes.Buffer
	out, _, err := runner.Run(f.ctx, base, head, nil, &output)
	if err != nil || out.Phase != phaseCompleted || out.Exit != 0 {
		t.Fatalf("node build failure must not fail completed hook run: %+v err=%v", out, err)
	}
	if !strings.Contains(output.String(), "rjc: pinned nodejs_24 build failed; pre-commit may fetch node via nodeenv") {
		t.Fatalf("node build failure warning missing from output %q", output.String())
	}
	log, err := os.ReadFile(os.Getenv("RJC_TEST_NIX_LOG"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "fake nix build failed for nodejs_24") {
		t.Fatalf("nodejs_24 build must fail from the injected package failure, got nix log %q", log)
	}
	gotNodePath, err := os.ReadFile(nodePathFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotNodePath) != 0 {
		t.Fatalf("failed node build changed hook PATH; command -v node = %q", gotNodePath)
	}
}
