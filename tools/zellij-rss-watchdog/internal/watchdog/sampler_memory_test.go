package watchdog

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/RigelBuild/nebula/tools/zellij-rss-watchdog/internal/capture"
	"github.com/RigelBuild/nebula/tools/zellij-rss-watchdog/internal/command"
)

func TestMemoryRangesCopiedAndReducedOneAtATimeIncludingCleanup(t *testing.T) {
	io := newFakeIO()
	rec := &rangeRecorder{t: t}
	io.readTextFn = func(path string) (string, error) {
		if strings.HasSuffix(path, "/smaps") {
			return fourLargeMappings(), nil
		}
		return io.files[path], nil
	}
	io.copyFn = rec.copy(io)
	io.writeTextFn = rec.writeText(io)
	io.runFn = rec.run()
	io.removeFn = rec.remove
	must(t, capture.CaptureServer(context.Background(), io, "123:456", 2*gib))
	if rec.copies != 4 {
		t.Fatalf("copies = %d, want 4", rec.copies)
	}
	if rec.reductions != 3 {
		t.Fatalf("reductions = %d, want 3", rec.reductions)
	}
	want := []string{
		"copy-1", "reduce-1", "remove-0",
		"copy-2", "remove-1",
		"copy-3", "reduce-3", "remove-2",
		"copy-4", "reduce-4", "remove-3",
	}
	if !reflect.DeepEqual(rec.events, want) {
		t.Fatalf("events = %v, want %v", rec.events, want)
	}
	if !strings.Contains(rec.errorsText, "mapping-0: command exited with status 1") {
		t.Errorf("errors.txt missing mapping-0 failure: %q", rec.errorsText)
	}
	if !strings.Contains(rec.errorsText, "mapping-1: short proc memory read") {
		t.Errorf("errors.txt missing mapping-1 failure: %q", rec.errorsText)
	}
}

// rangeRecorder records the copy, reduce and remove order for the mapping test.
type rangeRecorder struct {
	t          *testing.T
	events     []string
	copies     int
	reductions int
	errorsText string
}

// fourLargeMappings is four 1.5 GiB anonymous rw mappings, each starting 0x70000000 apart.
func fourLargeMappings() string {
	lines := make([]string, 0, 4)
	for i := range 4 {
		start := 0x10000000 + i*0x70000000
		end := 0x70000000 + i*0x70000000
		lines = append(lines, fmt.Sprintf("%x-%x rw-p 00000000 00:00 0\nSize: 1572864 kB", start, end))
	}
	return strings.Join(lines, "\n")
}

func (r *rangeRecorder) copy(io *fakeIO) func(_ string, start, end uint64, outputPath, _ string) error {
	return func(_ string, start, end uint64, outputPath, _ string) error {
		r.copies++
		r.events = append(r.events, fmt.Sprintf("copy-%d", r.copies))
		wantStart := uint64(0x10000000) + uint64(r.copies-1)*0x70000000
		if start != wantStart {
			r.t.Errorf("copy %d start = %#x, want %#x", r.copies, start, wantStart)
		}
		if end != start+512<<20 {
			r.t.Errorf("copy %d end = %#x, want start+512MiB", r.copies, end)
		}
		io.files[outputPath] = "memory"
		if r.copies == 2 {
			return errors.New("short proc memory read")
		}
		return nil
	}
}

func (r *rangeRecorder) writeText(io *fakeIO) func(path, text string) error {
	return func(path, text string) error {
		if strings.HasSuffix(path, "errors.txt") {
			r.errorsText = text
		}
		io.files[path] = text
		return nil
	}
}

func (r *rangeRecorder) run() func(_ context.Context, name string, args []string, timeout time.Duration) (command.Result, error) {
	return func(_ context.Context, name string, args []string, timeout time.Duration) (command.Result, error) {
		switch name {
		case "ss", "runuser":
			return command.Result{}, nil
		case "bash":
			return r.reduce(args, timeout)
		}
		return command.Result{}, fmt.Errorf("unexpected command: %s", name)
	}
}

// reduce checks the reducer argv and fails the first reduction, as the scenario requires.
func (r *rangeRecorder) reduce(args []string, timeout time.Duration) (command.Result, error) {
	r.reductions++
	r.events = append(r.events, fmt.Sprintf("reduce-%d", r.copies))
	if timeout != command.MappingTimeout {
		r.t.Errorf("reduce timeout = %v, want %v", timeout, command.MappingTimeout)
	}
	if !strings.Contains(args[1], "set -o pipefail") || !strings.Contains(args[1], `sort -T "$2"`) {
		r.t.Errorf("reducer script = %q", args[1])
	}
	if !strings.HasPrefix(args[4], "/captures/") {
		r.t.Errorf("reducer dir = %q", args[4])
	}
	if r.reductions == 1 {
		return command.Result{}, errors.New("command exited with status 1; stderr=sort failed")
	}
	return command.Result{Stdout: "reduced"}, nil
}

func (r *rangeRecorder) remove(path string) error {
	if strings.HasSuffix(path, ".raw") {
		r.events = append(r.events, "remove-"+mappingIndex(path))
	}
	return nil
}

// mappingIndex extracts N from a ".../mapping-N.raw" path.
func mappingIndex(path string) string {
	start := strings.Index(path, "mapping-") + len("mapping-")
	end := strings.Index(path[start:], ".raw")
	return path[start : start+end]
}
