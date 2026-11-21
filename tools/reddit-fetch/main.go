// Command reddit-fetch fetches Reddit's unauthenticated .json endpoints for manual review.
// A failed run may leave earlier bodies on stdout, so the exit code says whether it completed.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	defaultUserAgent  = "rigel-model-eval-scout/0.1 (+https://rigel.build; model-selection sentiment scouting)"
	defaultDelayMs    = 2000
	defaultMaxRetries = 3
	usageLine         = "usage: reddit-fetch <target> [<target> ...] [--limit N] [--delay-ms N] [--max-retries N] [--user-agent UA]"

	// The 429 backoff is clamped so a server-controlled Retry-After cannot wedge or hot-loop the tool.
	minBackoffMs     = 1000
	maxBackoffMs     = 300000
	defaultBackoffMs = 60000

	// requestTimeout bounds one HTTP request so a stalled connection cannot hang the CLI.
	requestTimeout = 30 * time.Second
)

var (
	// decimalNumber matches the decimal spellings JavaScript's Number() accepts, plus Infinity.
	decimalNumber = regexp.MustCompile(`^[+-]?(?:Infinity|(?:\d+\.?\d*|\.\d+)(?:[eE][+-]?\d+)?)$`)
	// radixNumber matches the 0x, 0o and 0b integer spellings that Number() also accepts.
	radixNumber = regexp.MustCompile(`^0(?:[xX][0-9a-fA-F]+|[oO][0-7]+|[bB][01]+)$`)
	// limitQuery matches a limit parameter that a target already carries.
	limitQuery = regexp.MustCompile(`[?&]limit=`)
)

// parsed is a valid command line.
type parsed struct {
	targets    []string
	limit      *int
	delayMs    int
	maxRetries int
	userAgent  string
}

// deps are the process effects, injected so tests can stub the environment, network and clock.
type deps struct {
	lookupEnv func(string) (string, bool)
	stdout    io.Writer
	stderr    io.Writer
	client    *http.Client
	sleep     func(context.Context, time.Duration) error
}

// usageError marks a bad command line. It exits 2; a failed fetch exits 1.
type usageError struct{ err error }

func (e usageError) Error() string { return e.err.Error() }

func main() {
	// main is the process root, so it owns the root context and turns Ctrl-C into cancellation.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, deps{
		lookupEnv: os.LookupEnv,
		stdout:    os.Stdout,
		stderr:    os.Stderr,
		client:    &http.Client{Timeout: requestTimeout},
		sleep:     sleepContext,
	}, os.Args[1:])
	stop()
	os.Exit(code)
}

// run executes one command line and returns its exit code: 0 on success, 2 for bad arguments,
// and 1 for a failed fetch.
func run(ctx context.Context, d deps, args []string) int {
	err := execute(ctx, d, args)
	if err == nil {
		return 0
	}
	// A failed stderr write has nowhere left to report, so the exit code still carries the result.
	_, _ = fmt.Fprintln(d.stderr, err)
	if _, ok := errors.AsType[usageError](err); ok {
		return 2
	}
	return 1
}

// execute fetches each target in order and streams its body to stdout.
func execute(ctx context.Context, d deps, args []string) error {
	p, err := parseArgs(args)
	if err != nil {
		return usageError{err: err}
	}
	agent := userAgent(p, d.lookupEnv)
	for index, target := range p.targets {
		if index > 0 {
			if err := d.sleep(ctx, time.Duration(p.delayMs)*time.Millisecond); err != nil {
				return fmt.Errorf("reddit-fetch: request cancelled for %s: %w", target, err)
			}
		}
		body, err := fetchTarget(ctx, d, p, target, agent)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintln(d.stdout, body); err != nil {
			return fmt.Errorf("reddit-fetch: writing %s: %w", target, err)
		}
	}
	return nil
}

