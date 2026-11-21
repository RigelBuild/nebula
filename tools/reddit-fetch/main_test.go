package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// scriptedReply describes one canned response: a status, a body, and optional headers.
type scriptedReply struct {
	status int
	body   string
	header http.Header
	// readErr, when set, makes the body read fail instead of returning body.
	readErr error
}

// scriptedTransport answers requests from a fixed script and records each request.
type scriptedTransport struct {
	replies  []scriptedReply
	requests []*http.Request
}

func (s *scriptedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	s.requests = append(s.requests, request)
	if len(s.replies) == 0 {
		return nil, errors.New("unexpected request")
	}
	next := s.replies[0]
	s.replies = s.replies[1:]
	return next.response(), nil
}

// failingReader is a response body whose every read returns err.
type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) {
	return 0, r.err
}

// response builds the http.Response the Go client would see for this reply.
func (r scriptedReply) response() *http.Response {
	header := r.header
	if header == nil {
		header = http.Header{}
	}
	body := io.NopCloser(strings.NewReader(r.body))
	if r.readErr != nil {
		body = io.NopCloser(failingReader{err: r.readErr})
	}
	return &http.Response{
		StatusCode: r.status,
		Status:     fmt.Sprintf("%d %s", r.status, http.StatusText(r.status)),
		Header:     header,
		Body:       body,
	}
}

// reply is a shorthand scriptedReply for a response with the given status, body and headers.
func reply(status int, body string, header http.Header) scriptedReply {
	return scriptedReply{status: status, body: body, header: header}
}

// failingBodyReply is a 200 response whose body read fails.
func failingBodyReply() scriptedReply {
	return scriptedReply{status: http.StatusOK, readErr: errors.New("body read failed")}
}

// retryAfterHeader returns a header that carries only Retry-After.
func retryAfterHeader(value string) http.Header {
	header := http.Header{}
	header.Set("Retry-After", value)
	return header
}

// fixture stubs the environment, network and clock for one invocation.
type fixture struct {
	transport *scriptedTransport
	env       map[string]string
	stdout    bytes.Buffer
	stderr    bytes.Buffer
	sleeps    []time.Duration
}

func newFixture(replies ...scriptedReply) *fixture {
	return &fixture{transport: &scriptedTransport{replies: replies}}
}

// invoke runs the tool with args and returns its exit code.
func (f *fixture) invoke(t *testing.T, args ...string) int {
	t.Helper()
	return run(t.Context(), deps{
		lookupEnv: func(key string) (string, bool) {
			value, ok := f.env[key]
			return value, ok
		},
		stdout: &f.stdout,
		stderr: &f.stderr,
		client: &http.Client{Transport: f.transport},
		sleep: func(_ context.Context, d time.Duration) error {
			f.sleeps = append(f.sleeps, d)
			return nil
		},
	}, args)
}

// assertCode checks the exit code.
func assertCode(t *testing.T, got, want int) {
	t.Helper()
	if got != want {
		t.Fatalf("exit code = %d, want %d", got, want)
	}
}

// assertSleeps checks the recorded waits, in order.
func assertSleeps(t *testing.T, got []time.Duration, want ...time.Duration) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("sleeps = %v, want %v", got, want)
	}
}

// assertStderr checks the exact error bytes the tool wrote.
func assertStderr(t *testing.T, f *fixture, want string) {
	t.Helper()
	if got := f.stderr.String(); got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
}

// assertNoStdout checks that no body reached stdout.
func assertNoStdout(t *testing.T, f *fixture) {
	t.Helper()
	if f.stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", f.stdout.String())
	}
}

