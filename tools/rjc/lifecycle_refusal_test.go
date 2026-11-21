package main

import (
	"strings"
	"testing"
)

func TestLifecycleReadyRefusals(t *testing.T) {
	l := newLifecycleFixture(t)
	store := localPrePush{root: l.f.repo.stateDir}
	original, found, err := store.Read(l.f.ctx, l.f.repo.profile.Name, 1, l.head)
	if err != nil || !found {
		t.Fatalf("record: %v %v", found, err)
	}
	for _, tc := range []struct {
		name                                 string
		markerAuthor, markerHead, markerBase string
		result                               HookResult
		skip                                 string
		staleBase, staleBlob                 bool
		gate                                 string
	}{
		{name: "missing marker", gate: "all-clear"},
		{name: "old marker", markerAuthor: "agent", markerHead: strings.Repeat("a", 40), markerBase: "main", gate: "all-clear"},
		{name: "other author", markerAuthor: "other", markerHead: l.head, markerBase: "main", gate: "all-clear"},
		{name: "changed base", markerAuthor: "agent", markerHead: l.head, markerBase: "other", gate: "all-clear"},
		{name: "fail", markerAuthor: "agent", markerHead: l.head, markerBase: "main", result: hookFail, gate: "hooks-not-passed"},
		{name: "fixup", markerAuthor: "agent", markerHead: l.head, markerBase: "main", result: hookFixup, gate: "hooks-not-passed"},
		{name: "absent", markerAuthor: "agent", markerHead: l.head, markerBase: "main", result: "absent", gate: "hooks-not-passed"},
		{name: "skipped", markerAuthor: "agent", markerHead: l.head, markerBase: "main", result: hookSkipped, skip: "main-red:#1", gate: "hooks-skipped"},
		{name: "partial", markerAuthor: "agent", markerHead: l.head, markerBase: "main", result: hookPartial, gate: "hooks-skipped"},
		{name: "stale base", markerAuthor: "agent", markerHead: l.head, markerBase: "main", result: hookPass, staleBase: true, gate: "hooks-not-passed"},
		{name: "stale config", markerAuthor: "agent", markerHead: l.head, markerBase: "main", result: hookPass, staleBlob: true, gate: "hooks-not-passed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l.comments = nil
			if tc.markerAuthor != "" {
				l.clear(tc.markerAuthor, tc.markerHead, tc.markerBase)
			}
			rec := original
			if tc.result != "" {
				rec.Result = tc.result
			}
			if tc.skip != "" {
				rec.Skip = tc.skip
			}
			if tc.staleBase {
				rec.Base = strings.Repeat("b", 40)
			}
			if tc.staleBlob {
				rec.HookConfigBlob = strings.Repeat("c", 40)
			}
			if err := store.Save(rec); err != nil {
				t.Fatal(err)
			}
			code, out := l.command(t, "ready", l.bookmark)
			if code != 1 || !strings.Contains(out, tc.gate) || l.promotions != 0 {
				t.Fatalf("code=%d output=%s promotions=%d", code, out, l.promotions)
			}
		})
	}
}
