package cdgate

import (
	"strconv"
	"testing"

	"github.com/RigelBuild/nebula/tools/go-lib/woodpecker"
)

func TestParsePositiveIntAcceptsWholeNumbers(t *testing.T) {
	for in, want := range map[string]int64{"1": 1, "42": 42, "9007199254740991": woodpecker.MaxSafeInteger} {
		got, err := ParsePositiveInt(in, "CI_PIPELINE_NUMBER")
		if err != nil || got != want {
			t.Fatalf("ParsePositiveInt(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
}

func TestParsePositiveIntRejectsZeroSignsFractionsAndSpaces(t *testing.T) {
	for _, in := range []string{"", "0", "-1", "+1", "1.5", "1e3", " 7", "7 ", "0x10"} {
		if _, err := ParsePositiveInt(in, "CI_PIPELINE_NUMBER"); err == nil || err.Error() != "invalid CI_PIPELINE_NUMBER" {
			t.Fatalf("ParsePositiveInt(%q) err = %v, want the invalid-number error", in, err)
		}
	}
}

func TestParsePositiveIntRejectsBeyondSafeInteger(t *testing.T) {
	beyond := strconv.FormatInt(woodpecker.MaxSafeInteger+1, 10)
	if _, err := ParsePositiveInt(beyond, "CI_PIPELINE_NUMBER"); err == nil || err.Error() != "invalid CI_PIPELINE_NUMBER" {
		t.Fatalf("ParsePositiveInt(%s) err = %v, want the invalid-number error", beyond, err)
	}
}

func TestParseNonNegativeIntAllowsZeroForNoParent(t *testing.T) {
	got, err := ParseNonNegativeInt("0", "CI_PIPELINE_PARENT")
	if err != nil || got != 0 {
		t.Fatalf("ParseNonNegativeInt(0) = %d, %v; want 0, nil: zero means no parent", got, err)
	}
}

func TestParseNonNegativeIntRejectsNegative(t *testing.T) {
	if _, err := ParseNonNegativeInt("-1", "CI_PIPELINE_PARENT"); err == nil || err.Error() != "invalid CI_PIPELINE_PARENT" {
		t.Fatalf("err = %v, want the invalid-parent error", err)
	}
}

// envOf returns a lookup over a fixed map, as the CI environment would.
func envOf(m map[string]string) func(string) string {
	return func(name string) string { return m[name] }
}

func TestOwnFromEnvReadsPipelineIdentity(t *testing.T) {
	env := envOf(map[string]string{
		"CI_PIPELINE_NUMBER": "20",
		"CI_PIPELINE_PARENT": "0",
		"CI_COMMIT_SHA":      "  " + shaB + "  ",
	})
	own, err := OwnFromEnv(env, "aws:up")
	if err != nil {
		t.Fatalf("OwnFromEnv: %v", err)
	}
	if own.Number != 20 || own.Parent != 0 || own.Commit != shaB || own.Target != "aws:up" {
		t.Fatalf("own = %+v", own)
	}
}

func TestOwnFromEnvRequiresCommit(t *testing.T) {
	env := envOf(map[string]string{"CI_PIPELINE_NUMBER": "20", "CI_PIPELINE_PARENT": "0"})
	if _, err := OwnFromEnv(env, "aws:up"); err == nil || err.Error() != "missing CI_COMMIT_SHA" {
		t.Fatalf("err = %v, want the missing-commit error", err)
	}
}

func TestOwnFromEnvRejectsBadPipelineNumber(t *testing.T) {
	env := envOf(map[string]string{"CI_PIPELINE_NUMBER": "x", "CI_PIPELINE_PARENT": "0", "CI_COMMIT_SHA": shaB})
	if _, err := OwnFromEnv(env, "aws:up"); err == nil || err.Error() != "invalid CI_PIPELINE_NUMBER" {
		t.Fatalf("err = %v, want the invalid-number error", err)
	}
}
