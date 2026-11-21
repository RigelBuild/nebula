package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RigelBuild/nebula/tools/nemo-move/internal/transfer"
)

const testSSHPath = "/nix/store/openssh/bin/ssh"

var testLaunchdJobs = []launchdJob{
	{label: "com.example.agent", domain: guiDomain},
	{label: "com.example.agent.chrome", domain: guiDomain},
	{label: "com.example.agent.dashboard", domain: guiDomain},
	{label: "com.example.agent.mcp-workspace", domain: systemDomain},
	{label: "com.example.agent.mcp-personal", domain: systemDomain},
	{label: "com.example.agent.tunnel", domain: systemDomain},
}

const testHome = "/Users/example"

// testInventory is the made-up deployment every source-touching test passes
// with --inventory.
const testInventory = "testdata/inventory.cue"

var testStopConfig = macStopConfig{home: testHome, jobs: testLaunchdJobs, processes: []string{"example-agent --serve", "example-tunnel run"}}

func TestLaunchdLabelsFlagValidatesAtParse(t *testing.T) {
	for _, test := range []struct {
		value string
		ok    bool
	}{
		{"com.example.agent:gui", true},
		{"com.example.agent:system", true},
		{"com.example.agent", false},
		{"com.example.agent:user", false},
		{":gui", false},
		{"-bad:gui", false},
		{"bad label:gui", false},
	} {
		var labels launchdLabels
		if err := labels.Set(test.value); (err == nil) != test.ok {
			t.Errorf("Set(%q) error = %v, want ok %t", test.value, err, test.ok)
		}
	}
	var labels launchdLabels
	if err := labels.Set("com.example.agent:gui"); err != nil {
		t.Fatal(err)
	}
	if err := labels.Set("com.example.agent:system"); err == nil {
		t.Error("duplicate label accepted")
	}
}

func TestMacStopRequiresSourceFlags(t *testing.T) {
	never := func(context.Context, string, ...string) (string, error) {
		t.Fatal("runner called before flag validation")
		return "", nil
	}
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"stop", "--mac", "example@192.0.2.10", "--home-root", testHome, "--ssh-user", "example", "--launchd-label", "com.example.agent:gui"}, "--inventory"},
		{[]string{"stop", "--mac", "example@192.0.2.10", "--ssh-user", "example", "--inventory", testInventory, "--launchd-label", "com.example.agent:gui"}, "--home-root"},
		{[]string{"stop", "--mac", "example@192.0.2.10", "--home-root", testHome, "--launchd-label", "com.example.agent:gui"}, "--ssh-user"},
		{[]string{"stop", "--mac", "example@192.0.2.10", "--home-root", testHome, "--ssh-user", "example", "--inventory", testInventory}, "--launchd-label"},
		{[]string{"stop", "--mac", "other@192.0.2.10", "--home-root", testHome, "--ssh-user", "example", "--inventory", testInventory, "--launchd-label", "com.example.agent:gui"}, "match --ssh-user"},
		{[]string{"stop", "--mac", "example@192.0.2.10", "--home-root", testHome, "--ssh-user", "example", "--inventory", testInventory, "--launchd-label", "com.example.agent:bogus"}, "<label>:gui"},
	} {
		if err := mac(t.Context(), test.args, never); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("mac(%q) error = %v, want %q", test.args, err, test.want)
		}
	}
}

func TestRunRemoteUsesAbsoluteSSHPath(t *testing.T) {
	called := false
	_, err := runRemote(t.Context(), "example@192.0.2.10", testSSHPath, func(_ context.Context, name string, args ...string) (string, error) {
		called = true
		if name != testSSHPath || !filepath.IsAbs(name) {
			t.Fatalf("runner executable = %q, want absolute %q", name, testSSHPath)
		}
		if len(args) != 4 || args[0] != "-o" || args[1] != "BatchMode=yes" || args[2] != "example@192.0.2.10" {
			t.Fatalf("ssh args = %q", args)
		}
		return "ok", nil
	}, "true")
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("runner was not called")
	}
}

