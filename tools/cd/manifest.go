// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

var errMissingAsset = errors.New("missing release asset")

// Asset is one per-system download of a package.
type Asset struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// Package is one manifest package entry: version fields plus one Asset per system.
type Package struct {
	Version     string
	UpstreamRev string
	StorePath   string
	Systems     map[string]Asset
}

// MarshalJSON writes the flat T6 shape: version keys beside per-system objects.
func (p Package) MarshalJSON() ([]byte, error) {
	m := map[string]any{"version": p.Version}
	if p.UpstreamRev != "" {
		m["upstreamRev"] = p.UpstreamRev
	}
	if p.StorePath != "" {
		m["storePath"] = p.StorePath
	}
	for system, a := range p.Systems {
		m[system] = a
	}
	return json.Marshal(m)
}

// UnmarshalJSON reads the flat T6 shape.
func (p *Package) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	p.Systems = map[string]Asset{}
	for key, value := range raw {
		var err error
		switch key {
		case "version":
			err = json.Unmarshal(value, &p.Version)
		case "upstreamRev":
			err = json.Unmarshal(value, &p.UpstreamRev)
		case "storePath":
			err = json.Unmarshal(value, &p.StorePath)
		default:
			var a Asset
			err = json.Unmarshal(value, &a)
			p.Systems[key] = a
		}
		if err != nil {
			return fmt.Errorf("package key %q: %w", key, err)
		}
	}
	return nil
}

// Manifest is release-manifest.json: source, tag, held, then one key per package.
type Manifest struct {
	Source   string
	Tag      string
	Held     []string
	Packages map[string]Package
}

// MarshalJSON writes packages as top-level keys beside source, tag and held.
func (m Manifest) MarshalJSON() ([]byte, error) {
	out := map[string]any{"source": m.Source, "tag": m.Tag, "held": m.Held}
	if m.Held == nil {
		out["held"] = []string{}
	}
	for name, p := range m.Packages {
		out[name] = p
	}
	return json.Marshal(out)
}

// UnmarshalJSON reads the top-level package keys.
func (m *Manifest) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	m.Packages = map[string]Package{}
	for key, value := range raw {
		var err error
		switch key {
		case "source":
			err = json.Unmarshal(value, &m.Source)
		case "tag":
			err = json.Unmarshal(value, &m.Tag)
		case "held":
			err = json.Unmarshal(value, &m.Held)
		default:
			var p Package
			err = json.Unmarshal(value, &p)
			m.Packages[key] = p
		}
		if err != nil {
			return fmt.Errorf("manifest key %q: %w", key, err)
		}
	}
	return nil
}

type manifestInput struct {
	planPath string
	tag      string
	assets   string
	baseURL  string
	held     []string
	systems  []string
}

// build renders the manifest bytes from the plan, the built assets and the held list.
func (in *manifestInput) build() ([]byte, error) {
	if in.planPath == "" || in.tag == "" || in.baseURL == "" || len(in.systems) == 0 {
		return nil, errors.New("manifest needs -plan, -tag, -base-url and at least one -system")
	}
	data, err := os.ReadFile(in.planPath)
	if err != nil {
		return nil, err
	}
	var p Plan
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("decode plan: %w", err)
	}
	m, err := buildManifest(p, in.tag, in.assets, in.baseURL, in.systems, in.held)
	if err != nil {
		return nil, err
	}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

func buildManifest(p Plan, tag, assets, baseURL string, systems, held []string) (Manifest, error) {
	var prev Manifest
	if len(p.Previous) > 0 {
		if err := json.Unmarshal(p.Previous, &prev); err != nil {
			return Manifest{}, fmt.Errorf("decode previous manifest: %w", err)
		}
	}
	m := Manifest{Source: p.Source, Tag: tag, Held: []string{}, Packages: map[string]Package{}}
	for _, b := range p.Binaries {
		if slices.Contains(held, b.Package) {
			m.Held = append(m.Held, b.Package)
			if old, ok := prev.Packages[b.Package]; ok {
				m.Packages[b.Package] = old
			}
			continue
		}
		if !slices.Contains(p.Build, b.Package) {
			if old, ok := prev.Packages[b.Package]; ok {
				m.Packages[b.Package] = old
				continue
			}
			return Manifest{}, fmt.Errorf("%s was not built and has no previous Release entry", b.Package)
		}
		entry := Package{Version: b.Version, Systems: map[string]Asset{}}
		for _, system := range systems {
			name := fmt.Sprintf("%s-%s-%s.tar.zst", b.Package, b.Version, system)
			sum, err := fileSHA256(filepath.Join(assets, name))
			if err != nil {
				return Manifest{}, fmt.Errorf("%w %s: %w", errMissingAsset, name, err)
			}
			entry.Systems[system] = Asset{URL: strings.TrimSuffix(baseURL, "/") + "/" + tag + "/" + name, SHA256: sum}
		}
		m.Packages[b.Package] = entry
	}
	sort.Strings(m.Held)
	return m, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
