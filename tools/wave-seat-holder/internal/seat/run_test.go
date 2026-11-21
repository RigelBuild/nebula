package seat

import (
	"context"
	"testing"
)

func TestAttachAttachesInReadWriteModeByDefault(t *testing.T) {
	fake := fakeDeps(fakeOptions{})
	assertDeepEqual(t, Run(context.Background(), []string{"attach", "--name", "seat"}, fake.deps), 0)
	assertDeepEqual(t, fake.calls, []recordedCall{{
		command: "tmux",
		args:    []string{"-S", testSocket, "attach", "-t", "seat"},
		inherit: true,
	}})
}

func TestAttachAddsDashRForReadOnlyAttachment(t *testing.T) {
	fake := fakeDeps(fakeOptions{})
	assertDeepEqual(t, Run(context.Background(), []string{"attach", "--name", "seat", "--read-only"}, fake.deps), 0)
	assertDeepEqual(t, fake.calls, []recordedCall{{
		command: "tmux",
		args:    []string{"-S", testSocket, "attach", "-r", "-t", "seat"},
		inherit: true,
	}})
}

func TestInterruptSendsCtrlCToNamedSeat(t *testing.T) {
	fake := fakeDeps(fakeOptions{})
	assertDeepEqual(t, Run(context.Background(), []string{"interrupt", "--name", "seat"}, fake.deps), 0)
	assertDeepEqual(t, fake.calls, []recordedCall{{
		command: "tmux",
		args:    []string{"-S", testSocket, "send-keys", "-t", "seat", "C-c"},
	}})
}

func TestListReportsOnlySocketsThatAcceptConnections(t *testing.T) {
	const liveSocket = "/run/test-seats/live.sock"
	fake := fakeDeps(fakeOptions{
		socketNames:     []string{"live.sock", "dead.sock", "notes.txt"},
		liveSocketPaths: []string{liveSocket},
	})
	assertDeepEqual(t, Run(context.Background(), []string{"list", "--json"}, fake.deps), 0)
	assertDeepEqual(t, fake.output, []string{"{\"name\":\"live\",\"status\":\"running\"}\n"})
}

func TestListExitsZeroWithNoOutputWhenTheSocketDirectoryIsMissing(t *testing.T) {
	fake := fakeDeps(fakeOptions{})
	assertDeepEqual(t, Run(context.Background(), []string{"list", "--json"}, fake.deps), 0)
	assertDeepEqual(t, fake.output, []string(nil))
	assertDeepEqual(t, fake.errors, []string(nil))
}

func TestUsageErrorsExitTwoWithTheTSMessage(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "no command", args: nil, want: usageRoot},
		{name: "unknown command", args: []string{"bogus"}, want: usageRoot},
		{name: "start without separator", args: []string{"start", "--name", "seat", "omp"}, want: usageStart},
		{name: "attach with a bad option", args: []string{"attach", "--name", "seat", "--bogus"}, want: usageAttach},
		{name: "interrupt with extra argument", args: []string{"interrupt", "--name", "seat", "extra"}, want: usageInterrupt},
		{name: "list without --json", args: []string{"list"}, want: usageList},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := fakeDeps(fakeOptions{})
			assertDeepEqual(t, Run(context.Background(), test.args, fake.deps), 2)
			assertDeepEqual(t, fake.errors, []string{test.want})
		})
	}
}
