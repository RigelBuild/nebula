package seats

import (
	"context"
	"fmt"
	"io/fs"
	"strings"
	"testing/fstest"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/relaunch"
)

var errReadOnly = fmt.Errorf("read-only fake: %w", fs.ErrPermission)

// mapFS adapts fstest.MapFS, which takes unrooted paths, to relaunch.FileSystem.
type mapFS struct{ files fstest.MapFS }

var _ relaunch.FileSystem = mapFS{}

func (m mapFS) ReadFile(path string) ([]byte, error) {
	return m.files.ReadFile(strings.TrimPrefix(path, "/"))
}
func (m mapFS) ReadDir(path string) ([]fs.DirEntry, error) {
	return m.files.ReadDir(strings.TrimPrefix(path, "/"))
}
func (m mapFS) Stat(path string) (fs.FileInfo, error) {
	return m.files.Stat(strings.TrimPrefix(path, "/"))
}
func (mapFS) Readlink(string) (string, error)             { return "", errReadOnly }
func (mapFS) WriteFile(string, []byte, fs.FileMode) error { return errReadOnly }
func (mapFS) MkdirAll(string, fs.FileMode) error          { return errReadOnly }
func (mapFS) Rename(string, string) error                 { return errReadOnly }
func (mapFS) Remove(string) error                         { return errReadOnly }

// seat describes one active seat in the fake world.
type seat struct {
	name       string
	gib        int64
	idle       bool // roster idle, verified, current view
	noProc     bool
	rosterAs   string // overrides the roster mesh value when set
	background bool   // roster shows background work running
}

// world is a fake wave: roster, /proc, cgroup memory, holders and manager status. Every command
// the step runs is recorded as argv in calls, the way a fake cotal and wave-seats would see it.
type world struct {
	seats           []seat
	files           fstest.MapFS
	calls           []string
	hold            bool
	threshold       string
	clock           time.Time
	logs            []string
	stopCode        int
	stopErr         error
	restartRC       int
	present         func(call int) (ps, holders string, status any)
	presenceN       int
	psErrAfter      error // ps read error once the seat is stopped
	holderErrAfter  error // holder read error once the seat is stopped
	psOverride      func(call int) string
	psCalls         int
	busyAfter       string // a seat whose roster turns working after the first ps read
	backgroundAfter string // a seat whose roster shows background running after the first ps read
	liveSubagents   int
	subagentErr     error
	probeCost       time.Duration
}

func newWorld(seats ...seat) *world {
	w := &world{seats: seats, files: fstest.MapFS{}, clock: time.UnixMilli(2_000_000)}
	for _, s := range seats {
		scope := "user.slice/wave-agents.slice/run-" + s.name + ".scope"
		w.files["cg/"+scope+"/memory.stat"] = &fstest.MapFile{Data: fmt.Appendf(nil, "anon %d\nfile %d\n", s.gib*gib, 40*gib)}
	}
	w.present = func(int) (string, string, any) { return psEnvelope(""), "", readyStatus() }
	return w
}

func readyStatus() map[string]any {
	return map[string]any{"state": "idle", "lastSweep": map[string]any{"completedAt": "t"}, "failures": []any{}}
}

func psEnvelope(rows string) string {
	return `{"schemaVersion":1,"command":"ps","outcome":"ok","result":{"kind":"seats","items":[` + rows + `]}}`
}

func (w *world) roster() string {
	rows := make([]string, 0, len(w.seats))
	for _, s := range w.seats {
		mesh := "working"
		if s.idle {
			mesh = "idle"
		}
		if s.rosterAs != "" {
			mesh = s.rosterAs
		}
		if w.busyAfter == s.name && w.psCalls > 1 {
			mesh = "working"
		}
		background := `,"background":"idle"`
		if s.background || (w.backgroundAfter == s.name && w.psCalls > 1) {
			background = `,"background":"running"`
		}
		rows = append(rows, fmt.Sprintf(`{"name":%q,"mesh":%q,"meshView":"current","presenceIdentityVerified":true%s}`, s.name, mesh, background))
	}
	return psEnvelope(strings.Join(rows, ","))
}

func (w *world) deps() Deps {
	stopped := false
	return Deps{
		FS: mapFS{w.files}, CgroupRoot: "/cg",
		Threshold: w.threshold, ReadTimeout: time.Second,
		Hold: func() bool { return w.hold },
		SeatNames: func(context.Context) (map[string]struct{}, error) {
			names := map[string]struct{}{}
			for _, s := range w.seats {
				names[s.name] = struct{}{}
			}
			return names, nil
		},
		PS: func(context.Context) (string, error) {
			w.psCalls++
			if stopped {
				if w.psErrAfter != nil {
					return "", w.psErrAfter
				}
				ps, _, _ := w.present(w.presenceN)
				return ps, nil
			}
			if w.psOverride != nil {
				return w.psOverride(w.psCalls), nil
			}
			return w.roster(), nil
		},
		Holders: func(context.Context) (string, error) {
			if stopped && w.holderErrAfter != nil {
				return "", w.holderErrAfter
			}
			_, holders, _ := w.present(w.presenceN)
			return holders, nil
		},
		Status: func(context.Context) (any, error) {
			w.clock = w.clock.Add(w.probeCost)
			_, _, status := w.present(w.presenceN)
			w.presenceN++
			return status, nil
		},
		Processes: func() ([]Process, error) {
			procs := []Process{}
			for _, s := range w.seats {
				if !s.noProc {
					procs = append(procs, Process{Name: s.name, Cgroup: "0::/user.slice/wave-agents.slice/run-" + s.name + ".scope\n"})
				}
			}
			return procs, nil
		},
		Stop: func(_ context.Context, name string) error {
			w.calls = append(w.calls, "cotal stop --name "+name+" --space main")
			stopped = true
			if w.stopErr != nil {
				return w.stopErr
			}
			if w.stopCode != 0 {
				return fmt.Errorf("cotal stop --name %s exited %d", name, w.stopCode)
			}
			return nil
		},
		Restart: func(_ context.Context, name string) (int, error) {
			w.calls = append(w.calls, "wave-seats restart "+name)
			return w.restartRC, nil
		},
		Now:           func() time.Time { return w.clock },
		Sleep:         func(_ context.Context, d time.Duration) error { w.clock = w.clock.Add(d); return nil },
		Log:           func(line string) { w.logs = append(w.logs, line) },
		LiveSubagents: func(context.Context, string) (int, error) { return w.liveSubagents, w.subagentErr },
		SeatCLI:       "wave-seats",
	}
}

func (w *world) run(ctx context.Context) { Run(ctx, w.deps()) }

func idleSeat(name string, gibs int64) seat {
	return seat{name: name, gib: gibs, idle: true}
}

func workingSeat(name string, gibs int64) seat {
	return seat{name: name, gib: gibs}
}

func outcomeLine(outcome, name string, gibs int64) string {
	return fmt.Sprintf("wave-supervisor-relaunch: seat outcome=%s seat=%s anon_bytes=%d", outcome, name, gibs*gib)
}
