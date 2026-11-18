// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"context"
	"debug/macho"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// publishFixture is a work clone, a bare remote and a plan for source.
type publishFixture struct {
	work, remote, plan, assets string
	source                     string
}

func newPublishFixture(t *testing.T) publishFixture {
	t.Helper()
	work := newRepo(t)
	remote := t.TempDir()
	testGit(t, remote, "init", "-q", "--bare", "-b", "main")
	testGit(t, work, "remote", "add", "origin", remote)
	testGit(t, work, "push", "-q", "origin", "main")
	f := publishFixture{work: work, remote: remote, source: testGit(t, work, "rev-parse", "HEAD")}
	f.assets = makeAssets(t, []string{"x86_64-linux"}, "alpha")
	f.plan = writePlan(t, Plan{Source: f.source, Build: []string{"alpha"}, Binaries: []Binary{{Package: "alpha", Version: "1.2.0"}}})
	return f
}

func writePlan(t *testing.T, p Plan) string {
	t.Helper()
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeFile(t, dir, "plan.json", string(data))
	return filepath.Join(dir, "plan.json")
}

func (f publishFixture) options(tag string) publishOptions {
	return publishOptions{
		repo: f.work, remote: "origin", branch: "main", attempts: 3,
		manifest: &manifestInput{planPath: f.plan, tag: tag, assets: f.assets, baseURL: "https://x/", systems: []string{"x86_64-linux"}},
	}
}

func TestPublishGuardsTheCommitBeforePushing(t *testing.T) {
	f := newPublishFixture(t)
	o := f.options("nebula-20261117-1")
	// A commit builder that also changes README.md: publish must refuse to push it.
	o.commit = func(ctx context.Context, repo, parent string, data []byte, tag string) (string, error) {
		testGit(t, repo, "checkout", "-q", parent)
		writeFile(t, repo, manifestFile, string(data))
		writeFile(t, repo, "README.md", "smuggled\n")
		return commitAll(t, repo, "release"), nil
	}
	before := testGit(t, f.remote, "rev-parse", "main")
	if _, err := publish(context.Background(), o); !errors.Is(err, errGuard) {
		t.Fatalf("err = %v, want errGuard", err)
	}
	if after := testGit(t, f.remote, "rev-parse", "main"); after != before {
		t.Fatalf("remote main moved to %s; a guarded commit was pushed", after)
	}
}

func TestGuardRefusesAMergeCommit(t *testing.T) {
	dir := newRepo(t)
	base := testGit(t, dir, "rev-parse", "HEAD")
	testGit(t, dir, "checkout", "-q", "-b", "side")
	writeFile(t, dir, "tools/alpha/side.go", "package main\n")
	commitAll(t, dir, "side")
	side := testGit(t, dir, "rev-parse", "HEAD")
	testGit(t, dir, "checkout", "-q", base)
	writeFile(t, dir, manifestFile, "{}\n")
	manifest := commitAll(t, dir, "manifest")
	// A merge whose first-parent diff is only the manifest: same tree as the
	// manifest commit, with side as a second parent that reaches main.
	merge := testGit(t, dir, "commit-tree", manifest+"^{tree}", "-p", base, "-p", side, "-m", "merge")
	if err := guard(context.Background(), dir, merge); !errors.Is(err, errGuard) {
		t.Fatalf("merge commit: err = %v, want errGuard", err)
	}
}

func TestPublishRefusesSourceNotOnMain(t *testing.T) {
	f := newPublishFixture(t)
	testGit(t, f.work, "checkout", "-q", "-b", "side")
	writeFile(t, f.work, "tools/alpha/side.go", "package main\n")
	side := commitAll(t, f.work, "side, never merged")
	f.plan = writePlan(t, Plan{Source: side, Build: []string{"alpha"}, Binaries: []Binary{{Package: "alpha", Version: "1.2.0"}}})
	if _, err := publish(context.Background(), f.options("nebula-20261117-2")); !errors.Is(err, errNotDescendant) {
		t.Fatalf("err = %v, want errNotDescendant", err)
	}
}

