// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"cuelang.org/go/cue/cuecontext"
)

const manifestFile = "release-manifest.json"

// Artifact kinds in component.cue. kindGo is built with go build in the project
// directory; kindNix is built with nix build .#<attr> in the project directory.
const (
	kindGo  = "binary"
	kindNix = "nix"
)

var errNotDescendant = errors.New("HEAD does not descend from the previous release source")
var (
	errNixAttr       = errors.New("nix artifact needs an attr")
	errNixFlake      = errors.New("nix artifact project has no flake.nix")
	errUnknownSystem = errors.New("artifact names an unknown release system")
	errRuntimeNotNix = errors.New("runtimeDeps needs a nix artifact")
	errNameClash     = errors.New("image and package share a name")
	errVersionDrift  = errors.New("rigel-version differs from release.base")
)

// defaultSystems are the release systems an artifact gets when it names none.
// windowsAMD64 is opt-in: an artifact must name it in systems.
var defaultSystems = []string{"x86_64-linux", "aarch64-linux", "aarch64-darwin"}

const windowsAMD64 = "windows-amd64"

// releaseSystems is every system a Release can ship, the default set plus
// the opt-in ones. An artifact's systems must come from this list.
var releaseSystems = append(slices.Clone(defaultSystems), windowsAMD64)

// Binary is one releasable binary package declared by a tool's component.cue.
type Binary struct {
	Package string `json:"package"`
	Project string `json:"project"`
	Version string `json:"version"`
	// Kind is how CD builds the package: kindGo or kindNix.
	Kind string `json:"kind"`
	// Attr is the flake attribute a kindNix package builds: nix build .#<attr>.
	Attr string `json:"attr,omitempty"`
	// UpstreamRev is the imported upstream commit, for forks only.
	UpstreamRev string   `json:"upstreamRev,omitempty"`
	Binaries    []string `json:"binaries"`
	// RuntimeDeps lists the sonames a declared-runtime nix artifact may link.
	RuntimeDeps []string `json:"runtimeDeps,omitempty"`
	// Systems limits the artifact to these release systems. Empty means all.
	Systems []string `json:"systems,omitempty"`
}

// Plan is the batch job's build plan and the previous Release it builds on.
type Plan struct {
	Source      string          `json:"source"`
	PreviousTag string          `json:"previousTag,omitempty"`
	Previous    json.RawMessage `json:"previous,omitempty"`
	Projects    []string        `json:"projects"`
	Binaries    []Binary        `json:"binaries"`
	Build       []string        `json:"build"`
	// NotBuilt lists binary packages CD has no build step for (no go.mod). They
	// are not tested, built, held or put in the manifest; the notes list them.
	NotBuilt []string `json:"notBuilt"`
	// Images lists every container image; BuildImages names the ones whose
	// project changed. The rest are re-tagged at their previous digest.
	Images      []ImageSpec `json:"images"`
	BuildImages []string    `json:"buildImages"`
}

