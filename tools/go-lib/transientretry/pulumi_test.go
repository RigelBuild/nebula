package transientretry

import (
	"errors"
	"testing"
)

func TestClassifyPulumiTransient(t *testing.T) {
	if got := ClassifyPulumiTransient(nil); got != "" {
		t.Fatalf("ClassifyPulumiTransient(nil) = %q, want none", got)
	}
	if got := ClassifyRegistryTransient(nil); got != "" {
		t.Fatalf("ClassifyRegistryTransient(nil) = %q, want none", got)
	}
	if got := ClassifyPulumiTransient(&DiagnosticError{Message: "ECONNRESET", HitLocalCeiling: true}); got != "" {
		t.Fatalf("local ceiling classified as %q, want none", got)
	}
	type retryCase struct {
		name string
		err  error
		want Class
	}
	cases := make([]retryCase, 0, 18)
	cases = append(cases,
		retryCase{"429 before any resource step", errors.New("HTTP 429 Too Many Requests"), PulumiTransient},
		retryCase{"network failure before any resource step", errors.New("ECONNRESET"), PulumiTransient},
		retryCase{"429 after a resource step", errors.New("HTTP 429 Too Many Requests\n + aws:s3:Bucket bucket"), ""},
		retryCase{"structured resource-step flag", &DiagnosticError{Message: "ECONNRESET", ResourceStepsStarted: true}, ""},
		retryCase{"conflict is not retried", errors.New("409 another update is in progress"), ""},
		retryCase{"503 is not a Pulumi transient", errors.New("HTTP 503 Service Unavailable"), ""},
	)
	// Every step prefix from Pulumi's RawPrefix except the two-space no-op row.
	for _, prefix := range []string{"+ ", "- ", "~ ", "+-", "++", "--", "> ", ">>", "< ", "<<", "= ", "=>"} {
		cases = append(cases, retryCase{
			name: "resource row " + prefix + " blocks the retry",
			err:  errors.New("    " + prefix + "aws:s3/bucket:Bucket site creating (0s)\nerror: HTTP 429 Too Many Requests"),
			want: "",
		})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyPulumiTransient(tc.err); got != tc.want {
				t.Fatalf("ClassifyPulumiTransient = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClassifyRegistryTransient(t *testing.T) {
	retry := []string{
		"PUT https://example.azurecr.io/v2/ci/ci-go/blobs/uploads/u: TOOMANYREQUESTS: too many requests",
		`level=fatal msg="toomanyrequests: too many requests"`,
		"too many requests to registry",
		"PUT https://example.azurecr.io/v2/ci/x/manifests/t: unexpected status code 429 Too Many Requests",
		"uploading layer chunked: StatusCode: 503 Service Unavailable",
		"uploading layer to https://registry.example/v2/layers/id: StatusCode: 503 Service Unavailable",
		"initiating layer upload to /v2/layers/id: server returned 503 Service Unavailable",
		"error: failed to solve: failed to push ghcr.io/o/i:t: unexpected status from PUT request to https://ghcr.io/v2/o/i/blobs/uploads/u: 502 Bad Gateway",
		"PUT https://ghcr.io/v2/o/i/manifests/t: unexpected status code 503 Service Unavailable",
		"connection reset by peer",
		"skopeo: use of closed network connection",
		"broken pipe",
		"unexpected EOF",
		"i/o timeout",
		"transfer timed out",
	}
	for _, msg := range retry {
		t.Run("retry "+msg, func(t *testing.T) {
			if got := ClassifyRegistryTransient(errors.New(msg)); got != RegistryTransient {
				t.Fatalf("got %q, want %q", got, RegistryTransient)
			}
		})
	}
	terminal := []struct {
		name string
		err  error
	}{
		{"429 inside a digest or port", errors.New("unexpected status from GET request to https://acr.io:429/v2/o/i/manifests/sha256:429a: 404 Not Found")},
		{"403 with a 5xx-looking port", errors.New("unexpected status from HEAD request to https://ghcr.io:503/v2/: 403 Forbidden")},
		{"403 with a retried 502", errors.New("unexpected status: 403 Forbidden, retried 502 times")},
		{"404 with a 5xx-looking digest", errors.New("unexpected status from GET request to https://ghcr.io/v2/o/i/manifests/sha256:500a: 404 Not Found")},
		{"local ceiling timeout (shared push)", &DiagnosticError{Message: "shared push timed out", HitLocalCeiling: true}},
		{"local ceiling timeout (stitch)", &DiagnosticError{Message: "stitch timed out", HitLocalCeiling: true}},
		{"per-attempt timeout with 401 in stderr", &DiagnosticError{Message: "build timed out", Stderr: "HTTP 401 Unauthorized", RetryableTimeout: true}},
		{"fatal denial overrides per-attempt timeout", &DiagnosticError{Message: "build+push timed out after 300s", Stderr: `level=fatal msg="denied: permission_denied"`, RetryableTimeout: true}},
		{"last fatal diagnostic overrides an earlier network failure", &DiagnosticError{Message: "build+push timed out after 300s", Stderr: "level=fatal msg=\"use of closed network connection\"\nlevel=fatal msg=\"denied: permission_denied\"", RetryableTimeout: true}},
		{"warning network text does not classify a fatal denial", &DiagnosticError{Message: "push failed", Stderr: "level=warning msg=\"connection reset by peer\"\nlevel=fatal msg=\"denied: permission_denied\""}},
		{"authorization is terminal", errors.New("denied: permission_denied (HTTP 403)")},
		{"manifest invalid is terminal", errors.New("manifest invalid")},
		{"Dockerfile parse error is terminal", errors.New("failed to solve: Dockerfile parse error")},
	}
	for _, tc := range terminal {
		t.Run("terminal "+tc.name, func(t *testing.T) {
			if got := ClassifyRegistryTransient(tc.err); got != "" {
				t.Fatalf("got %q, want none", got)
			}
		})
	}
	t.Run("structured per-attempt timeout remains transient", func(t *testing.T) {
		err := &DiagnosticError{Message: "build+push timed out after 300s\nordinary output", RetryableTimeout: true}
		if got := ClassifyRegistryTransient(err); got != RegistryTransient {
			t.Fatalf("got %q, want %q", got, RegistryTransient)
		}
	})
	t.Run("last fatal line carries a network drop", func(t *testing.T) {
		err := &DiagnosticError{Message: "push failed", Stderr: "level=fatal msg=\"use of closed network connection\"\nlevel=warning msg=\"retrying\""}
		if got := ClassifyRegistryTransient(err); got != RegistryTransient {
			t.Fatalf("got %q, want %q", got, RegistryTransient)
		}
	})
}
