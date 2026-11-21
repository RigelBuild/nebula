package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestMainWithRejectsWrongArgs(t *testing.T) {
	var out, errOut bytes.Buffer
	code := mainWith(context.Background(), []string{"only-one"}, &out, &errOut)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if got := strings.TrimSpace(errOut.String()); got != usage {
		t.Errorf("stderr = %q, want usage", got)
	}
}
