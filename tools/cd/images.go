// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"cuelang.org/go/cue/cuecontext"
)

var (
	errImageSpec   = errors.New("image needs repository, dockerfile and platforms")
	errImageDigest = errors.New("image digest")
)

// ImageSpec is one container image declared under images in a project's
// component.cue. CD builds it from the project directory with buildx, passing
// BuildArgs as --build-arg. A BuildArgs value may name {source} (the tested
// commit), {tag} (this Release's tag), or {version} (the project's
// .github/rigel-version, resolved here); cd.yaml replaces {source} and {tag}.
type ImageSpec struct {
	Name       string            `json:"name"`
	Project    string            `json:"project"`
	Repository string            `json:"repository"`
	Dockerfile string            `json:"dockerfile"`
	Platforms  []string          `json:"platforms"`
	BuildArgs  map[string]string `json:"buildArgs,omitempty"`
}

// Image is one manifest image entry: the repository, this Release's tag on it,
// and the pushed digest, so a consumer can pin repository:tag@digest.
type Image struct {
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	Digest     string `json:"digest"`
}

type imagesComponent struct {
	Images map[string]struct {
		Repository string         `json:"repository"`
		Dockerfile string         `json:"dockerfile"`
		Platforms  []string       `json:"platforms"`
		BuildArgs  map[string]any `json:"buildArgs"`
	} `json:"images"`
}

var (
	errBuildArg        = errors.New("image buildArg is invalid")
	errBuildArgUnknown = errors.New("image buildArg names an unknown placeholder")
)

// buildArgPlaceholders are the only {...} placeholders a buildArgs value may name.
var buildArgPlaceholders = []string{"{source}", "{tag}", "{version}"}

// resolveBuildArgs checks one project's buildArgs and resolves {version} from
// its .github/rigel-version file. {source} and {tag} stay for cd.yaml.
func resolveBuildArgs(repo, project string, args map[string]any) (map[string]string, error) {
	if len(args) == 0 {
		return nil, nil
	}
	version := ""
	out := make(map[string]string, len(args))
	for key, raw := range args {
		value, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("%w: %s: %s is not a string", errBuildArg, project, key)
		}
		if strings.ContainsAny(value, "\n\r") {
			return nil, fmt.Errorf("%w: %s: %s contains a newline", errBuildArg, project, key)
		}
		rest := value
		for {
			open := strings.Index(rest, "{")
			if open < 0 {
				break
			}
			closeAt := strings.Index(rest[open:], "}")
			if closeAt < 0 {
				return nil, fmt.Errorf("%w: %s: %s has an unclosed {", errBuildArgUnknown, project, key)
			}
			token := rest[open : open+closeAt+1]
			if !slices.Contains(buildArgPlaceholders, token) {
				return nil, fmt.Errorf("%w: %s: %s uses %s", errBuildArgUnknown, project, key, token)
			}
			rest = rest[open+closeAt+1:]
		}
		if strings.Contains(value, "{version}") {
			if version == "" {
				v, err := readRigelVersion(filepath.Join(repo, project, ".github", "rigel-version"))
				if err != nil {
					return nil, fmt.Errorf("%w: %s: %s: %w", errBuildArg, project, key, err)
				}
				version = v
			}
			value = strings.ReplaceAll(value, "{version}", version)
		}
		out[key] = value
	}
	return out, nil
}

// readRigelVersion returns the one version in a fork's .github/rigel-version.
func readRigelVersion(file string) (string, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(data))
	if v == "" || strings.ContainsAny(v, "\n\r") {
		return "", fmt.Errorf("%s must hold exactly one version", file)
	}
	return v, nil
}

// listImages reads every tools/*/component.cue and forks/*/component.cue images entry.
func listImages(repo string) ([]ImageSpec, error) {
	var matches []string
	for _, root := range []string{"tools", "forks"} {
		found, err := filepath.Glob(filepath.Join(repo, root, "*", "component.cue"))
		if err != nil {
			return nil, err
		}
		matches = append(matches, found...)
	}
	ctx := cuecontext.New()
	var out []ImageSpec
	for _, m := range matches {
		data, err := os.ReadFile(m)
		if err != nil {
			return nil, err
		}
		var c imagesComponent
		if err := ctx.CompileBytes(data).Decode(&c); err != nil {
			return nil, fmt.Errorf("decode %s: %w", m, err)
		}
		project := path.Join(filepath.Base(filepath.Dir(filepath.Dir(m))), filepath.Base(filepath.Dir(m)))
		for name, i := range c.Images {
			if i.Repository == "" || i.Dockerfile == "" || len(i.Platforms) == 0 {
				return nil, fmt.Errorf("%w: %s: image %q", errImageSpec, m, name)
			}
			args, err := resolveBuildArgs(repo, project, i.BuildArgs)
			if err != nil {
				return nil, err
			}
			out = append(out, ImageSpec{Name: name, Project: project, Repository: i.Repository, Dockerfile: i.Dockerfile, Platforms: i.Platforms, BuildArgs: args})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// parseImageDigests reads name=digest pairs from the -image flag.
func parseImageDigests(pairs []string) (map[string]string, error) {
	out := map[string]string{}
	for _, pair := range pairs {
		name, digest, ok := strings.Cut(pair, "=")
		if !ok || name == "" || !strings.HasPrefix(digest, "sha256:") {
			return nil, fmt.Errorf("%w: %q is not name=sha256:<hex>", errImageDigest, pair)
		}
		out[name] = digest
	}
	return out, nil
}

// buildImages returns this Release's image entries and the held image names. A
// planned image pushed with a digest records it. An unplanned or held image keeps
// its previous digest, which CD re-tagged with this Release's tag.
func buildImages(p Plan, tag string, digests map[string]string, held []string) (map[string]Image, []string, error) {
	var prev Manifest
	if len(p.Previous) > 0 {
		if err := json.Unmarshal(p.Previous, &prev); err != nil {
			return nil, nil, fmt.Errorf("decode previous manifest: %w", err)
		}
	}
	images := map[string]Image{}
	var heldImages []string
	for _, i := range p.Images {
		built := slices.Contains(p.BuildImages, i.Name)
		isHeld := slices.Contains(held, i.Name)
		if isHeld {
			heldImages = append(heldImages, i.Name)
			built = false
		}
		if built {
			digest, ok := digests[i.Name]
			if !ok {
				return nil, nil, fmt.Errorf("%w: image %s was built and has no digest", errImageDigest, i.Name)
			}
			images[i.Name] = Image{Repository: i.Repository, Tag: tag, Digest: digest}
			continue
		}
		old, ok := prev.Images[i.Name]
		if !ok {
			if isHeld {
				continue // Held before its first Release: the notes list it.
			}
			return nil, nil, fmt.Errorf("image %s was not built and has no previous Release entry", i.Name)
		}
		images[i.Name] = Image{Repository: i.Repository, Tag: tag, Digest: old.Digest}
	}
	return images, heldImages, nil
}