func TestRunSuccess(t *testing.T) {
	t.Run("emits the raw body and sends the URL and user agent", func(t *testing.T) {
		f := newFixture(reply(http.StatusOK, `{"data":1}`, nil))
		assertCode(t, f.invoke(t, "r/OpenAI"), 0)
		if got, want := f.stdout.String(), "{\"data\":1}\n"; got != want {
			t.Errorf("stdout = %q, want %q", got, want)
		}
		if len(f.transport.requests) != 1 {
			t.Fatalf("requests = %d, want 1", len(f.transport.requests))
		}
		request := f.transport.requests[0]
		if got, want := request.URL.String(), "https://www.reddit.com/r/OpenAI.json"; got != want {
			t.Errorf("url = %q, want %q", got, want)
		}
		if got := request.Header.Get("User-Agent"); got != defaultUserAgent {
			t.Errorf("User-Agent = %q, want %q", got, defaultUserAgent)
		}
	})
	t.Run("delays between targets and emits every body", func(t *testing.T) {
		f := newFixture(reply(http.StatusOK, "A", nil), reply(http.StatusOK, "B", nil), reply(http.StatusOK, "C", nil))
		assertCode(t, f.invoke(t, "r/a", "r/b", "r/c", "--delay-ms", "2000"), 0)
		if got, want := f.stdout.String(), "A\nB\nC\n"; got != want {
			t.Errorf("stdout = %q, want %q", got, want)
		}
		assertSleeps(t, f.sleeps, 2*time.Second, 2*time.Second)
	})
	t.Run("environment overrides the default user agent", func(t *testing.T) {
		f := newFixture(reply(http.StatusOK, "{}", nil))
		f.env = map[string]string{"REDDIT_USER_AGENT": "env-agent"}
		assertCode(t, f.invoke(t, "r/x"), 0)
		if got := f.transport.requests[0].Header.Get("User-Agent"); got != "env-agent" {
			t.Errorf("User-Agent = %q, want env-agent", got)
		}
	})
	t.Run("empty environment still sends the default user agent", func(t *testing.T) {
		f := newFixture(reply(http.StatusOK, "{}", nil))
		f.env = map[string]string{"REDDIT_USER_AGENT": ""}
		assertCode(t, f.invoke(t, "r/x"), 0)
		if got := f.transport.requests[0].Header.Get("User-Agent"); got != defaultUserAgent {
			t.Errorf("User-Agent = %q, want %q", got, defaultUserAgent)
		}
	})
}

func TestRunRetries(t *testing.T) {
	t.Run("honors Retry-After on 429", func(t *testing.T) {
		f := newFixture(reply(http.StatusTooManyRequests, "", retryAfterHeader("3")), reply(http.StatusOK, "{}", nil))
		assertCode(t, f.invoke(t, "r/x"), 0)
		assertSleeps(t, f.sleeps, 3*time.Second)
	})
	t.Run("waits sixty seconds when Retry-After is absent", func(t *testing.T) {
		f := newFixture(reply(http.StatusTooManyRequests, "", nil), reply(http.StatusOK, "{}", nil))
		assertCode(t, f.invoke(t, "r/x"), 0)
		assertSleeps(t, f.sleeps, time.Minute)
	})
	t.Run("bounds persistent 429 retries and fails loudly", func(t *testing.T) {
		f := newFixture(
			reply(http.StatusTooManyRequests, "", nil),
			reply(http.StatusTooManyRequests, "", nil),
			reply(http.StatusTooManyRequests, "", nil),
		)
		assertCode(t, f.invoke(t, "r/x", "--max-retries", "2"), 1)
		assertSleeps(t, f.sleeps, time.Minute, time.Minute)
		if got := len(f.transport.requests); got != 3 {
			t.Errorf("requests = %d, want 3", got)
		}
		assertNoStdout(t, f)
		assertStderr(t, f, "reddit-fetch: fetch failed for r/x: 429 Too Many Requests\n")
	})
	t.Run("default ceiling is 3 retries (4 fetches) then fails closed", func(t *testing.T) {
		f := newFixture(
			reply(http.StatusTooManyRequests, "", nil),
			reply(http.StatusTooManyRequests, "", nil),
			reply(http.StatusTooManyRequests, "", nil),
			reply(http.StatusTooManyRequests, "", nil),
		)
		assertCode(t, f.invoke(t, "r/x"), 1)
		assertSleeps(t, f.sleeps, time.Minute, time.Minute, time.Minute)
		if got := len(f.transport.requests); got != 4 {
			t.Errorf("requests = %d, want 4", got)
		}
		assertNoStdout(t, f)
	})
	t.Run("--max-retries 0 fetches once, never sleeps, and fails closed", func(t *testing.T) {
		f := newFixture(reply(http.StatusTooManyRequests, "", nil))
		assertCode(t, f.invoke(t, "r/x", "--max-retries", "0"), 1)
		assertSleeps(t, f.sleeps)
		if got := len(f.transport.requests); got != 1 {
			t.Errorf("requests = %d, want 1", got)
		}
		assertNoStdout(t, f)
	})
	t.Run("clamps Retry-After to the backoff band", func(t *testing.T) {
		tests := []struct {
			name   string
			header string
			want   time.Duration
		}{
			{name: "zero floors to one second", header: "0", want: time.Second},
			{name: "blank uses sixty seconds", header: "   ", want: time.Minute},
			{name: "huge value caps at five minutes", header: "86400", want: 5 * time.Minute},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				f := newFixture(reply(http.StatusTooManyRequests, "", retryAfterHeader(tt.header)), reply(http.StatusOK, "{}", nil))
				assertCode(t, f.invoke(t, "r/x"), 0)
				assertSleeps(t, f.sleeps, tt.want)
			})
		}
	})
}