func TestResolveSSHPath(t *testing.T) {
	for _, test := range []struct {
		name      string
		path      string
		lookPath  func(string) (string, error)
		want      string
		wantError string
	}{
		{
			name: "explicit absolute path",
			path: testSSHPath,
			lookPath: func(string) (string, error) {
				t.Fatal("lookPath called for explicit path")
				return "", nil
			},
			want: testSSHPath,
		},
		{
			name: "resolve executable from path",
			lookPath: func(name string) (string, error) {
				if name != "ssh" {
					t.Fatalf("lookPath name = %q, want ssh", name)
				}
				return testSSHPath, nil
			},
			want: testSSHPath,
		},
		{
			name:      "relative explicit path is rejected",
			path:      "ssh",
			wantError: "absolute executable path",
		},
		{
			name:      "lookup failure is returned",
			lookPath:  func(string) (string, error) { return "", errors.New("not found") },
			wantError: "find ssh executable on PATH",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := resolveSSHPath(test.path, test.lookPath)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("resolveSSHPath() error = %v, want %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("resolveSSHPath() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestMacStopRejectsRelativeSSHPath(t *testing.T) {
	called := false
	err := mac(t.Context(), []string{"stop", "--mac", "example@192.0.2.10", "--ssh", "ssh", "--home-root", testHome, "--ssh-user", "example", "--inventory", testInventory, "--launchd-label", "com.example.agent:gui"}, func(context.Context, string, ...string) (string, error) {
		called = true
		return "", nil
	})
	if err == nil || !strings.Contains(err.Error(), "absolute executable path") {
		t.Fatalf("mac() error = %v, want absolute executable error", err)
	}
	if called {
		t.Fatal("runner called with relative SSH path")
	}
}

func TestSplitSSHTargetRejectsOptionLikeComponents(t *testing.T) {
	for _, target := range []string{"-Fx@192.0.2.10", "example@-host"} {
		if _, _, err := transfer.SplitSSHTarget(target); err == nil {
			t.Errorf("splitSSHTarget(%q) unexpectedly succeeded", target)
		}
	}
}

func TestLaunchdJobLoadedClassifiesExitStatus(t *testing.T) {
	for _, test := range []struct {
		name      string
		result    string
		want      bool
		wantError bool
	}{
		{name: "loaded", result: "loaded", want: true},
		{name: "exit 113 is absent", result: "absent"},
		{name: "other launchd error fails", result: "exit:1", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var command string
			loaded, err := launchdJobLoaded(t.Context(), "example@192.0.2.10", testSSHPath, "system/com.example.agent.mcp-workspace", func(_ context.Context, _ string, args ...string) (string, error) {
				command = args[len(args)-1]
				return test.result, nil
			})
			if !strings.Contains(command, "status=$?") || !strings.Contains(command, "-eq 113") {
				t.Errorf("launchctl status command = %q, want exit 113 classification", command)
			}
			if strings.Contains(command, "sudo") {
				t.Errorf("launchctl status command uses sudo: %q", command)
			}
			if (err != nil) != test.wantError {
				t.Fatalf("launchdJobLoaded() error = %v, wantError %t", err, test.wantError)
			}
			if loaded != test.want {
				t.Errorf("launchdJobLoaded() = %t, want %t", loaded, test.want)
			}
		})
	}
}

type stopJobCase struct {
	name           string
	job            launchdJob
	loaded         bool
	inspectionFail bool
	operations     []string
	wantErr        string
}

func TestStopJobUsesExpectedDomainCommands(t *testing.T) {
	tests := []stopJobCase{
		{
			name:       "loaded gui job is booted out and disabled",
			job:        launchdJob{label: "com.example.agent.chrome", domain: guiDomain},
			loaded:     true,
			operations: []string{"launchctl bootout gui/501/com.example.agent.chrome", "launchctl disable gui/501/com.example.agent.chrome"},
		},
		{
			name:       "unloaded gui job is disabled",
			job:        launchdJob{label: "com.example.agent.chrome", domain: guiDomain},
			operations: []string{"launchctl disable gui/501/com.example.agent.chrome"},
		},
		{
			name:       "loaded system job uses noninteractive sudo",
			job:        launchdJob{label: "com.example.agent.mcp-workspace", domain: systemDomain},
			loaded:     true,
			operations: []string{"sudo -n /bin/launchctl bootout system/com.example.agent.mcp-workspace", "sudo -n /bin/launchctl disable system/com.example.agent.mcp-workspace"},
		},
		{
			name:       "unloaded system job is disabled",
			job:        launchdJob{label: "com.example.agent.mcp-workspace", domain: systemDomain},
			operations: []string{"sudo -n /bin/launchctl disable system/com.example.agent.mcp-workspace"},
		},
		{
			name:           "system inspection failure is not a sudo recovery error",
			job:            launchdJob{label: "com.example.agent.mcp-workspace", domain: systemDomain},
			inspectionFail: true,
			wantErr:        "check launchd job system/com.example.agent.mcp-workspace",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			testStopJobCase(t, test)
		})
	}
}

func testStopJobCase(t *testing.T, test stopJobCase) {
	t.Helper()
	var operations []string
	domain := serviceDomain(test.job, 501)
	runner := func(_ context.Context, name string, args ...string) (string, error) {
		if name != testSSHPath {
			return "", fmt.Errorf("SSH executable = %q, want %q", name, testSSHPath)
		}
		command := args[len(args)-1]
		if command == launchdJobProbeCommand(domain) {
			return stopJobProbeResult(test)
		}
		operations = append(operations, command)
		return "", nil
	}
	err := stopJob(t.Context(), "example@192.0.2.10", testSSHPath, domain, test.job, testLaunchdJobs, runner)
	assertStopJobResult(t, test, operations, err)
}

func stopJobProbeResult(test stopJobCase) (string, error) {
	if test.inspectionFail {
		return "exit:1", errors.New("launchctl denied")
	}
	if test.loaded {
		return "loaded", nil
	}
	return "absent", nil
}

func assertStopJobResult(t *testing.T, test stopJobCase, operations []string, err error) {
	t.Helper()
	if test.wantErr == "" && err != nil {
		t.Fatalf("stopJob() error = %v", err)
	}
	if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
		t.Fatalf("stopJob() error = %v, want %q", err, test.wantErr)
	}
	if err != nil && strings.Contains(err.Error(), "sudo -n could not stop") {
		t.Errorf("inspection failure was mislabeled as a sudo recovery error: %v", err)
	}
	if len(operations) != len(test.operations) {
		t.Fatalf("operations = %q, want %q", operations, test.operations)
	}
	for i, want := range test.operations {
		if !strings.Contains(operations[i], want) {
			t.Errorf("operation[%d] = %q, want it to contain %q", i, operations[i], want)
		}
	}
}

func TestVerifyMacStoppedFailsForLoadedJobOrProcess(t *testing.T) {
	for _, test := range []struct {
		name       string
		processes  string
		loaded     string
		enabledJob string
		wantError  string
	}{
		{
			name:      "remaining example agent process",
			processes: "123 /nix/store/example-agent/bin/example-agent --serve --foreground\n",
			wantError: "process remains matching",
		},
		{
			name:      "remaining example tunnel process",
			processes: "123 /nix/store/example-tunnel/bin/example-tunnel run --config /etc/example\n",
			wantError: "process remains matching",
		},
		{
			name:      "loaded system job remains loaded",
			loaded:    "system/com.example.agent.mcp-workspace",
			wantError: "remains loaded",
		},
		{
			name:      "loaded gui job remains loaded",
			loaded:    "gui/501/com.example.agent.chrome",
			wantError: "remains loaded",
		},
		{
			name:       "system job is still enabled",
			enabledJob: "com.example.agent.tunnel",
			wantError:  "is not disabled",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := func(_ context.Context, _ string, args ...string) (string, error) {
				command := args[len(args)-1]
				switch {
				case strings.HasPrefix(command, "/bin/launchctl print-disabled gui/"):
					return `"com.example.agent" => disabled
"com.example.agent.chrome" => disabled
"com.example.agent.dashboard" => disabled`, nil
				case command == "/bin/launchctl print-disabled system":
					output := formatDisabledJobs(map[string]bool{
						"com.example.agent.mcp-workspace": true,
						"com.example.agent.mcp-personal":  true,
						"com.example.agent.tunnel":        test.enabledJob != "com.example.agent.tunnel",
					}, systemDomain)
					return output, nil
				case strings.HasPrefix(command, "/bin/sh -c "):
					if test.loaded != "" && strings.Contains(command, test.loaded) {
						return "loaded", nil
					}
					return "absent", nil
				case command == "/bin/ps -axo pid=,command=":
					return test.processes, nil
				default:
					return "", fmt.Errorf("unexpected command %q", command)
				}
			}
			err := verifyMacStopped(t.Context(), "example@192.0.2.10", testSSHPath, testStopConfig, 501, runner)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("verifyMacStopped() error = %v, want %q", err, test.wantError)
			}
		})
	}
}

