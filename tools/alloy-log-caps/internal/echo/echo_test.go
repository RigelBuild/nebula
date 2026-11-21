package echo

import (
	"math"
	"testing"
	"time"
)

func TestParseGroupsByStream(t *testing.T) {
	log := `ts=2026-10-15T17:00:00Z component_id=loki.echo.e msg="x" entry="hello" stream_id=\"s1\"` + "\n" +
		`ts=2026-10-15T17:00:01Z component_id=loki.echo.e msg="x" entry="a\\nb" stream_id=\"s1\"` + "\n" +
		"unrelated line\n"
	got, err := Parse([]byte(log))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if n := len(got.Lines["s1"]); n != 2 {
		t.Fatalf("s1 lines = %d, want 2", n)
	}
	if got.Lines["s1"][0] != "hello" {
		t.Errorf("first entry = %q, want hello", got.Lines["s1"][0])
	}
	want, err := time.Parse(time.RFC3339Nano, "2026-10-15T17:00:00Z")
	if err != nil {
		t.Fatalf("parse want time: %v", err)
	}
	if got.At["s1"][0] != float64(want.UnixNano())/1e9 {
		t.Errorf("first at = %v, want %v", got.At["s1"][0], float64(want.UnixNano())/1e9)
	}
}

func TestParseReceiverTag(t *testing.T) {
	log := `ts=2026-10-15T17:00:00Z receiver=loki.echo.e msg="x" entry="hi" stream_id=\"s2\"` + "\n"
	got, err := Parse([]byte(log))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if n := got.Lines["s2"]; len(n) != 1 || n[0] != "hi" {
		t.Errorf("s2 lines = %q, want [hi]", n)
	}
}

func TestParseUnknownStreamIDAndBadTimestamp(t *testing.T) {
	log := `ts=bad component_id=loki.echo.e msg="x" entry="zz"` + "\n"
	got, err := Parse([]byte(log))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if n := got.Lines["?"]; len(n) != 1 || n[0] != "zz" {
		t.Fatalf("? lines = %q, want [zz]", n)
	}
	if at := got.At["?"]; len(at) != 1 || !math.IsNaN(at[0]) {
		t.Errorf("? at = %v, want one NaN", at)
	}
}

func TestParseRejectsBadEscape(t *testing.T) {
	log := `ts=2026-10-15T17:00:00Z component_id=loki.echo.e msg="x" entry="\q" stream_id=\"s1\"` + "\n"
	if _, err := Parse([]byte(log)); err == nil {
		t.Error("Parse accepted an invalid escape")
	}
}
