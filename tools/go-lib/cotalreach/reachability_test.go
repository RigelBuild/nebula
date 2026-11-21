package cotalreach

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestBuildPsArgs(t *testing.T) {
	t.Run("requests machine-readable presence for the main space", func(t *testing.T) {
		if got := BuildPsArgs(); !reflect.DeepEqual(got, []string{"ps", "--space", "main", "--json"}) {
			t.Fatalf("args = %v", got)
		}
	})
}

func TestParsePresentAgentNames(t *testing.T) {
	const rows = `{"name":"alpha","mesh":"working"}, {"name":"bravo","mesh":"waiting"}`
	tests := []struct {
		name     string
		input    string
		expected []string
		absent   string
	}{
		{name: "reads named agents in present mesh states", input: rows, expected: []string{"alpha", "bravo"}},
		{name: "does not treat offline agent as present", input: `{"name":"alpha","mesh":"offline"}`},
		{name: "does not treat stale mesh view as present", input: `{"name":"alpha","mesh":"working","meshView":"stale"}`},
		{name: "accepts current mesh views", input: `{"name":"alpha","mesh":"idle","meshView":"current"}, {"name":"bravo","mesh":"waiting"}`, expected: []string{"alpha", "bravo"}},
		{name: "ignores non-string fields", input: rows + `, {"name":3,"mesh":"working"}, {"name":"ghost","mesh":3}`, expected: []string{"alpha", "bravo"}},
		{name: "uses exact name", input: `{"name":"alpha-2","mesh":"working"}`, expected: []string{"alpha-2"}, absent: "alpha"},
		{name: "rejects null mesh view", input: `{"name":"alpha","mesh":"working","meshView":null}`},
		{name: "ignores null names", input: `{"name":null,"mesh":"working"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertPresentNames(t, test)
		})
	}
}

func assertPresentNames(t *testing.T, test struct {
	name     string
	input    string
	expected []string
	absent   string
}) {
	t.Helper()
	got := ParsePresentAgentNames(`{"schemaVersion":1,"command":"ps","outcome":"ok","result":{"kind":"seats","items":[` + test.input + `]}}`)
	if len(got) != len(test.expected) && test.absent == "" {
		t.Fatalf("names = %v, want %v", got, test.expected)
	}
	for _, name := range test.expected {
		if _, ok := got[name]; !ok {
			t.Errorf("missing %q in %v", name, got)
		}
	}
	if _, ok := got[test.absent]; test.absent != "" && ok {
		t.Errorf("unexpectedly contains %q in %v", test.absent, got)
	}
}

func TestBuildDmArgs(t *testing.T) {
	t.Run("sends the reachability ping to the named agent in main", func(t *testing.T) {
		want := []string{"send", "--to", "alpha", "spawn-agent reachability check 7f3a", "--space", "main"}
		if got := BuildDmArgs("alpha", "spawn-agent reachability check 7f3a"); !reflect.DeepEqual(got, want) {
			t.Fatalf("args = %v, want %v", got, want)
		}
	})
}

func TestWaitForAgentPresence(t *testing.T) {
	t.Run("returns once the exact agent has a mesh status", func(t *testing.T) {
		calls := 0
		err := WaitForAgentPresence(t.Context(), "alpha", PresenceOptions{Timeout: time.Second, Poll: time.Millisecond, ReadPS: func(context.Context) (string, error) {
			calls++
			if calls == 1 {
				return `{"schemaVersion":1,"command":"ps","outcome":"ok","result":{"kind":"seats","items":[{"name":"alpha","mesh":"absent"}]}}`, nil
			}
			return `{"schemaVersion":1,"command":"ps","outcome":"ok","result":{"kind":"seats","items":[{"name":"alpha","mesh":"working"}]}}`, nil
		}, Sleep: func(context.Context, time.Duration) error { return nil }})
		if err != nil || calls != 2 {
			t.Fatalf("error = %v, calls = %d", err, calls)
		}
	})
	t.Run("times out when the exact agent never appears", func(t *testing.T) {
		now := time.Unix(0, 0)
		err := WaitForAgentPresence(t.Context(), "alpha", PresenceOptions{Timeout: 2 * time.Millisecond, Poll: time.Millisecond, ReadPS: func(context.Context) (string, error) { return `{"name":"alpha-2"}`, nil }, Now: func() time.Time { return now }, Sleep: func(_ context.Context, delay time.Duration) error { now = now.Add(delay); return nil }})
		if err == nil || !strings.Contains(err.Error(), "alpha did not appear in cotal ps") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestWaitForAgentPresencePropagatesReaderAndSleepErrors(t *testing.T) {
	readerErr := errors.New("ps failed")
	t.Run("reader error", func(t *testing.T) {
		err := WaitForAgentPresence(t.Context(), "alpha", PresenceOptions{Timeout: time.Second, Poll: time.Millisecond, ReadPS: func(context.Context) (string, error) { return "", readerErr }})
		if !errors.Is(err, readerErr) {
			t.Fatalf("error = %v, want %v", err, readerErr)
		}
	})
	sleepErr := errors.New("sleep cancelled")
	t.Run("sleep error", func(t *testing.T) {
		err := WaitForAgentPresence(t.Context(), "alpha", PresenceOptions{Timeout: time.Second, Poll: time.Millisecond, ReadPS: func(context.Context) (string, error) { return `{"name":"alpha-2","mesh":"working"}`, nil }, Sleep: func(context.Context, time.Duration) error { return sleepErr }})
		if !errors.Is(err, sleepErr) {
			t.Fatalf("error = %v, want %v", err, sleepErr)
		}
	})
}

func TestVerifyAgentReachability(t *testing.T) {
	const present = `{"schemaVersion":1,"command":"ps","outcome":"ok","result":{"kind":"seats","items":[{"name":"alpha","mesh":"working"}]}}`
	t.Run("polls Cotal presence before sending the DM reachability ping", func(t *testing.T) {
		calls := [][]string{}
		psCalls := 0
		err := VerifyAgentReachability(t.Context(), "alpha", func(_ context.Context, args []string) (CommandResult, error) {
			calls = append(calls, args)
			if args[0] == "ps" {
				psCalls++
				if psCalls == 1 {
					return CommandResult{ExitCode: 0}, nil
				}
				return CommandResult{ExitCode: 0, Stdout: present}, nil
			}
			return CommandResult{ExitCode: 0}, nil
		}, VerifyOptions{Timeout: time.Second, Poll: time.Millisecond, Sleep: func(context.Context, time.Duration) error { return nil }})
		want := [][]string{{"ps", "--space", "main", "--json"}, {"ps", "--space", "main", "--json"}, {"send", "--to", "alpha", "spawn-agent reachability check alpha", "--space", "main"}}
		if err != nil || !reflect.DeepEqual(calls, want) {
			t.Fatalf("error = %v, calls = %v", err, calls)
		}
	})
	t.Run("reports a failed DM command with its stderr", func(t *testing.T) {
		err := VerifyAgentReachability(t.Context(), "alpha", func(_ context.Context, args []string) (CommandResult, error) {
			if args[0] == "ps" {
				return CommandResult{ExitCode: 0, Stdout: present}, nil
			}
			return CommandResult{ExitCode: 1, Stderr: "no agent alpha present"}, nil
		}, VerifyOptions{Timeout: time.Second, Poll: time.Millisecond})
		if err == nil || err.Error() != "cotal send exited 1: no agent alpha present" {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestRunCotal(t *testing.T) {
	t.Run("returns nonzero exit with stderr", func(t *testing.T) {
		_, err := RunCotal(t.Context(), []string{"send"}, func(context.Context, []string) (CommandResult, error) {
			return CommandResult{ExitCode: 1, Stderr: "offline"}, nil
		})
		if err == nil || !strings.Contains(err.Error(), "cotal send exited 1: offline") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("passes through runner errors", func(t *testing.T) {
		want := errors.New("failed")
		_, err := RunCotal(t.Context(), []string{"ps"}, func(context.Context, []string) (CommandResult, error) { return CommandResult{}, want })
		if !errors.Is(err, want) {
			t.Fatalf("error = %v", err)
		}
	})
}
