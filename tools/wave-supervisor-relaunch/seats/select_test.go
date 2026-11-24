package seats

import (
	"testing"
	"testing/fstest"
)

func TestThresholdBytes(t *testing.T) {
	if got, err := ThresholdBytes(""); err != nil || got != 9*gib {
		t.Fatalf("default = %d, %v", got, err)
	}
	if got, err := ThresholdBytes("4"); err != nil || got != 4*gib {
		t.Fatalf("override = %d, %v", got, err)
	}
	for _, bad := range []string{"0", "-1", "9.5", "lots", "99999999999"} {
		if _, err := ThresholdBytes(bad); err == nil {
			t.Fatalf("ThresholdBytes(%q) accepted", bad)
		}
	}
}

func TestCountsIdle(t *testing.T) {
	for _, tc := range []struct {
		name       string
		rosterIdle bool
		want       bool
	}{
		{name: "roster idle", rosterIdle: true, want: true},
		{name: "roster not idle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CountsIdle(tc.rosterIdle); got != tc.want {
				t.Fatalf("CountsIdle(%v) = %v, want %v", tc.rosterIdle, got, tc.want)
			}
		})
	}
}

func TestRunLeftSeatsFree(t *testing.T) {
	for _, reason := range []string{"hold", "root-seat-inactive", "starting", "manager-stopping", "restart-not-needed", "unchanged"} {
		if !RunLeftSeatsFree("skipped", reason) {
			t.Fatalf("skipped/%s blocked seats", reason)
		}
	}
	for _, tc := range [][2]string{{"relaunched", "absent"}, {"notified", "x"}, {"failed", "budget"}, {"skipped", "viewer-restarted"}, {"skipped", "a-new-reason"}} {
		if RunLeftSeatsFree(tc[0], tc[1]) {
			t.Fatalf("%s/%s freed seats", tc[0], tc[1])
		}
	}
}

func TestRosterIdleNamesIdentityChecks(t *testing.T) {
	ps := psEnvelope(`{"name":"web","mesh":"idle","meshView":"current","presenceIdentityVerified":true,"background":"idle"},` +
		`{"name":"unverified","mesh":"idle","meshView":"current","presenceIdentityVerified":false,"background":"idle"},` +
		`{"name":"noflag","mesh":"idle","meshView":"current","background":"idle"},` +
		`{"name":"stale","mesh":"idle","meshView":"stale","presenceIdentityVerified":true,"background":"idle"},` +
		`{"name":"waiting","mesh":"waiting","meshView":"current","presenceIdentityVerified":true,"background":"idle"}`)
	idle, err := rosterIdleNames(ps)
	if err != nil || len(idle) != 1 || !idle["web"] {
		t.Fatalf("idle = %v, %v; want only web", idle, err)
	}
	if _, err := rosterIdleNames("not json"); err == nil {
		t.Fatal("unreadable roster accepted")
	}
}

func TestRosterIdleNamesBackground(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra string
		idle  bool
	}{
		{name: "explicit idle background is idle", extra: `,"background":"idle"`, idle: true},
		{name: "running background is not idle", extra: `,"background":"running"`},
		{name: "other string background is not idle", extra: `,"background":"working"`},
		{name: "empty background is not idle", extra: `,"background":""`},
		{name: "null background is not idle", extra: `,"background":null`},
		{name: "non-string background is not idle", extra: `,"background":3`},
		{name: "absent background is not idle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ps := psEnvelope(`{"name":"web","mesh":"idle","meshView":"current","presenceIdentityVerified":true` + tc.extra + `}`)
			idle, err := rosterIdleNames(ps)
			if err != nil || idle["web"] != tc.idle {
				t.Fatalf("idle = %v, %v; want web idle=%v", idle, err, tc.idle)
			}
		})
	}
}

func TestScopeMemoryReadsAnon(t *testing.T) {
	files := fstest.MapFS{
		"cg/a.scope/memory.stat":    &fstest.MapFile{Data: []byte("anon 11\nfile 29\n")},
		"cg/a.scope/memory.current": &fstest.MapFile{Data: []byte("40\n")},
		"cg/b.scope/memory.current": &fstest.MapFile{Data: []byte("40\n")},
	}
	procs := []Process{{Name: "accounts", Cgroup: "0::/a.scope\n"}, {Name: "docs", Cgroup: "0::/b.scope\n"}, {Name: "web", Cgroup: ""}}
	memory := scopeMemory(procs, mapFS{files}, "/cg")
	if len(memory) != 1 || memory["accounts"] != 11 {
		t.Fatalf("memory = %v, want accounts=11 only", memory)
	}
}
