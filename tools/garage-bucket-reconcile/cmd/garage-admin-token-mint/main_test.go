package main

import (
	"bytes"
	"context"
	"testing"

	"github.com/RigelBuild/nebula/tools/garage-bucket-reconcile/internal/fakeadmin"
)

// validEnv is the complete environment the mint Job provides, including the in-cluster service vars.
func validEnv() map[string]string {
	return map[string]string{
		"GARAGE_ADMIN_URL":        "http://garage.example.com:3903",
		"GARAGE_MASTER_TOKEN":     "master-token",
		"POD_NAMESPACE":           "example-namespace",
		"SECRET_NAME":             "example-admin-token",
		"TOKEN_NAME":              "example-bucket-reconcile",
		"KUBERNETES_SERVICE_HOST": "10.0.0.1",
		"KUBERNETES_SERVICE_PORT": "443",
	}
}

func getenvOf(env map[string]string) func(string) string {
	return func(name string) string { return env[name] }
}

// TestRunReportsEachMissingVariableInOrder checks each env name and the order they are reported in.
// Each missing variable exits 1 with its name on stderr, before any HTTP call.
func TestRunReportsEachMissingVariableInOrder(t *testing.T) {
	t.Parallel()
	order := []string{
		"GARAGE_ADMIN_URL",
		"GARAGE_MASTER_TOKEN",
		"POD_NAMESPACE",
		"SECRET_NAME",
		"TOKEN_NAME",
	}
	for _, name := range order {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := validEnv()
			delete(env, name)
			d := fakeadmin.NewDoer(func(fakeadmin.Request) (fakeadmin.Reply, error) {
				t.Error("made an HTTP call before reporting a missing variable")
				return fakeadmin.Reply{}, nil
			})
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), nil, getenvOf(env), d.Client(), &stdout, &stderr)
			if code != 1 {
				t.Fatalf("exit = %d, want 1", code)
			}
			if want := name + " is required\n"; stderr.String() != want {
				t.Fatalf("stderr = %q, want %q", stderr.String(), want)
			}
		})
	}
}

// TestRunFailsWhenInClusterServiceHostIsMissing checks the mint Job needs the injected service host.
// The failure happens in kube.New, after config is read and before any HTTP call.
func TestRunFailsWhenInClusterServiceHostIsMissing(t *testing.T) {
	t.Parallel()
	env := validEnv()
	delete(env, "KUBERNETES_SERVICE_HOST")
	d := fakeadmin.NewDoer(func(fakeadmin.Request) (fakeadmin.Reply, error) {
		t.Error("made an HTTP call without a Kubernetes service host")
		return fakeadmin.Reply{}, nil
	})
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), nil, getenvOf(env), d.Client(), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if want := "KUBERNETES_SERVICE_HOST is missing from the in-cluster environment\n"; stderr.String() != want {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
}

// TestRunFailsWhenInClusterServicePortIsMissing checks the port is required alongside the host.
func TestRunFailsWhenInClusterServicePortIsMissing(t *testing.T) {
	t.Parallel()
	env := validEnv()
	delete(env, "KUBERNETES_SERVICE_PORT")
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), nil, getenvOf(env), fakeadmin.NewDoer(nil).Client(), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if want := "KUBERNETES_SERVICE_PORT is missing from the in-cluster environment\n"; stderr.String() != want {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
}

// TestReadConfigStripsTrailingSlashesAndAppliesDefaults checks the admin URL trim and defaults.
func TestReadConfigStripsTrailingSlashesAndAppliesDefaults(t *testing.T) {
	t.Parallel()
	env := validEnv()
	env["GARAGE_ADMIN_URL"] = "http://garage.example.com:3903//"
	cfg, err := readConfig(getenvOf(env))
	if err != nil {
		t.Fatalf("readConfig: %v", err)
	}
	if cfg.AdminURL != "http://garage.example.com:3903" {
		t.Fatalf("AdminURL = %q, want the trailing slashes removed", cfg.AdminURL)
	}
	if cfg.MasterToken != "master-token" || cfg.Namespace != "example-namespace" || cfg.SecretName != "example-admin-token" || cfg.TokenName != "example-bucket-reconcile" {
		t.Fatalf("config = %+v, want the env values", cfg)
	}
}

// TestRunTreatsBlankAsMissing checks a whitespace-only variable is reported as missing.
func TestRunTreatsBlankAsMissing(t *testing.T) {
	t.Parallel()
	env := validEnv()
	env["TOKEN_NAME"] = "   "
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), nil, getenvOf(env), fakeadmin.NewDoer(nil).Client(), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if want := "TOKEN_NAME is required\n"; stderr.String() != want {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
}
