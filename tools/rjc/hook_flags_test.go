package main

import (
	"bytes"
	"testing"
)

func TestSkipFlagsExclusive(t *testing.T) {
	var out bytes.Buffer
	code := run(t.Context(), []string{"submit", "--title", "feat: test", "--skip-checks", "artifact:EX-1", "--skip-target", "a:test", "--reason", "EX-1"}, &out, &out)
	if code != 2 {
		t.Fatalf("got code %d: %s", code, out.String())
	}
}

func TestEmptySkipReasonRefusesAtSkipReason(t *testing.T) {
	var output bytes.Buffer
	code := run(t.Context(), []string{"submit", "--title", "feat: x", "--skip-checks", ""}, &output, &output)
	if code != 1 || !bytes.Contains(output.Bytes(), []byte("metadata: skip-reason")) {
		t.Fatalf("exit=%d output=%s, want skip-reason refusal", code, output.String())
	}
}
