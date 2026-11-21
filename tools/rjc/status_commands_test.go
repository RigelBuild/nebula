package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestStatusCLIEmptyJSON(t *testing.T) {
	f := fixtureRepo(t)
	var out, errOut bytes.Buffer
	code := run(f.ctx, []string{"status", "--all", "--repo", "example/app", "-o", "json"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit=%d error=%s", code, errOut.String())
	}
	var rows []StatusRow
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil || rows == nil || len(rows) != 0 {
		t.Fatalf("rows=%v error=%v output=%s", rows, err, out.String())
	}
}