func TestStopMacRequiresReachableGUI(t *testing.T) {
	runner := func(_ context.Context, _ string, args ...string) (string, error) {
		switch args[len(args)-1] {
		case "/usr/bin/id -u":
			return "501\n", nil
		default:
			return "", errors.New("launchd domain unavailable")
		}
	}
	err := stopMac(t.Context(), "example@192.0.2.10", testSSHPath, testStopConfig, runner)
	if err == nil || !strings.Contains(err.Error(), "cannot reach user launchd domain gui/501") {
		t.Fatalf("stopMac() error = %v, want precise GUI launchd error", err)
	}
}

func TestStopMacIsIdempotent(t *testing.T) {
	fake := newMacStopFake(true)
	for run := 1; run <= 2; run++ {
		if err := stopMac(t.Context(), "example@192.0.2.10", testSSHPath, testStopConfig, fake.run); err != nil {
			t.Fatalf("stopMac() run %d error = %v", run, err)
		}
	}
	if fake.bootouts != len(testLaunchdJobs) {
		t.Errorf("bootout count = %d, want %d (one per initially loaded job)", fake.bootouts, len(testLaunchdJobs))
	}
	if fake.disables != 2*len(testLaunchdJobs) {
		t.Errorf("disable count = %d, want %d (one per job on each run)", fake.disables, 2*len(testLaunchdJobs))
	}
}

