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
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

var (
	errMissingAsset  = errors.New("missing release asset")
	errAssetMismatch = errors.New("release asset sha256 mismatch")
)

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
	previous string
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
	m, err := buildManifest(p, in.tag, in.assets, in.previous, in.baseURL, in.systems, in.held)
	if err != nil {
		return nil, err
	}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

func buildManifest(p Plan, tag, assets, previous, baseURL string, systems, held []string) (Manifest, error) {
	var prev Manifest
	if len(p.Previous) > 0 {
		if err := json.Unmarshal(p.Previous, &prev); err != nil {
			return Manifest{}, fmt.Errorf("decode previous manifest: %w", err)
		}
	}
	prefix := strings.TrimSuffix(baseURL, "/") + "/" + tag + "/"
	m := Manifest{Source: p.Source, Tag: tag, Held: []string{}, Packages: map[string]Package{}}
	for _, b := range p.Binaries {
		built := slices.Contains(p.Build, b.Package)
		if slices.Contains(held, b.Package) {
			m.Held = append(m.Held, b.Package)
			built = false
		}
		if !built {
			old, ok := prev.Packages[b.Package]
			if !ok {
				if slices.Contains(held, b.Package) {
					continue // Held before its first Release: the notes list it.
				}
				return Manifest{}, fmt.Errorf("%s was not built and has no previous Release entry", b.Package)
			}
			entry, err := carry(old, previous, assets, prefix)
			if err != nil {
				return Manifest{}, fmt.Errorf("%s: %w", b.Package, err)
			}
			m.Packages[b.Package] = entry
			continue
		}
		entry := Package{Version: b.Version, UpstreamRev: b.UpstreamRev, Systems: map[string]Asset{}}
		for _, system := range systems {
			name := fmt.Sprintf("%s-%s-%s.tar.zst", b.Package, b.Version, system)
			sum, err := fileSHA256(filepath.Join(assets, name))
			if err != nil {
				return Manifest{}, fmt.Errorf("%w %s: %w", errMissingAsset, name, err)
			}
			entry.Systems[system] = Asset{URL: prefix + name, SHA256: sum}
		}
		m.Packages[b.Package] = entry
	}
	sort.Strings(m.Held)
	return m, nil
}

// carry copies a previous entry into this Release. Each asset is read from
// previous, the previous Release's downloaded assets, must match its recorded
// sha256, and is copied into assets over any newly built file of the same name
// (a package held on one system may have been rebuilt on another). Its URL
// moves under this tag.
func carry(old Package, previous, assets, prefix string) (Package, error) {
	entry := old
	entry.Systems = map[string]Asset{}
	for system, a := range old.Systems {
		name := path.Base(a.URL)
		src := filepath.Join(previous, name)
		sum, err := fileSHA256(src)
		if err != nil {
			return Package{}, fmt.Errorf("%w %s: %w", errMissingAsset, name, err)
		}
		if sum != a.SHA256 {
			return Package{}, fmt.Errorf("%w: %s has sha256 %s, previous Release recorded %s", errAssetMismatch, name, sum, a.SHA256)
		}
		if err := copyFile(src, filepath.Join(assets, name)); err != nil {
			return Package{}, err
		}
		entry.Systems[system] = Asset{URL: prefix + name, SHA256: sum}
	}
	return entry, nil
}

// releaseFiles writes SHA256SUMS for every asset the manifest references and
// returns those asset paths. Assets the manifest does not reference are not released.
func releaseFiles(manifestPath, assets, sumsPath string) ([]string, error) {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	var names []string
	for _, p := range m.Packages {
		for _, a := range p.Systems {
			name := path.Base(a.URL)
			sum, err := fileSHA256(filepath.Join(assets, name))
			if err != nil {
				return nil, fmt.Errorf("%w %s: %w", errMissingAsset, name, err)
			}
			if sum != a.SHA256 {
				return nil, fmt.Errorf("%w: %s", errAssetMismatch, name)
			}
			names = append(names, sum+"  "+name)
		}
	}
	sort.Slice(names, func(i, j int) bool { return names[i][66:] < names[j][66:] })
	var sums strings.Builder
	files := make([]string, 0, len(names))
	for _, line := range names {
		sums.WriteString(line + "\n")
		files = append(files, filepath.Join(assets, line[66:]))
	}
	if err := os.WriteFile(sumsPath, []byte(sums.String()), 0o644); err != nil {
		return nil, err
	}
	return files, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
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
