// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const imageCUE = `id: "pi"
images: {
	pi: {repository: "ghcr.io/x/pi", dockerfile: "Dockerfile.pi", platforms: ["linux/amd64"]}
}
`

func addImageFork(t *testing.T, dir string) {
	t.Helper()
	writeFile(t, dir, "forks/pi/moon.yml", "layer: application\n")
	writeFile(t, dir, "forks/pi/component.cue", imageCUE)
	writeFile(t, dir, "forks/pi/upstream.json", `{"commit":"abc"}`)
	writeFile(t, dir, "forks/pi/Dockerfile.pi", "FROM scratch\n")
	commitAll(t, dir, "image fork")
}

func TestPlanListsImagesAndBuildsChangedOnes(t *testing.T) {
	dir := newRepo(t)
	addImageFork(t, dir)
	p, err := makePlan(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []ImageSpec{{Name: "pi", Project: "forks/pi", Repository: "ghcr.io/x/pi", Dockerfile: "Dockerfile.pi", Platforms: []string{"linux/amd64"}}}
	if !reflect.DeepEqual(p.Images, want) {
		t.Fatalf("images = %+v, want %+v", p.Images, want)
	}
	if !reflect.DeepEqual(p.BuildImages, []string{"pi"}) {
		t.Fatalf("buildImages = %v, want [pi]", p.BuildImages)
	}

	release(t, dir, "nebula-20261101-1", testGit(t, dir, "rev-parse", "HEAD"), nil)
	writeFile(t, dir, "tools/alpha/main.go", "package main // x\n")
	commitAll(t, dir, "alpha change")
	if p, err = makePlan(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
	if len(p.BuildImages) != 0 {
		t.Fatalf("buildImages = %v, want none for an unchanged project", p.BuildImages)
	}

	writeFile(t, dir, "forks/pi/Dockerfile.pi", "FROM scratch\nENV X=1\n")
	commitAll(t, dir, "pi change")
	if p, err = makePlan(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.BuildImages, []string{"pi"}) {
		t.Fatalf("buildImages = %v, want [pi] after its project changed", p.BuildImages)
	}
}

func TestPlanRebuildsAHeldImage(t *testing.T) {
	dir := newRepo(t)
	addImageFork(t, dir)
	release(t, dir, "nebula-20261101-1", testGit(t, dir, "rev-parse", "HEAD"), []string{"pi"})
	writeFile(t, dir, "README.md", "x\n")
	commitAll(t, dir, "readme")
	p, err := makePlan(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.BuildImages, []string{"pi"}) {
		t.Fatalf("buildImages = %v, want [pi] for a held image", p.BuildImages)
	}
}

func TestPlanRefusesAnIncompleteImage(t *testing.T) {
	dir := newRepo(t)
	writeFile(t, dir, "forks/pi/component.cue", `images: {pi: {repository: "ghcr.io/x/pi"}}`+"\n")
	writeFile(t, dir, "forks/pi/upstream.json", `{"commit":"abc"}`)
	commitAll(t, dir, "bad image")
	if _, err := makePlan(t.Context(), dir); !errors.Is(err, errImageSpec) {
		t.Fatalf("err = %v, want errImageSpec", err)
	}
}

func TestManifestImagesRoundTrip(t *testing.T) {
	in := Manifest{Source: "s", Tag: "nebula-20261117-1", Held: []string{}, Packages: map[string]Package{},
		Images: map[string]Image{"omp": {Repository: "ghcr.io/rigelbuild/omp", Tag: "nebula-20261117-1", Digest: "sha256:aa"}}}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Manifest
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round trip = %+v, want %+v", out, in)
	}
	if _, isPkg := out.Packages["images"]; isPkg {
		t.Fatal("images decoded as a package")
	}
}

func TestOldManifestWithoutImagesParses(t *testing.T) {
	var m Manifest
	old := `{"source":"s","tag":"t","held":[],"alpha":{"version":"1.0.0","x86_64-linux":{"url":"u","sha256":"aa"}}}`
	if err := json.Unmarshal([]byte(old), &m); err != nil {
		t.Fatal(err)
	}
	if m.Images != nil || m.Packages["alpha"].Version != "1.0.0" {
		t.Fatalf("old manifest = %+v", m)
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"images"`) {
		t.Fatalf("a manifest without images wrote an images key: %s", data)
	}
}

