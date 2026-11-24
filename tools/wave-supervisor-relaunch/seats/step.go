package seats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/cotalreach"
	"github.com/RigelBuild/nebula/tools/go-lib/relaunch"
	"github.com/RigelBuild/nebula/tools/go-lib/waveseats"
)

const (
	// ReadTimeout bounds each seat-step read, the same 30 s bound as a seat command.
	ReadTimeout = 30 * time.Second
	stopBound   = 60 * time.Second
	waitPoll    = time.Second
)

// Deps are the step's readers and commands. The caller wires the relauncher's runner into Stop,
// Restart and Holders, so there is one command runner and one stop path.
type Deps struct {
	FS          relaunch.FileSystem
	CgroupRoot  string
	Threshold   string
	ReadTimeout time.Duration
	Hold        func() bool
	SeatNames   func(context.Context) (map[string]struct{}, error)
	PS          func(context.Context) (string, error)
	Status      func(context.Context) (any, error)
	Holders     func(context.Context) (string, error)
	Processes   func() ([]Process, error)
	Stop        func(context.Context, string) error
	Restart     func(context.Context, string) (int, error)
	Now         func() time.Time
	Sleep       func(context.Context, time.Duration) error
	Log         func(string)
	SeatCLI     string
	// LiveSubagents reports a seat's live child sessions. Missing checks count as unknown/busy.
	LiveSubagents func(context.Context, string) (int, error)
}

// Run relaunches at most one idle non-root seat over the threshold. It logs every outcome and
// returns nothing, so the step cannot change run's exit code.
func Run(ctx context.Context, deps Deps) {
	threshold, err := ThresholdBytes(deps.Threshold)
	if err != nil {
		deps.Log(fmt.Sprintf("wave-supervisor-relaunch: seat outcome=failed reason=%q", err.Error()))
		return
	}
	names, err := bounded(ctx, deps.ReadTimeout, deps.SeatNames)
	if err != nil {
		deps.Log(fmt.Sprintf("wave-supervisor-relaunch: seat outcome=failed reason=%q", err.Error()))
		return
	}
	root := waveseats.ActiveRootNames(names)
	if root == "" {
		deps.Log("wave-supervisor-relaunch: seat outcome=skipped reason=root-seat-inactive")
		return
	}
	seats, err := deps.candidates(ctx, names)
	if err != nil {
		deps.Log(fmt.Sprintf("wave-supervisor-relaunch: seat outcome=failed reason=%q", err.Error()))
		return
	}
	step(ctx, root, seats, threshold, deps.Hold(), deps.sequence().relaunch, deps.Log)
}

func (deps Deps) sequence() sequence {
	return sequence{
		idleNow: func(ctx context.Context, name string) (bool, bool, error) {
			seats, err := deps.candidates(ctx, map[string]struct{}{name: {}})
			if err != nil {
				return false, false, err
			}
			if len(seats) != 1 {
				return false, false, nil
			}
			return seats[0].Idle, seats[0].Subagents, nil
		},
		stop:     deps.Stop,
		present:  deps.present,
		restart:  deps.Restart,
		now:      deps.Now,
		sleep:    deps.Sleep,
		deadline: stopBound,
		poll:     waitPoll,
		seatCLI:  deps.SeatCLI,
	}
}

// candidates lists each named seat with its scope memory and idle state. It walks the omp
// processes once.
func (deps Deps) candidates(ctx context.Context, names map[string]struct{}) ([]Candidate, error) {
	ps, err := bounded(ctx, deps.ReadTimeout, deps.PS)
	if err != nil {
		return nil, err
	}
	rosterIdle, err := rosterIdleNames(ps)
	if err != nil {
		return nil, err
	}
	procs, err := deps.Processes()
	if err != nil {
		return nil, err
	}
	memory := scopeMemory(procs, deps.FS, deps.CgroupRoot)
	candidates := make([]Candidate, 0, len(names))
	for _, name := range slices.Sorted(maps.Keys(names)) {
		candidate := Candidate{
			Name:        name,
			MemoryBytes: memory[name],
			Idle:        CountsIdle(rosterIdle[name]),
		}
		if candidate.Idle {
			candidate.Subagents = deps.subagentsBlock(ctx, name)
		}
		candidates = append(candidates, candidate)
	}
	return candidates, nil
}

// subagentsBlock treats a failed check as busy, never as zero.
func (deps Deps) subagentsBlock(ctx context.Context, name string) bool {
	if deps.LiveSubagents == nil {
		return true
	}
	count, err := deps.LiveSubagents(ctx, name)
	return err != nil || count > 0
}

// present is the presence test `wave-seats restart` applies (liveNames): ps rows, running
// holders, and retrying manager failures. complete is false when manager status is unavailable.
// A failed ps or holder read is an error, so the wait holds.
func (deps Deps) present(ctx context.Context, name string) (bool, bool, error) {
	ps, err := bounded(ctx, deps.ReadTimeout, deps.PS)
	if err != nil {
		return false, false, err
	}
	if _, _, valid := cotalreach.ParsePsItems(ps); !valid {
		return false, false, errors.New("invalid cotal ps envelope")
	}
	holders, err := bounded(ctx, deps.ReadTimeout, deps.Holders)
	if err != nil {
		return false, false, err
	}
	status, err := bounded(ctx, deps.ReadTimeout, deps.Status)
	if err != nil {
		status = nil
	}
	_, present := waveseats.PresentNames(ps, holders, status, []string{name})[name]
	return present, waveseats.ManagerStatusAvailable(status), nil
}

// rosterIdleNames returns verified idle seats from the current mesh view with explicit background
// idle state. Missing, null, and non-string background values are not idle.
func rosterIdleNames(ps string) (map[string]bool, error) {
	kind, rows, valid := cotalreach.ParsePsItems(ps)
	if !valid || kind != "seats" {
		return nil, fmt.Errorf("cotal ps returned no seats roster (kind %q)", kind)
	}
	idle := map[string]bool{}
	for _, row := range rows {
		var name, mesh, view string
		var verified bool
		if json.Unmarshal(row["name"], &name) != nil || json.Unmarshal(row["mesh"], &mesh) != nil ||
			json.Unmarshal(row["meshView"], &view) != nil || json.Unmarshal(row["presenceIdentityVerified"], &verified) != nil {
			continue
		}
		var background string
		if json.Unmarshal(row["background"], &background) != nil || background != "idle" {
			continue
		}
		if mesh == "idle" && view == "current" && verified {
			idle[name] = true
		}
	}
	return idle, nil
}

// bounded runs one read and returns once it finishes or the bound passes. The read is not awaited
// past the bound, so a read that ignores ctx cannot hold the run.
func bounded[T any](ctx context.Context, timeout time.Duration, read func(context.Context) (T, error)) (T, error) {
	readCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	type result struct {
		value T
		err   error
	}
	done := make(chan result, 1)
	go func() {
		value, err := read(readCtx)
		done <- result{value, err}
	}()
	select {
	case r := <-done:
		return r.value, r.err
	case <-readCtx.Done():
		var zero T
		return zero, fmt.Errorf("seat-step read gave up after %v: %w", timeout, readCtx.Err())
	}
}