// TestRunCancelled checks that a cancelled context stops the run at the delay between targets.
func TestRunCancelled(t *testing.T) {
	f := newFixture(reply(http.StatusOK, "A", nil), reply(http.StatusOK, "B", nil))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	code := run(ctx, deps{
		lookupEnv: func(string) (string, bool) { return "", false },
		stdout:    &f.stdout,
		stderr:    &f.stderr,
		client:    &http.Client{Transport: f.transport},
		sleep:     sleepContext,
	}, []string{"r/first", "r/second"})
	assertCode(t, code, 1)
	assertStderr(t, f, "reddit-fetch: request cancelled for r/second: context canceled\n")
	if got := len(f.transport.requests); got != 1 {
		t.Errorf("requests = %d, want 1 (cancel must stop before the second target)", got)
	}
}

func TestRunFailures(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(fmt.Sprintf("fails closed for HTTP %d", status), func(t *testing.T) {
			f := newFixture(reply(status, "", nil))
			assertCode(t, f.invoke(t, "r/x"), 1)
			assertNoStdout(t, f)
			assertStderr(t, f, fmt.Sprintf("reddit-fetch: fetch failed for r/x: %d %s\n", status, http.StatusText(status)))
		})
	}
	t.Run("keeps earlier bodies when a later target fails", func(t *testing.T) {
		f := newFixture(reply(http.StatusOK, "A", nil), reply(http.StatusForbidden, "", nil))
		assertCode(t, f.invoke(t, "r/a", "r/b"), 1)
		if got, want := f.stdout.String(), "A\n"; got != want {
			t.Errorf("stdout = %q, want %q", got, want)
		}
		assertSleeps(t, f.sleeps, 2*time.Second)
		assertStderr(t, f, "reddit-fetch: fetch failed for r/b: 403 Forbidden\n")
	})
	t.Run("body read failure fails closed without panicking", func(t *testing.T) {
		f := newFixture(failingBodyReply())
		assertCode(t, f.invoke(t, "r/x"), 1)
		assertNoStdout(t, f)
		assertStderr(t, f, "reddit-fetch: body read failed for r/x: body read failed\n")
	})
	t.Run("a failed request exits 1 with the request error", func(t *testing.T) {
		f := newFixture()
		assertCode(t, f.invoke(t, "r/x"), 1)
		if got := f.stderr.String(); !strings.HasPrefix(got, "reddit-fetch: request failed for r/x: ") {
			t.Errorf("stderr = %q, want a request failure", got)
		}
	})
	t.Run("bad arguments exit 2 with the usage line", func(t *testing.T) {
		f := newFixture()
		assertCode(t, f.invoke(t), 2)
		assertStderr(t, f, usageLine+"\n")
	})
	t.Run("a flag without a value exits 2 with its message", func(t *testing.T) {
		f := newFixture()
		assertCode(t, f.invoke(t, "--limit"), 2)
		assertStderr(t, f, "--limit needs a value\n")
	})
}

