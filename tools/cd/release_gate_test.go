// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/cuecontext"
)

// The release gate is a test, so moon's test task fails a PR that adds a
// component with nothing to ship. The task's inputs include every component.cue.
// The allowlist and side paths live in release-pending.cue.

// kindLibrary is an artifact that ships no executable. A component whose
// artifacts are all libraries still ships a release.
const kindLibrary = "library"

// gateConfigFile holds the gate's allowlist and side paths, beside this test.
const gateConfigFile = "release-pending.cue"

// publishedKinds are the forms a component can take instead of a Release. A
// runtime-tarball is an asset that a named CD side path uploads.
var publishedKinds = map[string]bool{
	"github-action":      true,
	"terraform-provider": true,
	"image":              true,
	"runtime-tarball":    true,
}

var (
	errArtifactKind = errors.New("unknown artifact kind")
	errPublished    = errors.New("published needs a known kind and a ref")
	errSidePath     = errors.New("sidePaths entry needs a known kind and a ref")
	errGateOverlap  = errors.New("project is both pending and on a sidePath")
)

// gateConfig is the decoded release-pending.cue.
type gateConfig struct {
	// Pending maps a project to the tracking issue for its missing release.
	Pending map[string]pendingEntry `json:"pending"`
	// SidePaths maps a project to the CD side path that publishes its release.
	SidePaths map[string]sidePath `json:"sidePaths"`
}

type pendingEntry struct {
	Issue string `json:"issue"`
}

type sidePath struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

// componentForm is the part of a component.cue that the gate reads.
type componentForm struct {
	Release *struct {
		Artifacts map[string]struct {
			Kind string `json:"kind"`
		} `json:"artifacts"`
	} `json:"release"`
	Images    map[string]struct{} `json:"images"`
	Published *publishedForm      `json:"published"`
}

type publishedForm struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

// componentShips reports whether one component.cue ships a release, images, or a
// published form. An artifact of an unknown kind, or a malformed published form,
// is an error whatever else the component declares.
func componentShips(data []byte) (bool, error) {
	var c componentForm
	if err := cuecontext.New().CompileBytes(data).Decode(&c); err != nil {
		return false, err
	}
	released := false
	if c.Release != nil {
		for _, name := range slices.Sorted(maps.Keys(c.Release.Artifacts)) {
			switch kind := c.Release.Artifacts[name].Kind; kind {
			case kindGo, kindNix, kindLibrary:
				released = true
			default:
				return false, fmt.Errorf("%w: artifact %q has kind %q", errArtifactKind, name, kind)
			}
		}
	}
	if c.Published != nil && (!publishedKinds[c.Published.Kind] || c.Published.Ref == "") {
		return false, fmt.Errorf("%w: kind %q, ref %q", errPublished, c.Published.Kind, c.Published.Ref)
	}
	return released || c.Published != nil || len(c.Images) > 0, nil
}

// checkReleaseGate fails each component.cue under repo that ships nothing, unless
// its project is pending or on a side path. It also fails a pending project that
// now ships, a listed project with no component.cue, and a project listed as both
// pending and on a side path. It reports every offender.
func checkReleaseGate(repo string, cfg gateConfig) error {
	var errs []error
	seen := map[string]bool{}
	for _, root := range []string{"tools", "forks"} {
		matches, err := filepath.Glob(filepath.Join(repo, root, "*", "component.cue"))
		if err != nil {
			return err
		}
		for _, m := range matches {
			project := path.Join(root, filepath.Base(filepath.Dir(m)))
			file := project + "/component.cue"
			seen[project] = true
			data, err := os.ReadFile(m)
			if err != nil {
				return err
			}
			ships, err := componentShips(data)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", file, err))
				continue
			}
			_, pending := cfg.Pending[project]
			_, side := cfg.SidePaths[project]
			switch {
			case ships && pending:
				errs = append(errs, fmt.Errorf("%s: ships now; remove %s from %s", file, project, gateConfigFile))
			case !ships && !pending && !side:
				errs = append(errs, fmt.Errorf("%s: ships no release, images or published form; add one, or list %s in %s", file, project, gateConfigFile))
			}
		}
	}
	for _, project := range slices.Sorted(maps.Keys(cfg.Pending)) {
		if !seen[project] {
			errs = append(errs, fmt.Errorf("%s: pending in %s but has no component.cue", project, gateConfigFile))
		}
		if _, ok := cfg.SidePaths[project]; ok {
			errs = append(errs, fmt.Errorf("%s: %w", project, errGateOverlap))
		}
	}
	for _, project := range slices.Sorted(maps.Keys(cfg.SidePaths)) {
		side := cfg.SidePaths[project]
		if !publishedKinds[side.Kind] || side.Ref == "" {
			errs = append(errs, fmt.Errorf("%s: %w: kind %q, ref %q", project, errSidePath, side.Kind, side.Ref))
		}
		if !seen[project] {
			errs = append(errs, fmt.Errorf("%s: sidePath in %s but has no component.cue", project, gateConfigFile))
		}
	}
	return errors.Join(errs...)
}

