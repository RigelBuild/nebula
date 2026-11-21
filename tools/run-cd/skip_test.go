package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/RigelBuild/nebula/tools/go-lib/cdgate"
)

func TestNewRunSkipWritesOutputs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "output")
	getenv := func(name string) string {
		if name == "CI_STEP_OUTPUT" {
			return path
		}
		return ""
	}
	var log bytes.Buffer
	r := mustNewRun(t, options{}, getenv, &log)
	r.Skip(cdgate.Verdict{Reason: cdgate.ReasonSuperseded, By: 9})
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "deploy.state=skipped\ndeploy.reason=superseded\n" {
		t.Fatalf("output = %q", data)
	}
	path = dir
	r.Skip(cdgate.Verdict{Reason: cdgate.ReasonRestartNotHead})
	if !bytes.Contains(log.Bytes(), []byte("step output: failed to append")) {
		t.Fatalf("log = %q", log.String())
	}
}
