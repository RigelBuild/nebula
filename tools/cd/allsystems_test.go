// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"slices"
	"testing"
)

// repoRoot is the nebula repository root, two levels above tools/cd.
const repoRoot = "../.."

// missingSystems lists the default release systems a package does not target. A package
// with no systems list targets every default system, so it misses none.
func missingSystems(b Binary) []string {
	if len(b.Systems) == 0 {
		return nil
	}
	var out []string
	for _, system := range defaultSystems {
		if !slices.Contains(b.Systems, system) {
			out = append(out, system)
		}
	}
	return out
}

// TestEveryBinaryPackageReleasesAllThreeSystems reads every tools/* and forks/*
// component.cue through the planner and requires each binary package to target
// all three default release systems, apart from the two exact exemptions below.
func TestEveryBinaryPackageReleasesAllThreeSystems(t *testing.T) {
	binaries, err := listBinaries(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(binaries) == 0 {
		t.Fatal("no binary packages found: the test would pass on an empty repository")
	}
	for _, b := range binaries {
		if b.Package == "fleet-gc-roots" || b.Package == "windows-agent-sync" {
			continue
		}
		if missing := missingSystems(b); len(missing) > 0 {
			t.Errorf("%s (%s) omits %v: systems = %v", b.Package, b.Project, missing, b.Systems)
		}
	}
}

// TestNarrowReleaseSystemDeclarationsAreExact pins the two intentional limits
// in the production component.cue data.
func TestNarrowReleaseSystemDeclarationsAreExact(t *testing.T) {
	binaries, err := listBinaries(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		packageName string
		systems     []string
	}{
		{packageName: "fleet-gc-roots", systems: []string{"x86_64-linux", "aarch64-linux"}},
		{packageName: "windows-agent-sync", systems: []string{"windows-amd64"}},
	}
	for _, tt := range tests {
		t.Run(tt.packageName, func(t *testing.T) {
			found := false
			for _, b := range binaries {
				if b.Package != tt.packageName {
					continue
				}
				found = true
				if !slices.Equal(b.Systems, tt.systems) {
					t.Errorf("%s systems = %v, want exactly %v", b.Package, b.Systems, tt.systems)
				}
			}
			if !found {
				t.Fatalf("production binary package %q not found", tt.packageName)
			}
		})
	}
}

// TestMissingSystemsReportsEachNarrowedSystem is the failing case the check
// above exists for: a package that names only some systems is reported as
// missing the rest, and a package with no list is not.
func TestMissingSystemsReportsEachNarrowedSystem(t *testing.T) {
	got := missingSystems(Binary{Package: "narrow", Systems: []string{"x86_64-linux"}})
	want := []string{"aarch64-linux", "aarch64-darwin"}
	if !slices.Equal(got, want) {
		t.Fatalf("missingSystems = %v, want %v", got, want)
	}
	if got := missingSystems(Binary{Package: "all"}); len(got) != 0 {
		t.Fatalf("a package with no systems list reported missing %v", got)
	}
}
