package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type commandCall struct {
	dir  string
	name string
	args []string
}

type fakeRunner struct {
	calls         []commandCall
	fetchErr      error
	checkoutErr   error
	gateOutput    []byte
	gateErr       error
	cotalErr      error
	wakeTexts     []string
	markerPath    string
	markAfter     string
	checkoutCalls int
	gatewayCalls  int
}

func (runner *fakeRunner) Run(
	ctx context.Context,
	dir string,
	name string,
	args ...string,
) ([]byte, error) {
	runner.calls = append(runner.calls, commandCall{
		dir:  dir,
		name: name,
		args: append([]string{}, args...),
	})
	mark := name == runner.markAfter
	if name == "git" && len(args) > 2 {
		mark = args[2] == runner.markAfter
	}
	if mark && runner.markerPath != "" {
		if err := os.WriteFile(runner.markerPath, nil, 0o600); err != nil {
			return nil, err
		}
	}

	switch name {
	case "git":
		if len(args) > 2 && args[2] == "fetch" {
			return nil, runner.fetchErr
		}
		if len(args) > 2 && args[2] == "checkout" {
			runner.checkoutCalls++
			return nil, runner.checkoutErr
		}
	case "devenv":
		runner.gatewayCalls++
		return runner.gateOutput, runner.gateErr
	case "cotal":
		if len(args) >= 4 {
			runner.wakeTexts = append(runner.wakeTexts, args[3])
		}
		return nil, runner.cotalErr
	}

	return nil, nil
}

func TestRun(t *testing.T) {
	tests := wakeTestCases()
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertWakeCase(t, test)
		})
	}
}

type wakeTestCase struct {
	name           string
	marker         bool
	missingLastAll bool
	lastAllAge     time.Duration
	gateOutput     []byte
	gateErr        error
	fetchErr       error
	checkoutErr    error
	cotalErr       error
	markAfter      string
	wantWake       string
	wantGate       bool
	wantFetch      bool
	wantCheckout   bool
	wantErr        bool
	missingEnv     string
}

const testCotalDir = "/tmp/rigel-workspaces"

func wakeTestCases() []wakeTestCase {
	securityOutput := make([]string, 25)
	for index := range securityOutput {
		securityOutput[index] = fmt.Sprintf("finding %02d", index+1)
	}

	return []wakeTestCase{
		{
			name:   "existing marker skips work",
			marker: true,
		},
		{
			name:         "stale last all sends all wake",
			lastAllAge:   25 * time.Hour,
			wantWake:     "depsupdate-wake: all",
			wantFetch:    true,
			wantCheckout: true,
		},
		{
			name:           "missing last all counts as stale",
			missingLastAll: true,
			wantWake:       "depsupdate-wake: all",
			wantFetch:      true,
			wantCheckout:   true,
		},
		{
			name:         "red vuln gate sends security tail",
			lastAllAge:   time.Hour,
			gateOutput:   []byte(strings.Join(securityOutput, "\n") + "\n"),
			gateErr:      errors.New("vuln-gate failed"),
			wantWake:     "depsupdate-wake: security\n" + strings.Join(securityOutput[5:], "\n"),
			wantGate:     true,
			wantFetch:    true,
			wantCheckout: true,
		},
		{
			name:         "green vuln gate sends nothing",
			lastAllAge:   time.Hour,
			wantGate:     true,
			wantFetch:    true,
			wantCheckout: true,
		},
		{
			name:       "missing clone fails before commands",
			missingEnv: "DEPSUPDATE_CLONE",
			wantErr:    true,
		},
		{
			name:       "missing state directory fails before commands",
			missingEnv: "DEPSUPDATE_STATE_DIR",
			wantErr:    true,
		},
		{
			name:       "missing Cotal recipient fails before commands",
			missingEnv: "COTAL_TO",
			wantErr:    true,
		},
		{
			name:       "missing Cotal directory fails before commands",
			missingEnv: "COTAL_DIR",
			wantErr:    true,
		},
		{
			name:       "fetch failure sends nothing",
			lastAllAge: time.Hour,
			fetchErr:   errors.New("fetch failed"),
			wantFetch:  true,
			wantErr:    true,
		},
		{
			name:         "marker created during fetch skips all wake",
			lastAllAge:   25 * time.Hour,
			markAfter:    "fetch",
			wantFetch:    true,
			wantCheckout: true,
			wantWake:     "",
		},
		{
			name:         "checkout failure sends no wake or later command",
			lastAllAge:   25 * time.Hour,
			checkoutErr:  errors.New("checkout failed"),
			wantFetch:    true,
			wantCheckout: true,
			wantErr:      true,
		},
		{
			name:         "all wake send failure returns error",
			lastAllAge:   25 * time.Hour,
			cotalErr:     errors.New("cotal failed"),
			wantWake:     "depsupdate-wake: all",
			wantFetch:    true,
			wantCheckout: true,
			wantErr:      true,
		},
		{
			name:         "security wake send failure returns error",
			lastAllAge:   time.Hour,
			gateErr:      errors.New("vuln-gate failed"),
			cotalErr:     errors.New("cotal failed"),
			wantWake:     "depsupdate-wake: security\n",
			wantGate:     true,
			wantFetch:    true,
			wantCheckout: true,
			wantErr:      true,
		},
		{
			name:         "marker created during gate skips security wake",
			lastAllAge:   time.Hour,
			gateErr:      errors.New("vuln-gate failed"),
			markAfter:    "devenv",
			wantGate:     true,
			wantFetch:    true,
			wantCheckout: true,
			wantWake:     "",
		},
	}
}

