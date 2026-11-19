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
// component.cue. CD builds it from the project directory with buildx.
type ImageSpec struct {
	Name       string   `json:"name"`
	Project    string   `json:"project"`
	Repository string   `json:"repository"`
	Dockerfile string   `json:"dockerfile"`
	Platforms  []string `json:"platforms"`
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
		Repository string   `json:"repository"`
		Dockerfile string   `json:"dockerfile"`
		Platforms  []string `json:"platforms"`
	} `json:"images"`
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
			out = append(out, ImageSpec{Name: name, Project: project, Repository: i.Repository, Dockerfile: i.Dockerfile, Platforms: i.Platforms})
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
