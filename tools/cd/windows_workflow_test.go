// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"strings"
	"testing"
)

// F1 (high): the Windows leg must not evaluate a devShell the flake does not
// define, and an empty systems list must not match the Windows leg.
func TestWindowsLegUsesLinuxDevShellAndExplicitFilter(t *testing.T) {
	wf := readWorkflow(t)
	if !strings.Contains(wf, "shell_system=x86_64-linux") {
		t.Fatal("cd.yaml: windows-amd64 does not map to the x86_64-linux devShell; the flake defines no windows devShell")
	}
	if !strings.Contains(wf, `$s != "windows-amd64"`) {
		t.Fatal("cd.yaml: an empty systems list still matches the windows-amd64 leg")
	}
}

// F2 (high): the release job must publish windows-amd64 assets to the Release.
func TestPublishStepIncludesWindowsSystem(t *testing.T) {
	wf := readWorkflow(t)
	start := strings.Index(wf, "- name: Publish release-manifest.json on main")
	end := strings.Index(wf, "- name: Tag the release commit")
	if start < 0 || end < start {
		t.Fatal("cd.yaml: could not locate the publish step")
	}
	if !strings.Contains(wf[start:end], "-system windows-amd64") {
		t.Fatal("cd.yaml: the publish step does not pass -system windows-amd64, so Windows assets never reach the Release")
	}
}
