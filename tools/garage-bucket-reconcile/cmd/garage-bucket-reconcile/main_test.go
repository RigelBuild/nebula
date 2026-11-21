package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/RigelBuild/nebula/tools/garage-bucket-reconcile/internal/fakeadmin"
)

const (
	testAdminURL = "http://garage.example.com:3903"
	testAdminTok = "scoped-admin-token"
	testAccess   = "S3-access-key"
	testBucket   = "example-bucket"
)

// validEnv is the complete, valid environment the Job provides.
func validEnv() map[string]string {
	return map[string]string{
		"GARAGE_ADMIN_URL":      testAdminURL,
		"GARAGE_ADMIN_TOKEN":    testAdminTok,
		"S3_ACCESS_KEY_ID":      testAccess,
		"BUCKET_NAME":           testBucket,
		"BUCKET_MAX_SIZE_BYTES": "5368709120",
	}
}

func getenvOf(env map[string]string) func(string) string {
	return func(name string) string { return env[name] }
}

// completeReplies answers each endpoint the happy path calls, with the same URLs as the reconcile tests.
func completeReplies(req fakeadmin.Request) (fakeadmin.Reply, error) {
	if req.URL == testAdminURL+"/health" {
		return fakeadmin.Reply{Body: "ready"}, nil
	}
	switch req.URL {
	case testAdminURL + "/v2/ListBuckets":
		return fakeadmin.Reply{Body: []any{}}, nil
	case testAdminURL + "/v2/CreateBucket":
		return fakeadmin.Reply{Body: map[string]any{"id": "bucket-123", "globalAliases": []any{testBucket}}}, nil
	case testAdminURL + "/v2/GetKeyInfo?id=" + testAccess:
		return fakeadmin.Reply{Body: map[string]any{"accessKeyId": testAccess}}, nil
	case testAdminURL + "/v2/UpdateBucket?id=bucket-123":
		return fakeadmin.Reply{Body: map[string]any{"quotas": map[string]any{"maxSize": int64(5368709120)}}}, nil
	case testAdminURL + "/v2/AllowBucketKey":
		return fakeadmin.Reply{Body: map[string]any{"keys": []any{map[string]any{
			"accessKeyId": testAccess,
			"permissions": map[string]any{"read": true, "write": true, "owner": false},
		}}}}, nil
	}
	return fakeadmin.Reply{}, errors.New("unexpected request " + req.URL)
}

// TestRunExitsZeroAndLogsEachStepOnSuccess covers the happy path: exit 0, stdout lines, and no stderr.
func TestRunExitsZeroAndLogsEachStepOnSuccess(t *testing.T) {
	t.Parallel()
	d := fakeadmin.NewDoer(completeReplies)
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), nil, getenvOf(validEnv()), d.Client(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	for _, line := range []string{
		"Waiting for Garage admin API readiness",
		"Listing Garage buckets",
		"Creating Garage bucket example-bucket",
		"Checking Garage access key",
		"Setting Garage bucket quota",
		"Granting Garage bucket key access",
	} {
		if !strings.Contains(stdout.String(), line+"\n") {
			t.Fatalf("stdout missing %q; got %q", line, stdout.String())
		}
	}
}

// TestRunReportsEachMissingVariableInOrder checks the env names and the order they are reported in.
func TestRunReportsEachMissingVariableInOrder(t *testing.T) {
	t.Parallel()
	// BUCKET_MAX_SIZE_BYTES is read first, then the admin URL, token, key, and bucket.
	order := []string{
		"BUCKET_MAX_SIZE_BYTES",
		"GARAGE_ADMIN_URL",
		"GARAGE_ADMIN_TOKEN",
		"S3_ACCESS_KEY_ID",
		"BUCKET_NAME",
	}
	for _, name := range order {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := validEnv()
			delete(env, name)
			d := fakeadmin.NewDoer(completeReplies)
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), nil, getenvOf(env), d.Client(), &stdout, &stderr)
			if code != 1 {
				t.Fatalf("exit = %d, want 1", code)
			}
			if want := name + " is required\n"; stderr.String() != want {
				t.Fatalf("stderr = %q, want %q", stderr.String(), want)
			}
			if len(d.Requests()) != 0 {
				t.Fatalf("made %d HTTP calls before reporting a missing variable", len(d.Requests()))
			}
		})
	}
}

// TestRunReportsFirstMissingVariableWhenSeveralAreMissing checks the first reported name follows the fixed order.
func TestRunReportsFirstMissingVariableWhenSeveralAreMissing(t *testing.T) {
	t.Parallel()
	env := map[string]string{"GARAGE_ADMIN_TOKEN": "x"}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), nil, getenvOf(env), fakeadmin.NewDoer(completeReplies).Client(), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if want := "BUCKET_MAX_SIZE_BYTES is required\n"; stderr.String() != want {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
}