// userAgent picks the User-Agent: --user-agent, then REDDIT_USER_AGENT, then the default.
// An empty value at either level falls back, so a blank value never reaches the header (TS sent it blank).
func userAgent(p parsed, lookupEnv func(string) (string, bool)) string {
	if p.userAgent != "" {
		return p.userAgent
	}
	if value, ok := lookupEnv("REDDIT_USER_AGENT"); ok && value != "" {
		return value
	}
	return defaultUserAgent
}

// fetchTarget returns one target's body, retrying 429 responses up to maxRetries times.
func fetchTarget(ctx context.Context, d deps, p parsed, target, agent string) (string, error) {
	address := buildURL(target, p.limit)
	for attempt := 0; ; attempt++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
		if err != nil {
			return "", fmt.Errorf("reddit-fetch: request failed for %s: %w", target, err)
		}
		request.Header.Set("User-Agent", agent)
		response, err := d.client.Do(request)
		if err != nil {
			return "", fmt.Errorf("reddit-fetch: request failed for %s: %w", target, err)
		}
		if response.StatusCode != http.StatusTooManyRequests || attempt == p.maxRetries {
			return responseBody(target, response)
		}
		delay := retryAfter(response)
		// The 429 body is discarded unread, so its close error has no use here.
		_ = response.Body.Close()
		if err := d.sleep(ctx, delay); err != nil {
			return "", fmt.Errorf("reddit-fetch: request cancelled for %s: %w", target, err)
		}
	}
}

// responseBody returns the body of a 2xx response, and an error for any other status.
func responseBody(target string, response *http.Response) (string, error) {
	// Any 2xx counts as success, as the TypeScript response.ok check did.
	if response.StatusCode < 200 || response.StatusCode > 299 {
		// The error body is never read, so its close error has no use here.
		_ = response.Body.Close()
		return "", fmt.Errorf("reddit-fetch: fetch failed for %s: %s", target, response.Status)
	}
	body, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return "", fmt.Errorf("reddit-fetch: body read failed for %s: %w", target, err)
	}
	return string(body), nil
}

// retryAfter returns the 429 backoff. A blank, absent or malformed header waits the default,
// and a valid value is clamped into the backoff band.
func retryAfter(response *http.Response) time.Duration {
	seconds, ok := jsNumber(trimJSSpace(response.Header.Get("Retry-After")))
	if !ok || math.IsInf(seconds, 0) || seconds < 0 {
		return defaultBackoffMs * time.Millisecond
	}
	ms := min(max(seconds*1000, minBackoffMs), maxBackoffMs)
	return time.Duration(ms * float64(time.Millisecond))
}

// sleepContext waits for d, or returns early with ctx's error when ctx is cancelled.
func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// buildURL returns the Reddit .json URL for target, with limit appended when it is set.
func buildURL(target string, limit *int) string {
	path, query, hasQuery := strings.Cut(target, "?")
	suffix := path + ".json"
	if hasQuery {
		suffix += "?" + query
	}
	if limit == nil {
		return "https://www.reddit.com/" + suffix
	}
	separator := "?"
	if hasQuery {
		separator = "&"
	}
	return fmt.Sprintf("https://www.reddit.com/%s%slimit=%d", suffix, separator, *limit)
}

// parseArgs validates argv into a request. Flags and targets may appear in any order.
func parseArgs(argv []string) (parsed, error) {
	p := parsed{delayMs: defaultDelayMs, maxRetries: defaultMaxRetries}
	// The loop advances i past each flag's value, so it stays a classic for loop.
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		switch arg {
		case "--limit", "--delay-ms", "--max-retries", "--user-agent":
			if i+1 >= len(argv) || strings.HasPrefix(argv[i+1], "--") {
				return parsed{}, fmt.Errorf("%s needs a value", arg)
			}
			i++
			if err := setFlag(&p, arg, argv[i]); err != nil {
				return parsed{}, err
			}
		default:
			if strings.HasPrefix(arg, "-") {
				return parsed{}, fmt.Errorf("unknown flag: %s", arg)
			}
			if err := validateTarget(arg); err != nil {
				return parsed{}, err
			}
			p.targets = append(p.targets, arg)
		}
	}
	if len(p.targets) == 0 {
		return parsed{}, errors.New(usageLine)
	}
	// --limit and an inline limit would both reach the query, and Reddit honors the first,
	// so the flag would be silently ignored. Reject the ambiguity.
	if p.limit != nil && slices.ContainsFunc(p.targets, limitQuery.MatchString) {
		return parsed{}, errors.New("--limit given but a target already carries a limit query")
	}
	return p, nil
}

