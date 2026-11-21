package main

import "testing"

func TestStatusEvidenceRequiresCurrentIdentity(t *testing.T) {
	rec := HookRecord{Repo: "example/app", Head: "head", Base: "base", HookConfigBlob: "blob", Result: hookPass}
	for _, tc := range []struct {
		name     string
		identity HookRecord
		current  bool
	}{
		{name: "current", identity: rec, current: true},
		{name: "stale base", identity: HookRecord{Repo: rec.Repo, Head: rec.Head, Base: "other", HookConfigBlob: rec.HookConfigBlob}},
		{name: "stale blob", identity: HookRecord{Repo: rec.Repo, Head: rec.Head, Base: rec.Base, HookConfigBlob: "other"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := statusEvidenceCurrent(rec, tc.identity); got != tc.current {
				t.Fatalf("current=%v want %v", got, tc.current)
			}
		})
	}
}
