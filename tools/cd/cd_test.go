// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func testGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitRun(context.Background(), dir, []string{
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
	}, nil, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commitAll(t *testing.T, dir, msg string) string {
	t.Helper()
	testGit(t, dir, "add", "-A")
	testGit(t, dir, "commit", "-q", "-m", msg)
	return testGit(t, dir, "rev-parse", "HEAD")
}

const toolCUE = `id: "%s"
release: {
	base: "1.2.0"
	artifacts: {
		"%s": {kind: "binary", package: "local:%s", binaries: ["%s"]}
	}
}
images: {}
`

// newRepo makes a repo with binary tools alpha and beta, library tool lib and fork vine.
func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	testGit(t, dir, "init", "-q", "-b", "main")
	for _, name := range []string{"alpha", "beta"} {
		writeFile(t, dir, "tools/"+name+"/moon.yml", "layer: tool\n")
		writeFile(t, dir, "tools/"+name+"/component.cue", sprintfCUE(name))
		writeFile(t, dir, "tools/"+name+"/main.go", "package main\n")
	}
	writeFile(t, dir, "tools/lib/moon.yml", "layer: tool\n")
	writeFile(t, dir, "tools/lib/component.cue", `release: {base: "0.1.0", artifacts: {lib: {kind: "library"}}}`+"\n")
	writeFile(t, dir, "forks/vine/moon.yml", "layer: application\n")
	writeFile(t, dir, "forks/vine/src.rs", "fn main() {}\n")
	writeFile(t, dir, "README.md", "nebula\n")
	commitAll(t, dir, "init")
	return dir
}

func sprintfCUE(name string) string {
	return strings.ReplaceAll(toolCUE, "%s", name)
}

// release writes a manifest commit and tags it as a previous Release.
func release(t *testing.T, dir, tag, source string, held []string) {
	t.Helper()
	m := Manifest{Source: source, Tag: tag, Held: held, Packages: map[string]Package{
		"alpha": {Version: "1.0.0", Systems: map[string]Asset{"x86_64-linux": {URL: "u/alpha", SHA256: "aa"}}},
		"beta":  {Version: "1.0.0", Systems: map[string]Asset{"x86_64-linux": {URL: "u/beta", SHA256: "bb"}}},
	}}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, manifestFile, string(data))
	commitAll(t, dir, "release: "+tag)
	testGit(t, dir, "tag", tag)
}

func TestPlanFirstRunPlansEveryProject(t *testing.T) {
	dir := newRepo(t)
	p, err := makePlan(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"forks/vine", "tools/alpha", "tools/beta", "tools/lib"}
	if !reflect.DeepEqual(p.Projects, want) {
		t.Fatalf("projects = %v, want %v", p.Projects, want)
	}
	if !reflect.DeepEqual(p.Build, []string{"alpha", "beta"}) {
		t.Fatalf("build = %v, want [alpha beta]", p.Build)
	}
	if p.PreviousTag != "" {
		t.Fatalf("previous tag = %q, want none", p.PreviousTag)
	}
}

