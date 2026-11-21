// SPDX-License-Identifier: MIT OR Apache-2.0

package transientretry

import (
	"errors"
	"regexp"
	"slices"
	"strings"

	"github.com/dlclark/regexp2/v2"
)

// PulumiTransient is the retry class for Pulumi up failures before resource steps.
const PulumiTransient Class = "pulumi-transient"

// RegistryTransient is the retry class for recoverable registry failures.
const RegistryTransient Class = "registry-transient"

// DiagnosticError is the structured failure a Pulumi or registry step reports.
// Its fields mirror the process output and retry boundaries callers know.
type DiagnosticError struct {
	Message string
	Stderr  string
	Stdout  string
	// ResourceStepsStarted marks a Pulumi failure after resource changes began.
	ResourceStepsStarted bool
	// HitLocalCeiling marks an operation timeout that the registry must not retry.
	HitLocalCeiling bool
	// RetryableTimeout marks a per-attempt timeout that may be retried.
	RetryableTimeout bool
}

func (d *DiagnosticError) Error() string { return d.Message }

func text(err error) string {
	if d := diagnostic(err); d != nil {
		return strings.Join([]string{d.Message, d.Stderr, d.Stdout}, "\n")
	}
	if err == nil {
		return ""
	}
	return err.Error()
}

func diagnostic(err error) *DiagnosticError {
	d, _ := errors.AsType[*DiagnosticError](err)
	return d
}

var (
	networkFailure   = mustRegexp2(`use of closed network connection|connection reset by peer|broken pipe|unexpected EOF|i\/o timeout|\b(?:ECONNRESET|ECONNREFUSED|ETIMEDOUT|EAI_AGAIN|ENETUNREACH|EHOSTUNREACH|ECONNABORTED|DNSResolveFailed|ConnectionClosed)\b|socket hang up|network (?:error|drop)|fetch failed|temporary failure in name resolution|TLS handshake timeout|context deadline exceeded`)
	http429          = mustRegexp2(`\bHTTP\s*[:=]?\s*429\b|\bstatus(?:code)?\s*[:=]?\s*429\b|\b429\s+too many requests\b`)
	registryThrottle = mustRegexp2(`\bunexpected status(?: code)?(?: from \S+ request to \S+)?:?\s+429\b|\btoomanyrequests\b|\btoo many requests\b`)
	http5xx          = mustRegexp2(`\bHTTP\s*[:=]?\s*5\d\d\b|\bstatus(?:code)?\s*[:=]?\s*5\d\d\b|\bserver returned\s+5\d\d\b|\bStatusCode:\s*5\d\d\b|\bunexpected status(?: code)?(?: from \S+ request to \S+)?:?\s+5\d\d\s+[A-Z]`)
	timeoutText      = mustRegexp2(`\b(?:timed? ?out|timeout|ETIMEDOUT)\b|deadline exceeded`)
	terminalRegistry = mustRegexp2(`\b(?:HTTP|status(?:code)?)\s*[:=]?\s*4(?!29)\d\d\b|\b4(?!29)\d\d\s+[A-Z][a-z]+\b|\b(?:unauthorized|forbidden|denied|permission_denied)\b`)
	resourceRow      = mustRegexp2(`(?m)^\s*(?:\+\+|--|\+-|~\+|>>|<<|=>|[~+\-=<>])\s*(?:[\w./-]+:){2}[\w./-]+\s+\S+`)
	resourceEvent    = mustRegexp2(`"(?:resourcePreEvent|resourceOutputsEvent|resourceOperationFailed|resourceOperationSucceeded)"\s*:`)
	resourcePhrase   = mustRegexp2(`\bresource (?:step|operation) (?:has )?started\b`)
	fatalLinePattern = regexp.MustCompile(`level=fatal`)
)

func fatalFailureLine(s string) (string, bool) {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	for _, line := range slices.Backward(lines) {
		if fatalLinePattern.MatchString(line) {
			return line, true
		}
	}
	return "", false
}

func lastFailureLine(s string) (string, bool) {
	if fatal, ok := fatalFailureLine(s); ok {
		return fatal, true
	}
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	for _, line := range slices.Backward(lines) {
		if strings.TrimSpace(line) != "" {
			return line, true
		}
	}
	return "", false
}

func hitLocalCeiling(err error) bool {
	d := diagnostic(err)
	return d != nil && d.HitLocalCeiling
}

func mustRegexp2(pattern string) *regexp2.Regexp {
	return regexp2.MustCompile(pattern, regexp2.IgnoreCase)
}

func matches(re *regexp2.Regexp, s string) bool {
	ok, err := re.MatchString(s)
	return err == nil && ok
}

func resourceStepStarted(err error, output string) bool {
	if d := diagnostic(err); d != nil && d.ResourceStepsStarted {
		return true
	}
	return matches(resourceRow, output) || matches(resourceEvent, output) || matches(resourcePhrase, output)
}

// ClassifyPulumiTransient retries a Pulumi failure that is a 429 or network
// error before any resource step started.
func ClassifyPulumiTransient(err error) Class {
	if err == nil || hitLocalCeiling(err) {
		return ""
	}
	all := text(err)
	if resourceStepStarted(err, all) {
		return ""
	}
	cause, ok := lastFailureLine(all)
	if !ok {
		cause = all
	}
	if resourceStepStarted(err, cause) {
		return ""
	}
	if matches(http429, all) || matches(networkFailure, cause) {
		return PulumiTransient
	}
	return ""
}

// ClassifyRegistryTransient retries registry throttling, 5xx, timeouts, and network failures.
func ClassifyRegistryTransient(err error) Class {
	if err == nil || hitLocalCeiling(err) {
		return ""
	}
	all := text(err)
	if fatal, ok := fatalFailureLine(all); ok {
		if matches(registryThrottle, fatal) || matches(http429, fatal) ||
			matches(http5xx, fatal) || matches(timeoutText, fatal) || matches(networkFailure, fatal) {
			return RegistryTransient
		}
		return ""
	}
	cause, ok := lastFailureLine(all)
	if ok && matches(terminalRegistry, cause) {
		return ""
	}
	if d := diagnostic(err); d != nil && d.RetryableTimeout {
		return RegistryTransient
	}
	if ok && (matches(registryThrottle, cause) || matches(http429, cause) ||
		matches(http5xx, cause) || matches(timeoutText, cause) || matches(networkFailure, cause)) {
		return RegistryTransient
	}
	return ""
}