// loadGateConfig reads and validates the gate's config file.
func loadGateConfig(file string) (gateConfig, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return gateConfig{}, err
	}
	v := cuecontext.New().CompileBytes(data, cue.Filename(file))
	if err := v.Validate(cue.Concrete(true)); err != nil {
		return gateConfig{}, err
	}
	var cfg gateConfig
	if err := v.Decode(&cfg); err != nil {
		return gateConfig{}, err
	}
	return cfg, nil
}

// TestReleaseGate runs the gate over fixture repos. Each case names the
// components it writes and the substrings the gate error must contain. An empty
// wantErr means the gate passes.
func TestReleaseGate(t *testing.T) {
	t.Parallel()
	const none = "pending: {}\nsidePaths: {}\n"
	tests := []struct {
		name       string
		components map[string]string
		config     string
		wantErr    []string
	}{
		{
			name:       "compliant binary release",
			components: map[string]string{"tools/alpha": `release: {base: "1.0.0", artifacts: {alpha: {kind: "binary", binaries: ["alpha"]}}}`},
			config:     none,
		},
		{
			name:       "compliant nix release",
			components: map[string]string{"forks/beta": `release: {base: "1.0.0", artifacts: {beta: {kind: "nix", attr: "beta", binaries: ["beta"]}}}`},
			config:     none,
		},
		{
			name:       "library-only release is compliant",
			components: map[string]string{"tools/go-lib": `release: {base: "0.1.0", artifacts: {"go-lib": {kind: "library"}}}`},
			config:     none,
		},
		{
			name:       "images-only is published",
			components: map[string]string{"forks/autoscaler": `images: {autoscaler: {repository: "ghcr.io/rigelbuild/autoscaler", dockerfile: "Dockerfile", platforms: ["linux/amd64"]}}`},
			config:     none,
		},
		{
			name:       "published form",
			components: map[string]string{"forks/cla-action": `published: {kind: "github-action", ref: "owner/action@v1"}`},
			config:     none,
		},
		{
			name:       "allowlisted pending",
			components: map[string]string{"forks/cla-action": `id: "cla-action"`},
			config:     `pending: {"forks/cla-action": {issue: "RIG-12302"}}` + "\nsidePaths: {}\n",
		},
		{
			name:       "new non-compliant",
			components: map[string]string{"tools/newthing": `id: "newthing"`},
			config:     none,
			wantErr:    []string{"tools/newthing/component.cue: ships no release"},
		},
		{
			name:       "reports every offender",
			components: map[string]string{"tools/one": `id: "one"`, "forks/two": `id: "two"`},
			config:     none,
			wantErr:    []string{"tools/one/component.cue", "forks/two/component.cue"},
		},
		{
			name:       "stale allowlist entry that now complies",
			components: map[string]string{"tools/alpha": `release: {base: "1.0.0", artifacts: {alpha: {kind: "binary", binaries: ["alpha"]}}}`},
			config:     `pending: {"tools/alpha": {issue: "RIG-12302"}}` + "\nsidePaths: {}\n",
			wantErr:    []string{"tools/alpha/component.cue: ships now"},
		},
		{
			name:       "stale allowlist entry with no component.cue",
			components: map[string]string{"tools/alpha": `release: {base: "1.0.0", artifacts: {alpha: {kind: "binary", binaries: ["alpha"]}}}`},
			config:     `pending: {"tools/gone": {issue: "RIG-12302"}}` + "\nsidePaths: {}\n",
			wantErr:    []string{"tools/gone: pending in release-pending.cue but has no component.cue"},
		},
		{
			name:       "unknown artifact kind fails beside a valid one",
			components: map[string]string{"tools/alpha": `release: {base: "1.0.0", artifacts: {alpha: {kind: "binary", binaries: ["alpha"]}, beta: {kind: "binaries", binaries: ["beta"]}}}`},
			config:     none,
			wantErr:    []string{"unknown artifact kind", `kind "binaries"`},
		},
		{
			name:       "artifact with no kind fails",
			components: map[string]string{"tools/alpha": `release: {base: "1.0.0", artifacts: {alpha: {binaries: ["alpha"]}}}`},
			config:     none,
			wantErr:    []string{"unknown artifact kind"},
		},
		{
			name:       "release with no artifacts is not a release",
			components: map[string]string{"tools/alpha": `release: {base: "1.0.0", artifacts: {}}`},
			config:     none,
			wantErr:    []string{"ships no release"},
		},
		{
			name:       "published with an unknown kind fails",
			components: map[string]string{"forks/x": `published: {kind: "pdf", ref: "x"}`},
			config:     none,
			wantErr:    []string{"published needs a known kind"},
		},
		{
			name:       "published with no ref fails",
			components: map[string]string{"forks/x": `published: {kind: "github-action"}`},
			config:     none,
			wantErr:    []string{"published needs a known kind"},
		},
		{
			name:       "component.cue that does not compile fails",
			components: map[string]string{"forks/broken": `release: {`},
			config:     none,
			wantErr:    []string{"forks/broken/component.cue"},
		},
		{
			name:       "sidePath publishes cotal",
			components: map[string]string{"forks/cotal": `id: "cotal"`},
			config:     `pending: {}` + "\n" + `sidePaths: {"forks/cotal": {kind: "runtime-tarball", ref: "cd.yaml runtime job"}}` + "\n",
		},
		{
			name:       "sidePath with an unknown kind fails",
			components: map[string]string{"forks/cotal": `id: "cotal"`},
			config:     `pending: {}` + "\n" + `sidePaths: {"forks/cotal": {kind: "bogus", ref: "x"}}` + "\n",
			wantErr:    []string{"sidePaths entry needs a known kind"},
		},
		{
			name:       "project both pending and on a sidePath fails",
			components: map[string]string{"forks/cotal": `id: "cotal"`},
			config:     `pending: {"forks/cotal": {issue: "RIG-12302"}}` + "\n" + `sidePaths: {"forks/cotal": {kind: "runtime-tarball", ref: "x"}}` + "\n",
			wantErr:    []string{"forks/cotal: project is both pending and on a sidePath"},
		},
		{
			name:       "sidePath with no component.cue fails",
			components: map[string]string{},
			config:     `pending: {}` + "\n" + `sidePaths: {"forks/cotal": {kind: "runtime-tarball", ref: "x"}}` + "\n",
			wantErr:    []string{"forks/cotal: sidePath in release-pending.cue but has no component.cue"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			for project, body := range tt.components {
				writeFile(t, dir, project+"/component.cue", body)
			}
			writeFile(t, dir, gateConfigFile, tt.config)
			cfg, err := loadGateConfig(filepath.Join(dir, gateConfigFile))
			if err != nil {
				t.Fatalf("loadGateConfig: %v", err)
			}
			err = checkReleaseGate(dir, cfg)
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("gate failed: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("gate passed; want an error containing %q", tt.wantErr)
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("gate error %q does not contain %q", err, want)
				}
			}
		})
	}
}

// TestRealRepoPassesReleaseGate runs the gate over the real repo. A new component
// with nothing to ship, a stale allowlist entry, or an unknown kind fails here.
func TestRealRepoPassesReleaseGate(t *testing.T) {
	t.Parallel()
	cfg, err := loadGateConfig(gateConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkReleaseGate(filepath.Join("..", ".."), cfg); err != nil {
		t.Fatal(err)
	}
}