// TestRunTreatsBlankAsMissing checks a whitespace-only value is treated as unset.
func TestRunTreatsBlankAsMissing(t *testing.T) {
	t.Parallel()
	env := validEnv()
	env["BUCKET_NAME"] = " \t "
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), nil, getenvOf(env), fakeadmin.NewDoer(completeReplies).Client(), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if want := "BUCKET_NAME is required\n"; stderr.String() != want {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
}

// TestRunSizeGuardAcceptsAndRejectsBoundaryValues is the size-guard pair.
// Each rejected value must exit 1 with the guard message, and each accepted value must reach the admin API.
func TestRunSizeGuardAcceptsAndRejectsBoundaryValues(t *testing.T) {
	t.Parallel()
	rejected := []string{"-1", "1.5", "9007199254740992", "NaN", "Infinity", "abc", "-0x1", "1e400"}
	for _, raw := range rejected {
		t.Run("rejects "+raw, func(t *testing.T) {
			t.Parallel()
			env := validEnv()
			env["BUCKET_MAX_SIZE_BYTES"] = raw
			d := fakeadmin.NewDoer(completeReplies)
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), nil, getenvOf(env), d.Client(), &stdout, &stderr)
			if code != 1 {
				t.Fatalf("exit = %d, want 1 for %q", code, raw)
			}
			if want := "BUCKET_MAX_SIZE_BYTES must be a non-negative safe integer\n"; stderr.String() != want {
				t.Fatalf("stderr = %q, want %q", stderr.String(), want)
			}
			if len(d.Requests()) != 0 {
				t.Fatalf("%q reached the admin API", raw)
			}
		})
	}
	accepted := []string{"0", "5368709120", " 5368709120 ", "9007199254740991", "+7", "0x10"}
	for _, raw := range accepted {
		t.Run("accepts "+raw, func(t *testing.T) {
			t.Parallel()
			env := validEnv()
			env["BUCKET_MAX_SIZE_BYTES"] = raw
			d := fakeadmin.NewDoer(completeReplies)
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), nil, getenvOf(env), d.Client(), &stdout, &stderr)
			if stderr.Len() != 0 && strings.Contains(stderr.String(), "BUCKET_MAX_SIZE_BYTES") {
				t.Fatalf("%q rejected by the size guard: %q", raw, stderr.String())
			}
			if len(d.Requests()) == 0 {
				t.Fatalf("%q never reached the admin API (exit %d, stderr %q)", raw, code, stderr.String())
			}
		})
	}
}

// TestRunStripsTrailingSlashesFromAdminURL checks readConfig trims the admin URL.
func TestRunStripsTrailingSlashesFromAdminURL(t *testing.T) {
	t.Parallel()
	cfg, err := readConfig(getenvOf(map[string]string{
		"BUCKET_MAX_SIZE_BYTES": "0",
		"GARAGE_ADMIN_URL":      testAdminURL + "///",
		"GARAGE_ADMIN_TOKEN":    testAdminTok,
		"S3_ACCESS_KEY_ID":      testAccess,
		"BUCKET_NAME":           testBucket,
	}))
	if err != nil {
		t.Fatalf("readConfig: %v", err)
	}
	if cfg.AdminURL != testAdminURL {
		t.Fatalf("AdminURL = %q, want %q", cfg.AdminURL, testAdminURL)
	}
	if cfg.MaxSize != 0 {
		t.Fatalf("MaxSize = %d, want 0", cfg.MaxSize)
	}
}

// TestRunExitsOneWhenReconcileFails checks a reconcile error becomes exit 1 with its message on stderr.
func TestRunExitsOneWhenReconcileFails(t *testing.T) {
	t.Parallel()
	d := fakeadmin.NewDoer(func(req fakeadmin.Request) (fakeadmin.Reply, error) {
		if req.URL == testAdminURL+"/health" {
			return fakeadmin.Reply{Body: "ready"}, nil
		}
		return fakeadmin.Reply{}, errors.New("connection refused")
	})
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), nil, getenvOf(validEnv()), d.Client(), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if stderr.Len() == 0 {
		t.Fatal("stderr is empty on a failed reconcile")
	}
}

// failingWriter fails every write, so a lost output line must turn into exit 1.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

// TestRunExitsOneWhenStdoutWriteFails checks a failed stdout write is a failure, not a silent success.
func TestRunExitsOneWhenStdoutWriteFails(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	code := run(context.Background(), nil, getenvOf(validEnv()), fakeadmin.NewDoer(completeReplies).Client(), failingWriter{}, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "writing reconcile output: disk full") {
		t.Fatalf("stderr = %q, want the write error", stderr.String())
	}
}