func TestUserAgent(t *testing.T) {
	setEnv := func(value string) func(string) (string, bool) {
		return func(string) (string, bool) { return value, true }
	}
	unset := func(string) (string, bool) { return "", false }
	tests := []struct {
		name   string
		p      parsed
		lookup func(string) (string, bool)
		want   string
	}{
		{name: "flag wins over the environment", p: parsed{userAgent: "flag"}, lookup: setEnv("env"), want: "flag"},
		{name: "environment is used without a flag", p: parsed{}, lookup: setEnv("env"), want: "env"},
		{name: "empty environment falls back to the default", p: parsed{}, lookup: setEnv(""), want: defaultUserAgent},
		{name: "empty flag falls back to the environment", p: parsed{userAgent: ""}, lookup: setEnv("env"), want: "env"},
		{name: "default when unset", p: parsed{}, lookup: unset, want: defaultUserAgent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := userAgent(tt.p, tt.lookup); got != tt.want {
				t.Errorf("userAgent() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildURL(t *testing.T) {
	limit := 10
	two := 2
	tests := []struct {
		name   string
		target string
		limit  *int
		want   string
	}{
		{name: "path only", target: "r/LocalLLaMA", want: "https://www.reddit.com/r/LocalLLaMA.json"},
		{name: "json goes before the query", target: "r/OpenAI/top?t=week", limit: &limit, want: "https://www.reddit.com/r/OpenAI/top.json?t=week&limit=10"},
		{name: "search with a small limit", target: "search?q=codex", limit: &two, want: "https://www.reddit.com/search.json?q=codex&limit=2"},
		{name: "limit without a query", target: "r/LocalLLaMA", limit: &limit, want: "https://www.reddit.com/r/LocalLLaMA.json?limit=10"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildURL(tt.target, tt.limit); got != tt.want {
				t.Errorf("buildURL(%q) = %q, want %q", tt.target, got, tt.want)
			}
		})
	}
}

func TestParseArgs(t *testing.T) {
	limit := 10
	tests := []struct {
		name string
		argv []string
		want parsed
	}{
		{
			name: "one target",
			argv: []string{"r/OpenAI"},
			want: parsed{targets: []string{"r/OpenAI"}, delayMs: 2000, maxRetries: 3},
		},
		{
			name: "multiple targets",
			argv: []string{"r/OpenAI", "search?q=codex"},
			want: parsed{targets: []string{"r/OpenAI", "search?q=codex"}, delayMs: 2000, maxRetries: 3},
		},
		{
			name: "numeric and user-agent flags",
			argv: []string{"r/x", "--limit", "10", "--delay-ms", "25", "--max-retries", "4", "--user-agent", "ua"},
			want: parsed{targets: []string{"r/x"}, limit: &limit, delayMs: 25, maxRetries: 4, userAgent: "ua"},
		},
		{
			name: "flags may precede the target",
			argv: []string{"--limit", "10", "r/x"},
			want: parsed{targets: []string{"r/x"}, limit: &limit, delayMs: 2000, maxRetries: 3},
		},
		{
			name: "an inline limit is allowed without --limit",
			argv: []string{"r/x?limit=5"},
			want: parsed{targets: []string{"r/x?limit=5"}, delayMs: 2000, maxRetries: 3},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseArgs(tt.argv)
			if err != nil {
				t.Fatalf("parseArgs(%q) error = %v", tt.argv, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseArgs(%q) = %+v, want %+v", tt.argv, got, tt.want)
			}
		})
	}
}

func TestParseArgsRejects(t *testing.T) {
	tests := []struct {
		name string
		argv []string
	}{
		{name: "no target", argv: nil},
		{name: "unknown long flag", argv: []string{"--unknown"}},
		{name: "unknown short flag", argv: []string{"-x"}},
		{name: "fragment in target", argv: []string{"r/x#frag"}},
		{name: "dot-dot segment", argv: []string{"r/x/../admin"}},
		{name: "space in target", argv: []string{"r/a b"}},
		{name: "empty numeric value", argv: []string{"r/x", "--limit", ""}},
		{name: "flag without a value", argv: []string{"r/x", "--limit"}},
		{name: "flag followed by a flag", argv: []string{"r/x", "--limit", "--delay-ms", "5"}},
		{name: "negative limit", argv: []string{"r/x", "--limit", "-1"}},
		{name: "fractional delay", argv: []string{"r/x", "--delay-ms", "1.5"}},
		{name: "empty user agent", argv: []string{"r/x", "--user-agent", ""}},
		{name: "limit flag with an inline limit", argv: []string{"r/x?limit=5", "--limit", "10"}},
		{name: "inline limit before the limit flag", argv: []string{"--limit", "10", "r/x?limit=5"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseArgs(tt.argv); err == nil {
				t.Errorf("parseArgs(%q) accepted, want error", tt.argv)
			}
		})
	}
}

func TestParseNonNegativeInteger(t *testing.T) {
	tests := []struct {
		value  string
		want   int
		wantOK bool
	}{
		{value: "0", want: 0, wantOK: true},
		{value: "-0", want: 0, wantOK: true},
		{value: "5.", want: 5, wantOK: true},
		{value: " 7 ", want: 7, wantOK: true},
		{value: "1e3", want: 1000, wantOK: true},
		{value: "0x10", want: 16, wantOK: true},
		{value: ".5"},
		{value: "-1"},
		{value: "1.5"},
		{value: "Infinity"},
		{value: "1_000"},
		{value: ""},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%q", tt.value), func(t *testing.T) {
			got, ok := parseNonNegativeInteger(tt.value)
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("parseNonNegativeInteger(%q) = %d, %t; want %d, %t", tt.value, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestJSNumber(t *testing.T) {
	tests := []struct {
		value  string
		want   float64
		wantOK bool
	}{
		{value: "10", want: 10, wantOK: true},
		{value: "+10", want: 10, wantOK: true},
		{value: "1.5", want: 1.5, wantOK: true},
		{value: "1e2", want: 100, wantOK: true},
		{value: "0x10", want: 16, wantOK: true},
		{value: "0X1f", want: 31, wantOK: true},
		{value: "0o17", want: 15, wantOK: true},
		{value: "0b101", want: 5, wantOK: true},
		{value: "Infinity", want: math.Inf(1), wantOK: true},
		{value: "-Infinity", want: math.Inf(-1), wantOK: true},
		{value: ""},
		{value: "0x"},
		{value: "1_000"},
		{value: "NaN"},
		{value: "12px"},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%q", tt.value), func(t *testing.T) {
			got, ok := jsNumber(tt.value)
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("jsNumber(%q) = %v, %t; want %v, %t", tt.value, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestValidateTarget(t *testing.T) {
	tests := []struct {
		arg string
		ok  bool
	}{
		{arg: "r/OpenAI/top?t=week", ok: true},
		{arg: "search?q=codex&sort=top", ok: true},
		{arg: "r/x?limit=5", ok: true},
		{arg: "/r/x"},
		{arg: "r/x/.."},
		{arg: "r/x/..?t=week"},
		{arg: "r/OpenAI/top.json?t=week"},
		{arg: "r/x/%2e%2e/%2e%2e/admin"},
		{arg: `r/x\..\admin`},
		{arg: "r/x/%zz"},
		{arg: "r/x?t=week#frag"},
		{arg: "r/x?t=week#a&limit=99"},
		{arg: "search?q=a b"},
		{arg: "?limit=5"},
		{arg: "r/OpenAI/top%2ejson?t=week"},
		{arg: "r/%FF"},
		{arg: "r/a\u00a0b"},
		{arg: "r/a\ufeffb"},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%q", tt.arg), func(t *testing.T) {
			if got := validateTarget(tt.arg) == nil; got != tt.ok {
				t.Errorf("validateTarget(%q) accepted = %t, want %t", tt.arg, got, tt.ok)
			}
		})
	}
}