// setFlag stores one flag's value on p, and rejects a value the flag cannot take.
func setFlag(p *parsed, flag, value string) error {
	if flag == "--user-agent" {
		if value == "" {
			return errors.New("--user-agent needs a value")
		}
		p.userAgent = value
		return nil
	}
	n, ok := parseNonNegativeInteger(value)
	if !ok {
		return fmt.Errorf("%s needs a non-negative integer", flag)
	}
	switch flag {
	case "--limit":
		p.limit = &n
	case "--delay-ms":
		p.delayMs = n
	default:
		p.maxRetries = n
	}
	return nil
}

// parseNonNegativeInteger parses a trimmed, non-negative whole number in JavaScript Number() spelling.
func parseNonNegativeInteger(value string) (int, bool) {
	n, ok := jsNumber(trimJSSpace(value))
	if !ok || n < 0 || n != math.Trunc(n) || n >= math.MaxInt {
		return 0, false
	}
	return int(n), true
}

// jsNumber parses value with the grammar of JavaScript's Number(), so flag and header values
// accept the spellings the TypeScript tool did.
func jsNumber(value string) (float64, bool) {
	if radixNumber.MatchString(value) {
		base := map[string]int{"x": 16, "o": 8, "b": 2}[strings.ToLower(value[1:2])]
		n, err := strconv.ParseUint(value[2:], base, 64)
		return float64(n), err == nil
	}
	if !decimalNumber.MatchString(value) {
		return 0, false
	}
	n, err := strconv.ParseFloat(value, 64)
	return n, err == nil
}

// validateTarget rejects a target whose fetched path could escape, or whose fragment or
// whitespace could swallow the appended limit. buildURL fetches only the part before "?",
// so that part is checked too.
func validateTarget(arg string) error {
	rawPath, _, _ := strings.Cut(arg, "?")
	decoded, err := url.PathUnescape(rawPath)
	if err != nil || !utf8.ValidString(decoded) ||
		strings.HasPrefix(arg, "/") ||
		rawPath == "" ||
		strings.HasSuffix(rawPath, ".json") ||
		strings.HasSuffix(decoded, ".json") ||
		hasFragmentOrSpace(arg) ||
		hasFragmentOrSpace(decoded) ||
		hasDotDotSegment(decoded) {
		return fmt.Errorf("invalid target: %s", arg)
	}
	return nil
}

// isJSSpace reports whether r is in JavaScript's whitespace set, which \s and trim() both use.
func isJSSpace(r rune) bool {
	// JavaScript includes U+FEFF and excludes U+0085, the reverse of unicode.IsSpace for U+0085.
	return r == 0xFEFF || (r != 0x85 && unicode.IsSpace(r))
}

// trimJSSpace trims the characters that JavaScript's trim() removes.
func trimJSSpace(s string) string {
	return strings.TrimFunc(s, isJSSpace)
}

// hasFragmentOrSpace reports whether s has a '#' or a JavaScript whitespace character.
func hasFragmentOrSpace(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r == '#' || isJSSpace(r) })
}

// hasDotDotSegment reports whether decoded has a ".." segment, split on both / and \.
func hasDotDotSegment(decoded string) bool {
	segments := strings.FieldsFunc(decoded, func(r rune) bool { return r == '/' || r == '\\' })
	return slices.Contains(segments, "..")
}
