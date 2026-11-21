package stepoutput

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteAppendsWhenPathIsSetAndIsNoOpWhenUnset(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  bool
	}{{"set", true}, {"unset", false}} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "output")
			var stderr bytes.Buffer
			getenv := func(string) string {
				if tc.set {
					return path
				}
				return ""
			}
			Write(getenv, &stderr, "deploy.state", "started")
			Write(getenv, &stderr, "deploy.state", "completed")
			data, err := os.ReadFile(path)
			if !tc.set {
				if !os.IsNotExist(err) {
					t.Fatalf("unset: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != "deploy.state=started\ndeploy.state=completed\n" {
				t.Fatalf("output = %q", data)
			}
		})
	}
}

func TestWriteLogsFailedAppendAndKeepsGoing(t *testing.T) {
	var stderr bytes.Buffer
	getenv := func(string) string { return t.TempDir() }
	Write(getenv, &stderr, "deploy.state", "started")
	if !strings.Contains(stderr.String(), "step output: failed to append") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}