func makePlan(ctx context.Context, repo string) (Plan, error) {
	head, err := git(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		return Plan{}, err
	}
	all, err := listProjects(repo)
	if err != nil {
		return Plan{}, err
	}
	listed, err := listBinaries(repo)
	if err != nil {
		return Plan{}, err
	}
	p := Plan{Source: head, NotBuilt: []string{}}
	var binaries []Binary
	notBuiltProjects := map[string]bool{}
	for _, b := range listed {
		if b.Kind == kindNix {
			binaries = append(binaries, b)
			continue
		}
		if _, err := os.Stat(filepath.Join(repo, b.Project, "go.mod")); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return Plan{}, err
			}
			p.NotBuilt = append(p.NotBuilt, b.Package)
			notBuiltProjects[b.Project] = true
			continue
		}
		binaries = append(binaries, b)
	}
	p.Binaries = binaries
	if p.Images, err = listImages(repo); err != nil {
		return Plan{}, err
	}
	if err = checkNameClash(listed, p.Images); err != nil {
		return Plan{}, err
	}
	all = slices.DeleteFunc(all, func(project string) bool { return notBuiltProjects[project] })

	tag, prev, raw, err := newestRelease(ctx, repo)
	if err != nil {
		return Plan{}, err
	}
	if tag == "" {
		p.Projects = all
	} else {
		p.PreviousTag = tag
		p.Previous = json.RawMessage(raw)
		ok, err := isAncestor(ctx, repo, prev.Source, head)
		if err != nil {
			return Plan{}, err
		}
		if !ok {
			return Plan{}, fmt.Errorf("%w: %s is not an ancestor of %s", errNotDescendant, prev.Source, head)
		}
		changed, err := git(ctx, repo, "diff", "--name-only", prev.Source, head)
		if err != nil {
			return Plan{}, err
		}
		set := map[string]bool{}
		for _, file := range lines(changed) {
			if project, ok := projectOf(file); ok && slices.Contains(all, project) {
				set[project] = true
			}
		}
		for _, held := range prev.Held {
			if project := projectOfPackage(binaries, held); project != "" {
				set[project] = true
			} else if i := slices.IndexFunc(p.Images, func(i ImageSpec) bool { return i.Name == held }); i >= 0 {
				set[p.Images[i].Project] = true
			}
		}
		for project := range set {
			p.Projects = append(p.Projects, project)
		}
		sort.Strings(p.Projects)
	}
	if p.Projects == nil {
		p.Projects = []string{}
	}
	p.Build = []string{}
	for _, b := range binaries {
		if slices.Contains(p.Projects, b.Project) {
			p.Build = append(p.Build, b.Package)
		}
	}
	p.BuildImages = []string{}
	for _, i := range p.Images {
		if slices.Contains(p.Projects, i.Project) {
			p.BuildImages = append(p.BuildImages, i.Name)
		}
	}
	if p.Images == nil {
		p.Images = []ImageSpec{}
	}
	return p, nil
}

// checkNameClash fails when an image shares a name with a binary package. The
// Release keeps both kinds of name in one held list, so a shared name is ambiguous.
func checkNameClash(binaries []Binary, images []ImageSpec) error {
	for _, i := range images {
		if slices.ContainsFunc(binaries, func(b Binary) bool { return b.Package == i.Name }) {
			return fmt.Errorf("%w: %s", errNameClash, i.Name)
		}
	}
	return nil
}

// projectOf maps a repository path to its tools/<name> or forks/<name> project.
func projectOf(file string) (string, bool) {
	parts := strings.SplitN(file, "/", 3)
	if len(parts) < 3 || (parts[0] != "tools" && parts[0] != "forks") {
		return "", false
	}
	return parts[0] + "/" + parts[1], true
}

// projectOfPackage resolves a held name: a package of a binary, or a project path.
func projectOfPackage(binaries []Binary, name string) string {
	for _, b := range binaries {
		if b.Package == name {
			return b.Project
		}
	}
	if _, ok := projectOf(name + "/x"); ok {
		return name
	}
	return ""
}

// listProjects returns every tools/* and forks/* directory with a moon.yml.
func listProjects(repo string) ([]string, error) {
	var out []string
	for _, root := range []string{"tools", "forks"} {
		matches, err := filepath.Glob(filepath.Join(repo, root, "*", "moon.yml"))
		if err != nil {
			return nil, err
		}
		for _, m := range matches {
			out = append(out, path.Join(root, filepath.Base(filepath.Dir(m))))
		}
	}
	sort.Strings(out)
	return out, nil
}

type component struct {
	Release struct {
		Base      string `json:"base"`
		Artifacts map[string]struct {
			Kind        string   `json:"kind"`
			Attr        string   `json:"attr"`
			Binaries    []string `json:"binaries"`
			RuntimeDeps []string `json:"runtimeDeps"`
			Systems     []string `json:"systems"`
		} `json:"artifacts"`
	} `json:"release"`
}