func TestStopMacLastQuietAlreadyStoppedAndRerun(t *testing.T) {
	fake := newMacStopFake(false)
	for _, job := range testLaunchdJobs {
		fake.loaded[job.label] = false
		fake.disabled[job.label] = true
	}
	for range 2 {
		if err := stopMac(t.Context(), "example@192.0.2.10", testSSHPath, testStopConfig, fake.run); err != nil {
			t.Fatal(err)
		}
	}
	if fake.quietWrites != 1 {
		t.Fatalf("quiet marker writes = %d, want 1", fake.quietWrites)
	}
	fake.loaded[testLaunchdJobs[0].label] = true
	if err := stopMac(t.Context(), "example@192.0.2.10", testSSHPath, testStopConfig, fake.run); err != nil {
		t.Fatal(err)
	}
	if fake.quietWrites != 2 {
		t.Fatalf("quiet marker writes after job = %d, want 2", fake.quietWrites)
	}
}

type macStopFake struct {
	loaded               map[string]bool
	disabled             map[string]bool
	sudoAvailable        bool
	failSystemInspection bool
	processes            string
	bootouts             int
	disables             int
	quietWrites          int
	quietExists          bool
}

func newMacStopFake(sudoAvailable bool) *macStopFake {
	fake := &macStopFake{
		loaded:        make(map[string]bool, len(testLaunchdJobs)),
		disabled:      make(map[string]bool, len(testLaunchdJobs)),
		sudoAvailable: sudoAvailable,
	}
	for _, job := range testLaunchdJobs {
		fake.loaded[job.label] = true
	}
	return fake
}

func (fake *macStopFake) run(_ context.Context, name string, args ...string) (string, error) {
	if name != testSSHPath {
		return "", fmt.Errorf("SSH executable = %q, want %q", name, testSSHPath)
	}
	command := args[len(args)-1]
	switch {
	case command == "/usr/bin/id -u":
		return "501\n", nil
	case strings.Contains(command, "printf reachable"):
		return "reachable", nil
	case command == "sudo -n true":
		return fake.sudoResult()
	case strings.HasPrefix(command, "/bin/launchctl print-disabled gui/"):
		return formatDisabledJobs(fake.disabled, guiDomain), nil
	case command == "/bin/launchctl print-disabled system":
		return formatDisabledJobs(fake.disabled, systemDomain), nil
	case strings.Contains(command, "nemo-move/last-quiet"):
		if !fake.quietExists || strings.Contains(command, "if true;") {
			fake.quietExists = true
			fake.quietWrites++
		}
		return "", nil
	case strings.HasPrefix(command, "/bin/sh -c "):
		return fake.probe(command)
	case strings.Contains(command, "launchctl bootout "):
		return fake.applyJobState(command, true)
	case strings.Contains(command, "launchctl disable "):
		return fake.applyJobState(command, false)
	case command == "/bin/ps -axo pid=,command=":
		return fake.processes, nil
	default:
		return "", fmt.Errorf("unexpected command %q", command)
	}
}

