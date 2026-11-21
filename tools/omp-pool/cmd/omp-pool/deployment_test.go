// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"errors"
	"strings"
	"testing"
)

// Example values for tests. Deployment inputs have no defaults, so every test
// supplies this complete set through the environment.
const (
	testHost       = "example-host"
	testKubeconfig = "/etc/example/broker.kubeconfig"
	testNamespace  = "example-ns"
	testPod        = "example-pod"
	testSecret     = "example-secret"
)

// testEnv returns a getenv that supplies every deployment input. Overrides
// replace or, when set to "", remove a single entry.
func testEnv(overrides map[string]string) func(string) string {
	values := map[string]string{
		envHost:         testHost,
		envKubeconfig:   testKubeconfig,
		envNamespace:    testNamespace,
		envBrokerPod:    testPod,
		envBrokerSecret: testSecret,
	}
	for key, value := range overrides {
		values[key] = value
	}
	return func(key string) string { return values[key] }
}

// testDeployment is the deployment that testEnv(nil) resolves to.
func testDeployment() deployment {
	return deployment{host: testHost, kubeconfig: testKubeconfig, namespace: testNamespace, pod: testPod, secret: testSecret}
}

func TestLoadDeploymentRequiresEveryInput(t *testing.T) {
	for _, name := range []string{envHost, envKubeconfig, envNamespace, envBrokerPod, envBrokerSecret} {
		t.Run(name, func(t *testing.T) {
			_, err := loadDeployment(testEnv(map[string]string{name: ""}), "")
			var usage usageError
			if !errors.As(err, &usage) {
				t.Fatalf("loadDeployment() error = %v, want usage error", err)
			}
			if !strings.Contains(err.Error(), name) {
				t.Fatalf("loadDeployment() error = %q, want it to name %s", err, name)
			}
		})
	}
}

func TestLoadDeploymentReportsEveryMissingInput(t *testing.T) {
	empty := func(string) string { return "" }
	_, err := loadDeployment(empty, "")
	if err == nil {
		t.Fatal("loadDeployment() error = nil, want missing-input error")
	}
	for _, name := range []string{envHost, envKubeconfig, envNamespace, envBrokerPod, envBrokerSecret} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("loadDeployment() error = %q, want it to name %s", err, name)
		}
	}
}

func TestLoadDeploymentHostOverride(t *testing.T) {
	got, err := loadDeployment(testEnv(map[string]string{envHost: "env-host"}), "override-host")
	if err != nil {
		t.Fatalf("loadDeployment() error = %v", err)
	}
	if got.host != "override-host" {
		t.Fatalf("host = %q, want the --host override", got.host)
	}
}

func TestLoadDeploymentHostFromEnvironment(t *testing.T) {
	got, err := loadDeployment(testEnv(nil), "")
	if err != nil {
		t.Fatalf("loadDeployment() error = %v", err)
	}
	if got != testDeployment() {
		t.Fatalf("loadDeployment() = %+v, want %+v", got, testDeployment())
	}
}

func TestMissingDeploymentIsUsageErrorExitTwo(t *testing.T) {
	var stdout, stderr strings.Builder
	code := runWithRunner(t.Context(), []string{"status", "extra"}, testEnv(nil), strings.NewReader(""), &stdout, &stderr, &fakeRunner{})
	if code != 2 {
		t.Fatalf("runWithRunner() = %d, want usage exit 2", code)
	}
	code = runWithRunner(t.Context(), []string{"push", poolPrimary}, testEnv(map[string]string{envBrokerPod: ""}), strings.NewReader(""), &stdout, &stderr, &fakeRunner{})
	if code != 2 {
		t.Fatalf("runWithRunner() with missing pod = %d, want usage exit 2", code)
	}
	if !strings.Contains(stderr.String(), envBrokerPod) {
		t.Fatalf("stderr %q does not name %s", stderr.String(), envBrokerPod)
	}
}