// listBinaries reads every tools/*/component.cue and forks/*/component.cue
// artifact of kind "binary". A fork's upstreamRev comes from its upstream.json.
func listBinaries(repo string) ([]Binary, error) {
	var matches []string
	for _, root := range []string{"tools", "forks"} {
		found, err := filepath.Glob(filepath.Join(repo, root, "*", "component.cue"))
		if err != nil {
			return nil, err
		}
		matches = append(matches, found...)
	}
	ctx := cuecontext.New()
	var out []Binary
	for _, m := range matches {
		data, err := os.ReadFile(m)
		if err != nil {
			return nil, err
		}
		value := ctx.CompileBytes(data)
		var c component
		if err := value.Decode(&c); err != nil {
			return nil, fmt.Errorf("decode %s: %w", m, err)
		}
		project := path.Join(filepath.Base(filepath.Dir(filepath.Dir(m))), filepath.Base(filepath.Dir(m)))
		if err := checkRigelVersion(filepath.Dir(m), project, c.Release.Base); err != nil {
			return nil, err
		}
		upstreamRev := ""
		if strings.HasPrefix(project, "forks/") {
			if upstreamRev, err = readUpstreamRev(filepath.Join(filepath.Dir(m), "upstream.json")); err != nil {
				return nil, err
			}
		}
		for name, a := range c.Release.Artifacts {
			for _, s := range a.Systems {
				if !slices.Contains(releaseSystems, s) {
					return nil, fmt.Errorf("%w: %s: artifact %q names %q", errUnknownSystem, m, name, s)
				}
			}
			if a.Kind != kindGo && a.Kind != kindNix {
				continue
			}
			if c.Release.Base == "" || len(a.Binaries) == 0 {
				return nil, fmt.Errorf("%s: binary artifact %q needs release.base and binaries", m, name)
			}
			if a.Kind == kindNix {
				if a.Attr == "" {
					return nil, fmt.Errorf("%w: %s: artifact %q", errNixAttr, m, name)
				}
				if _, err := os.Stat(filepath.Join(filepath.Dir(m), "flake.nix")); err != nil {
					return nil, fmt.Errorf("%w: %s: %w", errNixFlake, m, err)
				}
			} else if len(a.RuntimeDeps) > 0 {
				return nil, fmt.Errorf("%w: %s: artifact %q", errRuntimeNotNix, m, name)
			}
			out = append(out, Binary{Package: name, Project: project, Version: c.Release.Base, Kind: a.Kind, Attr: a.Attr, UpstreamRev: upstreamRev, Binaries: a.Binaries, RuntimeDeps: a.RuntimeDeps, Systems: a.Systems})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Package < out[j].Package })
	return out, nil
}

func readUpstreamRev(file string) (string, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	var u struct {
		Commit string `json:"commit"`
	}
	if err := json.Unmarshal(data, &u); err != nil {
		return "", fmt.Errorf("decode %s: %w", file, err)
	}
	if u.Commit == "" {
		return "", fmt.Errorf("%s has no commit", file)
	}
	return u.Commit, nil
}

// newestRelease returns the newest nebula-* tag and its manifest, or "" with no tag.
func newestRelease(ctx context.Context, repo string) (string, Manifest, string, error) {
	tags, err := git(ctx, repo, "tag", "--list", "nebula-*", "--sort=-v:refname")
	if err != nil || tags == "" {
		return "", Manifest{}, "", err
	}
	tag := lines(tags)[0]
	raw, err := git(ctx, repo, "show", tag+":"+manifestFile)
	if err != nil {
		return "", Manifest{}, "", err
	}
	var m Manifest
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return "", Manifest{}, "", fmt.Errorf("decode %s manifest: %w", tag, err)
	}
	return tag, m, raw, nil
}

func writeJSON(out string, stdout io.Writer, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if out == "" {
		_, err = stdout.Write(data)
		return err
	}
	return os.WriteFile(out, data, 0o644)
}
