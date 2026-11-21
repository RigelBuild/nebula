package main

import (
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"testing"

	"github.com/RigelBuild/nebula/tools/nemo-move/internal/transfer"
)

type fakeCutover struct {
	events                  []string
	state, id, phase, fault string
	lostAck                 bool
}

func (f *fakeCutover) event(s string)              { f.events = append(f.events, s) }
func (f *fakeCutover) open(context.Context) error  { f.event("open"); return nil }
func (f *fakeCutover) close(context.Context) error { f.event("close"); return nil }
func (f *fakeCutover) record(context.Context) (string, string, error) {
	f.event("record")
	return f.id, f.state, nil
}
func (f *fakeCutover) readPhase(context.Context) (string, error) {
	f.event("read-phase")
	return f.phase, nil
}
func (f *fakeCutover) writePhase(_ context.Context, value string) error {
	f.event("phase:" + value)
	f.phase = value
	if value == "blocked" && f.fault == "phase-not-blocked" {
		f.phase = "unexpected"
	}
	if value == "owner" && f.lostAck {
		return errors.New("lost acknowledgement")
	}
	return nil
}
func (f *fakeCutover) writeRecord(_ context.Context, id, state string) error {
	f.event("record:" + state)
	f.id, f.state = id, state
	return nil
}
func (f *fakeCutover) stop(context.Context, string) error   { f.event("stop"); return nil }
func (f *fakeCutover) export(context.Context, string) error { f.event("export"); return nil }
func (f *fakeCutover) extract(_ context.Context, fault func() error) error {
	f.event("extract")
	if f.fault == "extract" {
		return errors.New("extract failed")
	}
	return fault()
}
func (f *fakeCutover) verify(context.Context) error {
	f.event("verify")
	if f.fault == "verify" {
		return errors.New("verify failed")
	}
	return nil
}
func (f *fakeCutover) cleanup(context.Context, string) error { f.event("cleanup"); return nil }
func (f *fakeCutover) inject(point string) error {
	if f.fault == point {
		return errors.New(point)
	}
	return nil
}

func TestCutover(t *testing.T) {
	args := []string{"--kubeconfig", "config", "--mac", "example@192.0.2.10", "--home-root", "/Users/example", "--ssh-user", "example", "--launchd-label", "com.example.agent:gui", "--mac-binary", "mac-binary"}
	for _, tc := range []struct {
		name, phase, state, resume string
		want                       []string
	}{
		{"owner refusal", "owner", "", "", []string{"open", "record", "read-phase", "close"}},
		{"committing refusal", "blocked", "committing", "", []string{"open", "record", "close"}},
		{"wrong resume", "blocked", "committing", "wrong", []string{"open", "record", "close"}},
		{"resume", "blocked", "committing", "run", []string{"open", "record", "read-phase", "phase:owner", "read-phase", "record:committed", "close"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeCutover{id: "run", state: tc.state, phase: tc.phase}
			callArgs := append([]string(nil), args...)
			if tc.resume != "" {
				callArgs = append(callArgs, "--resume", tc.resume)
			}
			err := cutover(t.Context(), callArgs, cutoverDeps{ops: f, newID: func() (string, error) { return "new", nil }, fault: f.inject}, io.Discard, io.Discard)
			if (err == nil) != (tc.name == "resume") {
				t.Fatalf("error = %v", err)
			}
			if !reflect.DeepEqual(f.events, tc.want) {
				t.Fatalf("events = %v, want %v", f.events, tc.want)
			}
		})
	}
}

func TestCutoverNormalAndFaults(t *testing.T) {
	args := []string{"--kubeconfig", "config", "--mac", "example@192.0.2.10", "--home-root", "/Users/example", "--ssh-user", "example", "--launchd-label", "com.example.agent:gui", "--mac-binary", "mac-binary"}
	for _, point := range []string{"", "before-extract", "mid-extract", "after-verify-before-commit", "after-phase-write-before-ack"} {
		t.Run(point, func(t *testing.T) {
			f := &fakeCutover{phase: "blocked", fault: point}
			err := cutover(t.Context(), args, cutoverDeps{ops: f, newID: func() (string, error) { return "new", nil }, fault: f.inject}, io.Discard, io.Discard)
			if point == "" {
				want := []string{"open", "record", "read-phase", "stop", "export", "record:started", "phase:blocked", "record", "read-phase", "extract", "verify", "record:committing", "phase:owner", "read-phase", "record:committed", "cleanup", "close"}
				if err != nil || !reflect.DeepEqual(f.events, want) {
					t.Fatalf("error %v, events %v", err, f.events)
				}
			} else if point == "after-phase-write-before-ack" {
				if err != nil || f.phase != "owner" || f.state != "committed" {
					t.Fatalf("error %v phase %s record %s", err, f.phase, f.state)
				}
			} else if err == nil || f.phase != "blocked" || f.state != "started" {
				t.Fatalf("error %v phase %s record %s", err, f.phase, f.state)
			}
		})
	}
}