func TestPlanDiffsNewestTagSourceAndAddsHeld(t *testing.T) {
	dir := newRepo(t)
	first := testGit(t, dir, "rev-parse", "HEAD")
	release(t, dir, "nebula-20261101-9", first, nil)
	// An older-sorting tag with a different source must be ignored.
	writeFile(t, dir, "tools/lib/x.go", "package lib\n")
	second := commitAll(t, dir, "lib change")
	release(t, dir, "nebula-20261102-10", second, []string{"beta"})
	writeFile(t, dir, "forks/vine/src.rs", "fn main() { }\n")
	writeFile(t, dir, "README.md", "changed\n")
	commitAll(t, dir, "vine change")

	p, err := makePlan(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.PreviousTag != "nebula-20261102-10" {
		t.Fatalf("previous tag = %q, want nebula-20261102-10", p.PreviousTag)
	}
	want := []string{"forks/vine", "tools/beta"}
	if !reflect.DeepEqual(p.Projects, want) {
		t.Fatalf("projects = %v, want %v (diff from newest source plus held)", p.Projects, want)
	}
	if !reflect.DeepEqual(p.Build, []string{"beta"}) {
		t.Fatalf("build = %v, want [beta]", p.Build)
	}
}

func TestPlanRefusesWhenHeadDoesNotDescendFromSource(t *testing.T) {
	dir := newRepo(t)
	testGit(t, dir, "checkout", "-q", "-b", "side")
	writeFile(t, dir, "tools/alpha/side.go", "package main\n")
	side := commitAll(t, dir, "side")
	release(t, dir, "nebula-20261101-1", side, nil)
	testGit(t, dir, "checkout", "-q", "main")
	_, err := makePlan(context.Background(), dir)
	if !errors.Is(err, errNotDescendant) {
		t.Fatalf("err = %v, want errNotDescendant", err)
	}
}

func makeAssets(t *testing.T, systems []string, pkgs ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, pkg := range pkgs {
		for _, s := range systems {
			writeFile(t, dir, pkg+"-1.2.0-"+s+".tar.zst", pkg+s)
		}
	}
	return dir
}

func TestManifestSchemaShape(t *testing.T) {
	systems := []string{"x86_64-linux", "aarch64-darwin"}
	assets := makeAssets(t, systems, "alpha")
	writeFile(t, assets, "beta-1.0.0-x86_64-linux.tar.zst", "old beta")
	betaSum, err := fileSHA256(filepath.Join(assets, "beta-1.0.0-x86_64-linux.tar.zst"))
	if err != nil {
		t.Fatal(err)
	}
	prev, err := json.Marshal(Manifest{Source: "old", Tag: "nebula-20261101-1", Packages: map[string]Package{
		"beta": {Version: "1.0.0", UpstreamRev: "abc", Systems: map[string]Asset{"x86_64-linux": {
			URL: "https://x/nebula-20261101-1/beta-1.0.0-x86_64-linux.tar.zst", SHA256: betaSum}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	p := Plan{
		Source: "deadbeef", Previous: prev, Build: []string{"alpha", "beta"},
		Binaries: []Binary{{Package: "alpha", Version: "1.2.0"}, {Package: "beta", Version: "1.2.0"}},
	}
	planPath := filepath.Join(t.TempDir(), "plan.json")
	data, _ := json.Marshal(p)
	writeFile(t, filepath.Dir(planPath), "plan.json", string(data))
	in := &manifestInput{
		planPath: planPath, tag: "nebula-20261117-42", assets: assets,
		baseURL: "https://github.com/RigelBuild/nebula/releases/download/", held: []string{"beta"}, systems: systems,
	}
	out, err := in.build()
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["source"] != "deadbeef" || got["tag"] != "nebula-20261117-42" {
		t.Fatalf("source/tag wrong: %v", got)
	}
	if !reflect.DeepEqual(got["held"], []any{"beta"}) {
		t.Fatalf("held = %v, want [beta]", got["held"])
	}
	alpha, ok := got["alpha"].(map[string]any)
	if !ok || alpha["version"] != "1.2.0" {
		t.Fatalf("alpha = %v", got["alpha"])
	}
	asset, ok := alpha["aarch64-darwin"].(map[string]any)
	if !ok {
		t.Fatalf("alpha has no aarch64-darwin object: %v", alpha)
	}
	wantURL := "https://github.com/RigelBuild/nebula/releases/download/nebula-20261117-42/alpha-1.2.0-aarch64-darwin.tar.zst"
	if asset["url"] != wantURL || len(asset["sha256"].(string)) != 64 || len(asset) != 2 {
		t.Fatalf("asset = %v, want url %s and a sha256 hex", asset, wantURL)
	}
	beta := got["beta"].(map[string]any)
	if beta["version"] != "1.0.0" || beta["upstreamRev"] != "abc" {
		t.Fatalf("held beta must keep its previous entry, got %v", beta)
	}

	in.held = nil
	if _, err := in.build(); !errors.Is(err, errMissingAsset) {
		t.Fatalf("a built package with a missing asset: err = %v, want errMissingAsset", err)
	}
}

func TestGuardRefusesASecondChangedFile(t *testing.T) {
	dir := newRepo(t)
	writeFile(t, dir, manifestFile, "{}\n")
	ok := commitAll(t, dir, "manifest only")
	if err := guard(context.Background(), dir, ok); err != nil {
		t.Fatalf("manifest-only commit refused: %v", err)
	}
	writeFile(t, dir, manifestFile, "{\"a\":1}\n")
	writeFile(t, dir, "tools/alpha/main.go", "package main // x\n")
	bad := commitAll(t, dir, "manifest plus code")
	if err := guard(context.Background(), dir, bad); !errors.Is(err, errGuard) {
		t.Fatalf("err = %v, want errGuard", err)
	}
	writeFile(t, dir, "README.md", "only code\n")
	code := commitAll(t, dir, "code only")
	if err := guard(context.Background(), dir, code); !errors.Is(err, errGuard) {
		t.Fatalf("code-only commit: err = %v, want errGuard", err)
	}
}

func TestPublishRetriesAfterMainMoves(t *testing.T) {
	work := newRepo(t)
	source := testGit(t, work, "rev-parse", "HEAD")
	remote := t.TempDir()
	testGit(t, remote, "init", "-q", "--bare", "-b", "main")
	testGit(t, work, "remote", "add", "origin", remote)
	testGit(t, work, "push", "-q", "origin", "main")

	other := t.TempDir()
	testGit(t, other, "clone", "-q", remote, ".")
	systems := []string{"x86_64-linux"}
	data, _ := json.Marshal(Plan{Source: source, Build: []string{"alpha"}, Binaries: []Binary{{Package: "alpha", Version: "1.2.0"}}})
	planDir := t.TempDir()
	writeFile(t, planDir, "plan.json", string(data))

	var merged string
	commit, err := publish(context.Background(), publishOptions{
		repo: work, remote: "origin", branch: "main", attempts: 3,
		manifest: &manifestInput{planPath: filepath.Join(planDir, "plan.json"), tag: "nebula-20261117-7",
			assets: makeAssets(t, systems, "alpha"), baseURL: "https://x/", systems: systems},
		beforePush: func(attempt int) error {
			if attempt == 1 { // A merge lands on main between commit and push.
				writeFile(t, other, "tools/beta/new.go", "package main\n")
				merged = commitAll(t, other, "merge M1")
				testGit(t, other, "push", "-q", "origin", "main")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	tip := testGit(t, remote, "rev-parse", "main")
	if tip != commit {
		t.Fatalf("remote main = %s, want release commit %s", tip, commit)
	}
	if parent := testGit(t, remote, "rev-parse", "main^"); parent != merged {
		t.Fatalf("release parent = %s, want the moved tip %s", parent, merged)
	}
	if err := guard(context.Background(), work, commit); err != nil {
		t.Fatal(err)
	}
	var m Manifest
	if err := json.Unmarshal([]byte(testGit(t, remote, "show", "main:"+manifestFile)), &m); err != nil {
		t.Fatal(err)
	}
	if m.Source != source {
		t.Fatalf("source = %s, want the tested commit %s", m.Source, source)
	}
	if msg := testGit(t, remote, "log", "-1", "--format=%s", "main"); msg != "release: nebula-20261117-7 [skip ci]" {
		t.Fatalf("message = %q", msg)
	}
}

func TestCheckAsset(t *testing.T) {
	src := t.TempDir()
	writeFile(t, src, "go.mod", "module x\n\ngo 1.27\n")
	writeFile(t, src, "main.go", "package main\n\nfunc main() {}\n")
	build := func(goos, goarch string) string {
		out := filepath.Join(src, goos+"-"+goarch)
		cmd := exec.Command("go", "build", "-o", out, ".")
		cmd.Dir = src
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch, "GOFLAGS=")
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s/%s: %v\n%s", goos, goarch, err, b)
		}
		return out
	}
	linux := build("linux", "arm64")
	if err := checkAsset(linux, "aarch64-linux"); err != nil {
		t.Fatalf("static linux binary refused: %v", err)
	}
	if err := checkAsset(linux, "x86_64-linux"); !errors.Is(err, errAssetCheck) {
		t.Fatalf("wrong-arch linux binary: err = %v", err)
	}
	darwin := build("darwin", "arm64")
	if err := checkAsset(darwin, "aarch64-darwin"); err != nil {
		t.Fatalf("darwin binary linking libSystem refused: %v", err)
	}
	if runtime.GOOS == "linux" {
		gitPath, err := exec.LookPath("git")
		if err != nil {
			t.Fatal(err)
		}
		system := map[string]string{"amd64": "x86_64-linux", "arm64": "aarch64-linux"}[runtime.GOARCH]
		if err := checkAsset(gitPath, system); !errors.Is(err, errAssetCheck) {
			t.Fatalf("dynamic git binary accepted: err = %v", err)
		}
	}
}
