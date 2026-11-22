package main

import (
	"testing"
)

// TestWaveSliceNameComesFromConfig pins the cgroup match to the configured slice. The processes
// sit under x.slice and under wave-agents.slice. Configured to x.slice, only the x.slice process
// is in the wave. A literal wave-agents.slice match fails this test.
func TestWaveSliceNameComesFromConfig(t *testing.T) {
	fsys := newMemoryFS()
	fsys.files["/proc-fixture/41/cgroup"] = []byte("0::/user.slice/x.slice/scope.scope")
	fsys.files["/proc-fixture/41/cmdline"] = []byte("omp\x00")
	fsys.files["/proc-fixture/42/cgroup"] = []byte("0::/user.slice/wave-agents.slice/scope.scope")
	fsys.files["/proc-fixture/42/cmdline"] = []byte("omp\x00")

	pids, err := listSliceOmpPIDs("/proc-fixture", fsys, "x.slice")
	if err != nil || len(pids) != 1 || pids[0] != "41" {
		t.Fatalf("listSliceOmpPIDs(x.slice) = %v, %v; want [41]", pids, err)
	}
	if !inWaveSlice(fsys, "/proc-fixture", "41", "x.slice") {
		t.Fatal("inWaveSlice(41, x.slice) = false; a process under /x.slice/ must match")
	}
	if inWaveSlice(fsys, "/proc-fixture", "42", "x.slice") {
		t.Fatal("inWaveSlice(42, x.slice) = true; a process under /wave-agents.slice/ must not match")
	}
}