func (fake *macStopFake) applyJobState(command string, bootout bool) (string, error) {
	job, ok := findCommandJob(command)
	if !ok {
		return "", fmt.Errorf("unrecognized launchctl command %q", command)
	}
	if bootout {
		fake.loaded[job.label] = false
		fake.bootouts++
		return "", nil
	}
	fake.disabled[job.label] = true
	fake.disables++
	return "", nil
}

func (fake *macStopFake) sudoResult() (string, error) {
	if !fake.sudoAvailable {
		return "", errors.New("sudo requires an interactive session")
	}
	return "", nil
}

func (fake *macStopFake) probe(command string) (string, error) {
	job, ok := findProbeJob(command)
	if !ok {
		return "exit:113", nil
	}
	if fake.failSystemInspection && job.domain == systemDomain {
		return "exit:1", errors.New("SSH transport failed")
	}
	if fake.loaded[job.label] {
		return "loaded", nil
	}
	return "absent", nil
}

func findProbeJob(command string) (launchdJob, bool) {
	for _, job := range testLaunchdJobs {
		if command == launchdJobProbeCommand(serviceDomain(job, 501)) {
			return job, true
		}
	}
	return launchdJob{}, false
}

func findCommandJob(command string) (launchdJob, bool) {
	var matched launchdJob
	found := false
	for _, job := range testLaunchdJobs {
		if strings.HasSuffix(command, "/"+job.label) && (!found || len(job.label) > len(matched.label)) {
			matched = job
			found = true
		}
	}
	return matched, found
}

func formatDisabledJobs(disabled map[string]bool, domain string) string {
	var lines []string
	for _, job := range testLaunchdJobs {
		if job.domain == domain && disabled[job.label] {
			lines = append(lines, fmt.Sprintf("%q => disabled", job.label))
		}
	}
	return strings.Join(lines, "\n")
}

func TestStopMacRerunsAfterInteractiveSystemShutdown(t *testing.T) {
	fake := newMacStopFake(false)
	firstErr := stopMac(t.Context(), "example@192.0.2.10", testSSHPath, testStopConfig, fake.run)
	if firstErr == nil || !strings.Contains(firstErr.Error(), "sudo -n could not stop all system launchd jobs") {
		t.Fatalf("first stopMac() error = %v, want interactive system shutdown commands", firstErr)
	}
	if !strings.Contains(firstErr.Error(), "sudo /bin/launchctl bootout system/com.example.agent.mcp-workspace") {
		t.Fatalf("first stopMac() error lacks recovery command: %v", firstErr)
	}
	fake.markSystemJobsStopped()
	fake.failSystemInspection = true
	inspectionErr := stopMac(t.Context(), "example@192.0.2.10", testSSHPath, testStopConfig, fake.run)
	if inspectionErr == nil || !strings.Contains(inspectionErr.Error(), "verify system launchd job") || strings.Contains(inspectionErr.Error(), "sudo -n could not stop") {
		t.Fatalf("rerun with system inspection failure error = %v, want inspection-specific error", inspectionErr)
	}
	fake.failSystemInspection = false
	fake.processes = "123 /nix/store/example-agent/bin/example-agent --serve --agent example\n"
	processErr := stopMac(t.Context(), "example@192.0.2.10", testSSHPath, testStopConfig, fake.run)
	if processErr == nil || !strings.Contains(processErr.Error(), "process remains matching") || strings.Contains(processErr.Error(), "sudo -n could not stop") {
		t.Fatalf("rerun with leftover process error = %v, want process-specific verification error", processErr)
	}
	fake.processes = ""
	if err := stopMac(t.Context(), "example@192.0.2.10", testSSHPath, testStopConfig, fake.run); err != nil {
		t.Fatalf("rerun after interactive system shutdown: %v", err)
	}
}

func (fake *macStopFake) markSystemJobsStopped() {
	for _, job := range testLaunchdJobs {
		if job.domain == systemDomain {
			fake.loaded[job.label] = false
			fake.disabled[job.label] = true
		}
	}
}
