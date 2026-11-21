package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RigelBuild/nebula/tools/nemo-move/internal/phase"
)

func TestWaitForOwner(t *testing.T) {
	t.Run("returns immediately when phase is owner", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "phase")
		if err := os.WriteFile(path, []byte(phase.Owner), 0600); err != nil {
			t.Fatal(err)
		}
		if err := waitForOwner(context.Background(), path, time.Hour); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("polls until phase becomes owner", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "phase")
		if err := os.WriteFile(path, []byte(phase.Blocked), 0600); err != nil {
			t.Fatal(err)
		}
		blocked := make(chan struct{})
		resume := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- waitForOwnerObserved(context.Background(), path, 10*time.Millisecond, func() {
				close(blocked)
				<-resume
			})
		}()
		<-blocked
		if err := os.WriteFile(path, []byte(phase.Owner), 0600); err != nil {
			t.Fatal(err)
		}
		close(resume)
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("waitForOwner() error = %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("waitForOwner did not return after phase became owner")
		}
	})
	t.Run("cancellation returns promptly while phase is blocked", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "phase")
		if err := os.WriteFile(path, []byte(phase.Blocked), 0600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		started := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			close(started)
			done <- waitForOwner(ctx, path, time.Hour)
		}()
		<-started
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("waitForOwner() error = %v, want context.Canceled", err)
			}
		case <-time.After(time.Second):
			t.Fatal("waitForOwner did not return after cancellation")
		}
	})
}

func TestMaintSideOperations(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		file string
		want string
	}{
		{name: "phase", args: []string{"phase-write", "owner"}, file: "phase", want: "owner"},
		{name: "cutover", args: []string{"cutover-write", "run-id", "started"}, file: "cutover.json", want: `{"runID":"run-id","state":"started"}`},
		{name: "restore", args: []string{"restore-write", "data", "control", "nonce"}, file: "restore-prepare.json", want: `{"backupData":"data","backupControl":"control","nonce":"nonce"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := maintSide(tc.args, io.Discard, dir); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(dir, tc.file))
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(string(got)) != tc.want {
				t.Fatalf("content = %q, want %q", got, tc.want)
			}
		})
	}
	t.Run("invalid phase refused", func(t *testing.T) {
		err := maintSide([]string{"phase-write", "faulted"}, io.Discard, t.TempDir())
		if err == nil || err.Error() != "phase-write requires owner or blocked" {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}