func assertWakeCase(t *testing.T, test wakeTestCase) {
	t.Helper()
	fixedNow := time.Date(2026, time.November, 4, 12, 0, 0, 0, time.UTC)
	stateDir := t.TempDir()
	if test.marker {
		if err := os.WriteFile(filepath.Join(stateDir, inflightFile), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if !test.marker && !test.missingLastAll && test.missingEnv == "" {
		lastAll := fixedNow.Add(-test.lastAllAge).Format(time.RFC3339)
		if err := os.WriteFile(filepath.Join(stateDir, lastAllFile), []byte(lastAll+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	runner := &fakeRunner{
		fetchErr:    test.fetchErr,
		checkoutErr: test.checkoutErr,
		gateOutput:  test.gateOutput,
		gateErr:     test.gateErr,
		cotalErr:    test.cotalErr,
		markerPath:  filepath.Join(stateDir, inflightFile),
		markAfter:   test.markAfter,
	}
	values := map[string]string{
		"DEPSUPDATE_CLONE":     "/tmp/rigel-clone",
		"DEPSUPDATE_STATE_DIR": stateDir,
		"COTAL_TO":             "deps",
		"COTAL_DIR":            testCotalDir,
	}
	if test.missingEnv != "" {
		values[test.missingEnv] = ""
	}
	err := decide(
		t.Context(),
		func(name string) string { return values[name] },
		runner,
		func() time.Time { return fixedNow },
	)

	if test.missingEnv != "" && (err == nil || !strings.Contains(err.Error(), test.missingEnv)) {
		t.Errorf("error = %v, want it to name %s", err, test.missingEnv)
	}
	assertError(t, err, test.wantErr)
	assertCommandCount(t, runner.calls, "git", "fetch", test.wantFetch)
	assertCommandCount(t, runner.calls, "git", "checkout", test.wantCheckout)
	assertCommandCount(t, runner.calls, "devenv", "", test.wantGate)
	assertWakeText(t, runner, test)
	if test.checkoutErr != nil && (runner.gatewayCalls != 0 || len(runner.wakeTexts) != 0) {
		t.Errorf("checkout failure ran later commands: gate=%d wakes=%d", runner.gatewayCalls, len(runner.wakeTexts))
	}
}

func assertError(t *testing.T, err error, want bool) {
	t.Helper()
	if (err != nil) != want {
		t.Fatalf("run() error = %v, wantErr %t", err, want)
	}
}

func assertCommandCount(t *testing.T, calls []commandCall, name, arg string, want bool) {
	t.Helper()
	if got := countCommand(calls, name, arg); got != boolCount(want) {
		t.Errorf("%s %s calls = %d, want %t", name, arg, got, want)
	}
}

func assertWakeText(t *testing.T, runner *fakeRunner, test wakeTestCase) {
	t.Helper()
	if got := len(runner.wakeTexts); got != boolCount(test.wantWake != "") {
		t.Fatalf("wake count = %d, want %t", got, test.wantWake != "")
	}
	if test.wantWake == "" {
		return
	}
	if runner.wakeTexts[0] != test.wantWake {
		t.Errorf("wake text = %q, want %q", runner.wakeTexts[0], test.wantWake)
	}
	call := lastCall(runner.calls)
	if call.name != "cotal" || call.dir != testCotalDir {
		t.Errorf("Cotal call = %#v, want command run in %s", call, testCotalDir)
	}
	if got := strings.Join(call.args, " "); got != "send --to deps "+test.wantWake+" --space main" {
		t.Errorf("Cotal args = %q, want wake sent to deps in main", got)
	}
}

func countCommand(calls []commandCall, name, arg string) int {
	count := 0
	for _, call := range calls {
		if call.name != name {
			continue
		}
		if arg == "" || len(call.args) > 2 && call.args[2] == arg {
			count++
		}
	}
	return count
}

func boolCount(value bool) int {
	if value {
		return 1
	}
	return 0
}

func lastCall(calls []commandCall) commandCall {
	return calls[len(calls)-1]
}