func TestCutoverTransferFailureStopsCommit(t *testing.T) {
	args := []string{"--kubeconfig", "config", "--mac", "example@192.0.2.10", "--home-root", "/Users/example", "--ssh-user", "example", "--launchd-label", "com.example.agent:gui", "--mac-binary", "mac-binary"}
	for _, tc := range []struct {
		failure string
		want    []string
	}{
		{"extract", []string{"open", "record", "read-phase", "stop", "export", "record:started", "phase:blocked", "record", "read-phase", "extract"}},
		{"verify", []string{"open", "record", "read-phase", "stop", "export", "record:started", "phase:blocked", "record", "read-phase", "extract", "verify"}},
	} {
		t.Run(tc.failure, func(t *testing.T) {
			f := &fakeCutover{phase: "blocked", fault: tc.failure}
			err := cutover(t.Context(), args, cutoverDeps{ops: f, newID: func() (string, error) { return "new", nil }, fault: f.inject}, io.Discard, io.Discard)
			if err == nil || !reflect.DeepEqual(f.events, tc.want) {
				t.Fatalf("error %v, events %v, want %v", err, f.events, tc.want)
			}
		})
	}
}

func TestCutoverLostAcknowledgement(t *testing.T) {
	f := &fakeCutover{id: "run", state: "committing", phase: "blocked", lostAck: true}
	err := cutover(t.Context(), []string{"--kubeconfig", "config", "--mac", "example@192.0.2.10", "--home-root", "/Users/example", "--ssh-user", "example", "--launchd-label", "com.example.agent:gui", "--mac-binary", "mac-binary", "--resume", "run"}, cutoverDeps{ops: f, fault: f.inject}, io.Discard, io.Discard)
	if err != nil || f.state != "committed" {
		t.Fatalf("error %v state %s", err, f.state)
	}
	want := []string{"open", "record", "read-phase", "phase:owner", "read-phase", "record:committed", "close"}
	if !reflect.DeepEqual(f.events, want) {
		t.Fatalf("events = %v", f.events)
	}
}

func TestCutoverRequiresMacBinary(t *testing.T) {
	f := &fakeCutover{phase: "blocked"}
	err := cutover(t.Context(), []string{"--kubeconfig", "config", "--mac", "example@192.0.2.10", "--home-root", "/Users/example", "--ssh-user", "example", "--launchd-label", "com.example.agent:gui"}, cutoverDeps{ops: f}, io.Discard, io.Discard)
	if err == nil || len(f.events) != 0 {
		t.Fatalf("error %v, events %v", err, f.events)
	}
}

func TestCutoverRefusesExtractWhenPhaseNotBlocked(t *testing.T) {
	f := &fakeCutover{phase: "blocked", fault: "phase-not-blocked"}
	err := cutover(t.Context(), []string{"--kubeconfig", "config", "--mac", "example@192.0.2.10", "--home-root", "/Users/example", "--ssh-user", "example", "--launchd-label", "com.example.agent:gui", "--mac-binary", "mac-binary"}, cutoverDeps{ops: f, newID: func() (string, error) { return "new", nil }}, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("extract accepted a non-blocked phase")
	}
	for _, event := range f.events {
		if event == "extract" {
			t.Fatal("extract called with non-blocked phase")
		}
	}
}

func TestArchiveSpoolPrivateAndRemoved(t *testing.T) {
	for _, tc := range []struct{ name string }{{"per-run spool"}} {
		t.Run(tc.name, func(t *testing.T) {
			p := &productionCutover{}
			if err := p.prepareArchiveSpool(); err != nil {
				t.Fatal(err)
			}
			dir := p.archiveDir
			info, err := os.Stat(dir)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o700 {
				t.Fatalf("mode = %o", info.Mode().Perm())
			}
			if err := p.removeArchive(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("spool remains: %v", err)
			}
		})
	}
}

func TestProductionCutoverExtractSendsSourceHome(t *testing.T) {
	archive, err := os.CreateTemp(t.TempDir(), "archive-*.tar")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := archive.Close(); err != nil {
			t.Errorf("close archive: %v", err)
		}
	})
	var got []string
	run := func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		got = args
		return []byte("{}"), nil
	}
	p := &productionCutover{
		ctl:     controlPath{run: run, namespace: "nemo"},
		source:  transfer.Options{SourceHome: "/Users/example", MacUser: "example-user"},
		archive: archive,
	}
	if err := p.extract(t.Context(), func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	want := []string{"-n", "nemo", "exec", "-i", maintName, "--", "nemo-move", "maint-side", "extract", "/Users/example"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("extract argv = %v, want %v", got, want)
	}
}