func TestPublishRefusesAnOlderRunAfterANewerRelease(t *testing.T) {
	f := newPublishFixture(t)
	// A newer run released source S2, a descendant of this run's source.
	writeFile(t, f.work, "tools/beta/new.go", "package main\n")
	newer := commitAll(t, f.work, "merge M1")
	release(t, f.work, "nebula-20261118-5", newer, nil)
	testGit(t, f.work, "push", "-q", "origin", "main", "--tags")
	testGit(t, f.work, "tag", "-d", "nebula-20261118-5") // publish must fetch it.
	before := testGit(t, f.remote, "rev-parse", "main")
	if _, err := publish(context.Background(), f.options("nebula-20261119-4")); !errors.Is(err, errStaleRelease) {
		t.Fatalf("err = %v, want errStaleRelease", err)
	}
	if after := testGit(t, f.remote, "rev-parse", "main"); after != before {
		t.Fatal("a stale run moved main")
	}
}

func TestPublishFailsFastOnARulesetDenial(t *testing.T) {
	f := newPublishFixture(t)
	hook := filepath.Join(f.remote, "hooks", "pre-receive")
	writeFile(t, filepath.Dir(hook), "pre-receive", "#!/bin/sh\necho denied by ruleset >&2\nexit 1\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
	o := f.options("nebula-20261117-3")
	attempts := 0
	o.beforePush = func(int) error { attempts++; return nil }
	if _, err := publish(context.Background(), o); err == nil || errors.Is(err, errPushRejected) {
		t.Fatalf("err = %v, want the denial itself", err)
	}
	if attempts != 1 {
		t.Fatalf("pushed %d times, want 1: a denial is not retried", attempts)
	}
}

func TestPlanPicksRun100OverRun99OnTheSameDay(t *testing.T) {
	dir := newRepo(t)
	first := testGit(t, dir, "rev-parse", "HEAD")
	release(t, dir, "nebula-20261117-99", first, nil)
	writeFile(t, dir, "tools/alpha/x.go", "package main\n")
	second := commitAll(t, dir, "alpha change")
	release(t, dir, "nebula-20261117-100", second, nil)
	p, err := makePlan(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.PreviousTag != "nebula-20261117-100" {
		t.Fatalf("previous tag = %q, want nebula-20261117-100", p.PreviousTag)
	}
	if len(p.Projects) != 0 {
		t.Fatalf("projects = %v, want none since run 100", p.Projects)
	}
}

func TestPlanReleasesForkBinariesWithUpstreamRev(t *testing.T) {
	dir := newRepo(t)
	writeFile(t, dir, "forks/vine/component.cue", strings.ReplaceAll(toolCUE, "%s", "vine"))
	writeFile(t, dir, "forks/vine/go.mod", "module vine\n")
	writeFile(t, dir, "forks/vine/upstream.json", `{"repo":"r","branch":"main","commit":"0c03418"}`)
	commitAll(t, dir, "vine component")
	p, err := makePlan(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	want := Binary{Package: "vine", Project: "forks/vine", Version: "1.2.0", Kind: kindGo, UpstreamRev: "0c03418", Binaries: []string{"vine"}}
	if !reflect.DeepEqual(p.Binaries[2], want) {
		t.Fatalf("binaries = %+v, want fork entry %+v", p.Binaries, want)
	}
	m, err := buildManifest(p, "nebula-20261117-1", makeAssets(t, []string{"x86_64-linux"}, "alpha", "beta", "vine"), "",
		"https://x", []string{"x86_64-linux"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.Packages["vine"].UpstreamRev != "0c03418" {
		t.Fatalf("vine upstreamRev = %q", m.Packages["vine"].UpstreamRev)
	}
}

// previousRelease is a manifest whose beta asset lives under an older tag.
func previousRelease(t *testing.T, assets string) json.RawMessage {
	t.Helper()
	sum, err := fileSHA256(filepath.Join(assets, "beta-1.2.0-x86_64-linux.tar.zst"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(Manifest{Source: "old", Tag: "nebula-20261101-1", Packages: map[string]Package{
		"beta": {Version: "1.2.0", Systems: map[string]Asset{"x86_64-linux": {
			URL: "https://x/nebula-20261101-1/beta-1.2.0-x86_64-linux.tar.zst", SHA256: sum}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestManifestCarriesUnbuiltPackagesIntoThisRelease(t *testing.T) {
	systems := []string{"x86_64-linux"}
	assets := makeAssets(t, systems, "alpha", "beta")
	p := Plan{Source: "s", Previous: previousRelease(t, assets), Build: []string{"alpha"},
		Binaries: []Binary{{Package: "alpha", Version: "1.2.0"}, {Package: "beta", Version: "1.2.0"}}}
	m, err := buildManifest(p, "nebula-20261117-2", assets, assets, "https://x", systems, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := m.Packages["beta"].Systems["x86_64-linux"].URL
	if got != "https://x/nebula-20261117-2/beta-1.2.0-x86_64-linux.tar.zst" {
		t.Fatalf("carried beta url = %q, want it under this tag", got)
	}

	// A copied asset whose bytes differ from the previous Release is refused.
	writeFile(t, assets, "beta-1.2.0-x86_64-linux.tar.zst", "tampered")
	if _, err := buildManifest(p, "nebula-20261117-2", assets, assets, "https://x", systems, nil); !errors.Is(err, errAssetMismatch) {
		t.Fatalf("err = %v, want errAssetMismatch", err)
	}

	// An unbuilt package with no previous entry is an error, not a silent drop.
	p.Previous = nil
	_, err = buildManifest(p, "nebula-20261117-2", assets, assets, "https://x", systems, nil)
	if err == nil || !strings.Contains(err.Error(), "no previous Release entry") {
		t.Fatalf("err = %v, want the no-previous-entry error", err)
	}
}

func TestReleaseFilesWritesSumsAndSkipsHeldAssets(t *testing.T) {
	systems := []string{"x86_64-linux"}
	assets := makeAssets(t, systems, "alpha", "beta")
	p := Plan{Source: "s", Build: []string{"alpha", "beta"},
		Binaries: []Binary{{Package: "alpha", Version: "1.2.0"}, {Package: "beta", Version: "1.2.0"}}}
	m, err := buildManifest(p, "nebula-20261117-3", assets, "", "https://x", systems, []string{"beta"})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(m)
	dir := t.TempDir()
	writeFile(t, dir, manifestFile, string(data))
	files, err := releaseFiles(filepath.Join(dir, manifestFile), assets, filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{filepath.Join(assets, "alpha-1.2.0-x86_64-linux.tar.zst")}; !reflect.DeepEqual(files, want) {
		t.Fatalf("files = %v, want %v (held beta not uploaded)", files, want)
	}
	sums, _ := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	want := m.Packages["alpha"].Systems["x86_64-linux"].SHA256 + "  alpha-1.2.0-x86_64-linux.tar.zst\n"
	if string(sums) != want {
		t.Fatalf("SHA256SUMS = %q, want %q", sums, want)
	}
}

// writeMachO writes a minimal 64-bit Mach-O executable for cpu linking libs.
func writeMachO(t *testing.T, cpu macho.Cpu, libs ...string) string {
	t.Helper()
	var cmds []byte
	for _, lib := range libs {
		name := append([]byte(lib), 0)
		size := 24 + len(name)
		size = (size + 7) &^ 7
		cmd := make([]byte, size)
		binary.LittleEndian.PutUint32(cmd[0:], uint32(macho.LoadCmdDylib))
		binary.LittleEndian.PutUint32(cmd[4:], uint32(size))
		binary.LittleEndian.PutUint32(cmd[8:], 24)
		copy(cmd[24:], name)
		cmds = append(cmds, cmd...)
	}
	header := make([]byte, 32)
	binary.LittleEndian.PutUint32(header[0:], macho.Magic64)
	binary.LittleEndian.PutUint32(header[4:], uint32(cpu))
	binary.LittleEndian.PutUint32(header[12:], uint32(macho.TypeExec))
	binary.LittleEndian.PutUint32(header[16:], uint32(len(libs)))
	binary.LittleEndian.PutUint32(header[20:], uint32(len(cmds)))
	path := filepath.Join(t.TempDir(), "bin")
	if err := os.WriteFile(path, append(header, cmds...), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCheckAssetMachO(t *testing.T) {
	ok := writeMachO(t, macho.CpuArm64, "/usr/lib/libSystem.B.dylib", "/System/Library/Frameworks/Security.framework/Security")
	if err := checkAsset(ok, "aarch64-darwin"); err != nil {
		t.Fatalf("system-only links refused: %v", err)
	}
	nix := writeMachO(t, macho.CpuArm64, "/usr/lib/libSystem.B.dylib", "/nix/store/abc-libiconv/lib/libiconv.dylib")
	if err := checkAsset(nix, "aarch64-darwin"); !errors.Is(err, errAssetCheck) {
		t.Fatalf("/nix/store dylib: err = %v, want errAssetCheck", err)
	}
	amd := writeMachO(t, macho.CpuAmd64, "/usr/lib/libSystem.B.dylib")
	if err := checkAsset(amd, "aarch64-darwin"); !errors.Is(err, errAssetCheck) {
		t.Fatalf("x86_64 Mach-O for aarch64-darwin: err = %v, want errAssetCheck", err)
	}
}

// TestCheckAssetELFInterp covers a dynamic ELF with an interpreter but no DT_NEEDED.
func TestCheckAssetELFInterp(t *testing.T) {
	// ELF64 header plus one PT_INTERP program header, no dynamic section.
	const ehsize, phsize = 64, 56
	b := make([]byte, ehsize+phsize+16)
	copy(b, "\x7fELF")
	b[4], b[5], b[6] = 2, 1, 1 // 64-bit, little-endian, version 1
	le := binary.LittleEndian
	le.PutUint16(b[16:], 2)      // ET_EXEC
	le.PutUint16(b[18:], 62)     // EM_X86_64
	le.PutUint32(b[20:], 1)      // version
	le.PutUint64(b[32:], ehsize) // phoff
	le.PutUint16(b[52:], ehsize)
	le.PutUint16(b[54:], phsize)
	le.PutUint16(b[56:], 1) // phnum
	ph := b[ehsize:]
	le.PutUint32(ph[0:], 3) // PT_INTERP
	le.PutUint64(ph[8:], ehsize+phsize)
	le.PutUint64(ph[32:], 16)
	le.PutUint64(ph[40:], 16)
	copy(b[ehsize+phsize:], "/lib/ld.so\x00")
	path := filepath.Join(t.TempDir(), "bin")
	if err := os.WriteFile(path, b, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkAsset(path, "x86_64-linux"); !errors.Is(err, errAssetCheck) {
		t.Fatalf("ELF with PT_INTERP: err = %v, want errAssetCheck", err)
	}
}

// TestHeldOnOneSystemKeepsThePreviousAssetEverywhere: beta is red on
// x86_64-linux but built on aarch64-linux. The new aarch64 bytes sit in the
// release set; the manifest must still hold beta at the previous asset on
// every system, and the release set must carry the previous bytes.
func TestHeldOnOneSystemKeepsThePreviousAssetEverywhere(t *testing.T) {
	systems := []string{"x86_64-linux", "aarch64-linux"}
	previous := makeAssets(t, systems, "beta")
	prev := Manifest{Source: "old", Tag: "nebula-20261101-1", Packages: map[string]Package{"beta": {Version: "1.2.0", Systems: map[string]Asset{}}}}
	for _, s := range systems {
		name := "beta-1.2.0-" + s + ".tar.zst"
		sum, err := fileSHA256(filepath.Join(previous, name))
		if err != nil {
			t.Fatal(err)
		}
		prev.Packages["beta"].Systems[s] = Asset{URL: "https://x/nebula-20261101-1/" + name, SHA256: sum}
	}
	prevData, _ := json.Marshal(prev)
	assets := makeAssets(t, systems, "alpha")
	writeFile(t, assets, "beta-1.2.0-aarch64-linux.tar.zst", "new aarch64 bytes")
	p := Plan{Source: "s", Previous: prevData, Build: []string{"alpha", "beta"},
		Binaries: []Binary{{Package: "alpha", Version: "1.2.0"}, {Package: "beta", Version: "1.2.0"}}}
	m, err := buildManifest(p, "nebula-20261117-4", assets, previous, "https://x", systems, []string{"beta"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m.Held, []string{"beta"}) {
		t.Fatalf("held = %v, want [beta]", m.Held)
	}
	for _, s := range systems {
		got := m.Packages["beta"].Systems[s]
		if got.SHA256 != prev.Packages["beta"].Systems[s].SHA256 {
			t.Fatalf("%s: beta sha256 = %s, want the previous asset", s, got.SHA256)
		}
		sum, err := fileSHA256(filepath.Join(assets, "beta-1.2.0-"+s+".tar.zst"))
		if err != nil || sum != got.SHA256 {
			t.Fatalf("%s: release set has %s (%v), want the previous bytes", s, sum, err)
		}
	}
}

func TestPlanSkipsNonGoBinaries(t *testing.T) {
	dir := newRepo(t)
	writeFile(t, dir, "tools/hooks/moon.yml", "layer: tool\n")
	writeFile(t, dir, "tools/hooks/Cargo.toml", "[package]\n")
	writeFile(t, dir, "tools/hooks/component.cue", strings.ReplaceAll(toolCUE, "%s", "hooks"))
	commitAll(t, dir, "rust tool")
	p, err := makePlan(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.NotBuilt, []string{"hooks"}) {
		t.Fatalf("notBuilt = %v, want [hooks]", p.NotBuilt)
	}
	if slices.Contains(p.Projects, "tools/hooks") || slices.Contains(p.Build, "hooks") {
		t.Fatalf("a non-Go binary was planned: projects %v, build %v", p.Projects, p.Build)
	}
	m, err := buildManifest(p, "nebula-20261117-5", makeAssets(t, []string{"x86_64-linux"}, "alpha", "beta"), "",
		"https://x", []string{"x86_64-linux"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Packages["hooks"]; ok || slices.Contains(m.Held, "hooks") {
		t.Fatalf("hooks must stay out of the manifest: %+v", m)
	}
}

func TestPublishRefusesAReRunOfTheReleasedRun(t *testing.T) {
	f := newPublishFixture(t)
	release(t, f.work, "nebula-20261117-6", f.source, nil)
	testGit(t, f.work, "push", "-q", "origin", "main", "--tags")
	if _, err := publish(context.Background(), f.options("nebula-20261117-6")); !errors.Is(err, errStaleRelease) {
		t.Fatalf("err = %v, want errStaleRelease for a re-run of the same source", err)
	}
}

func TestReleaseFilesRefusesChangedBytes(t *testing.T) {
	systems := []string{"x86_64-linux"}
	assets := makeAssets(t, systems, "alpha")
	p := Plan{Source: "s", Build: []string{"alpha"}, Binaries: []Binary{{Package: "alpha", Version: "1.2.0"}}}
	m, err := buildManifest(p, "nebula-20261117-7", assets, "", "https://x", systems, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(m)
	dir := t.TempDir()
	writeFile(t, dir, manifestFile, string(data))
	writeFile(t, assets, "alpha-1.2.0-x86_64-linux.tar.zst", "swapped after the manifest")
	if _, err := releaseFiles(filepath.Join(dir, manifestFile), assets, filepath.Join(dir, "SHA256SUMS")); !errors.Is(err, errAssetMismatch) {
		t.Fatalf("err = %v, want errAssetMismatch", err)
	}
}