func imagePlan(t *testing.T, build []string) Plan {
	t.Helper()
	prev, err := json.Marshal(Manifest{Source: "old", Tag: "nebula-20261101-1", Images: map[string]Image{
		"omp": {Repository: "ghcr.io/rigelbuild/omp", Tag: "nebula-20261101-1", Digest: "sha256:old"}}})
	if err != nil {
		t.Fatal(err)
	}
	return Plan{Source: "s", Previous: prev, BuildImages: build,
		Images: []ImageSpec{{Name: "omp", Repository: "ghcr.io/rigelbuild/omp"}}}
}

func TestImagesCarryTheirDigestWhenNotRebuilt(t *testing.T) {
	tag := "nebula-20261117-5"
	want := Image{Repository: "ghcr.io/rigelbuild/omp", Tag: tag, Digest: "sha256:old"}
	for _, tc := range []struct {
		name     string
		build    []string
		held     []string
		wantHeld []string
	}{
		{name: "unplanned"},
		{name: "held", build: []string{"omp"}, held: []string{"omp"}, wantHeld: []string{"omp"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			images, held, err := buildImages(imagePlan(t, tc.build), tag, map[string]string{"omp": "sha256:new"}, tc.held)
			if err != nil {
				t.Fatal(err)
			}
			if images["omp"] != want {
				t.Fatalf("omp = %+v, want %+v", images["omp"], want)
			}
			if !reflect.DeepEqual(held, tc.wantHeld) {
				t.Fatalf("held = %v, want %v", held, tc.wantHeld)
			}
		})
	}
}

func TestImagesRecordTheNewDigestWhenRebuilt(t *testing.T) {
	images, _, err := buildImages(imagePlan(t, []string{"omp"}), "nebula-20261117-6", map[string]string{"omp": "sha256:new"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if images["omp"].Digest != "sha256:new" || images["omp"].Tag != "nebula-20261117-6" {
		t.Fatalf("omp = %+v", images["omp"])
	}
	if _, _, err := buildImages(imagePlan(t, []string{"omp"}), "t", nil, nil); err == nil {
		t.Fatal("a built image with no digest must fail")
	}
}

func TestUnplannedImageWithoutPreviousEntryFails(t *testing.T) {
	p := imagePlan(t, nil)
	p.Previous = nil
	_, _, err := buildImages(p, "t", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "no previous Release entry") {
		t.Fatalf("err = %v, want the no-previous-entry error", err)
	}
}

func TestImageHeldBeforeFirstReleaseIsSkipped(t *testing.T) {
	p := imagePlan(t, []string{"omp"})
	p.Previous = nil
	images, held, err := buildImages(p, "t", map[string]string{"omp": "sha256:new"}, []string{"omp"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := images["omp"]; ok {
		t.Fatalf("held image with no previous Release was recorded: %+v", images)
	}
	if !reflect.DeepEqual(held, []string{"omp"}) {
		t.Fatalf("held = %v, want [omp]", held)
	}
}

func TestManifestMergesAndSortsHeldPackagesAndImages(t *testing.T) {
	// Neither has a previous Release, so both are skipped and only listed as held.
	p := Plan{Source: "s", Build: []string{"beta"}, BuildImages: []string{"aaa"},
		Binaries: []Binary{{Package: "beta", Version: "1.2.0"}},
		Images:   []ImageSpec{{Name: "aaa", Repository: "ghcr.io/x/aaa"}}}
	in := &manifestInput{planPath: writePlan(t, p), tag: "nebula-20261117-8", baseURL: "https://x/",
		systems: []string{"x86_64-linux"}, held: []string{"beta", "aaa"}}
	data, err := in.build()
	if err != nil {
		t.Fatal(err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m.Held, []string{"aaa", "beta"}) {
		t.Fatalf("held = %v, want [aaa beta]", m.Held)
	}
}

func TestManifestCommandRecordsImageDigests(t *testing.T) {
	p := imagePlan(t, []string{"omp"})
	in := &manifestInput{planPath: writePlan(t, p), tag: "nebula-20261117-7", baseURL: "https://x/",
		systems: []string{"x86_64-linux"}, images: []string{"omp=sha256:new"}}
	data, err := in.build()
	if err != nil {
		t.Fatal(err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m.Images["omp"].Digest != "sha256:new" {
		t.Fatalf("images = %+v", m.Images)
	}
	in.images = []string{"omp"}
	if _, err := in.build(); err == nil {
		t.Fatal("a malformed -image must fail")
	}
}
