package main

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// mapRemotes makes https://fixture.invalid/<name> resolve to a local repository directory. Production code then
// validates a supported URL, and git reads the directory through the rewrite.
func mapRemotes(t *testing.T, remotes map[string]string) {
	t.Helper()
	// Porcelain add and commit start a detached `git maintenance run --auto` that can race object reads. Disable
	// it for every git child the test starts, including importFork and syncFork.
	pairs := [][2]string{{"maintenance.auto", "false"}, {"gc.auto", "0"}}
	for name, dir := range remotes {
		pairs = append(pairs, [2]string{"url." + dir + ".insteadOf", "https://fixture.invalid/" + name})
	}
	t.Setenv("GIT_CONFIG_COUNT", strconv.Itoa(len(pairs)))
	for index, pair := range pairs {
		t.Setenv("GIT_CONFIG_KEY_"+strconv.Itoa(index), pair[0])
		t.Setenv("GIT_CONFIG_VALUE_"+strconv.Itoa(index), pair[1])
	}
}

// requireRefusedRepo asserts that a record repo was refused before git ran it: the error names the refusal,
// and the marker file that an injected --upload-pack would create does not exist.
func requireRefusedRepo(t *testing.T, err error, marker string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "refused upstream repo") {
		t.Fatalf("error = %v, want a refused upstream repo", err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("option injection ran: marker stat = %v", statErr)
	}
}

// headRange returns the commits HEAD^ and HEAD of a repository.
func headRange(t *testing.T, root string) (start, end string) {
	t.Helper()
	start = strings.TrimSpace(string(gitOutput(t, root, "rev-parse", "HEAD^")))
	end = strings.TrimSpace(string(gitOutput(t, root, "rev-parse", "HEAD")))
	return start, end
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writeTestFile(t *testing.T, root, path, content string) {
	t.Helper()
	absolute := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(absolute), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(absolute, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func gitOutput(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return output
}

func captureStderr(t *testing.T, run func()) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stderr
	os.Stderr = writer
	writerClosed, readerClosed := false, false
	defer func() {
		os.Stderr = previous
		if !writerClosed {
			if err := writer.Close(); err != nil {
				t.Errorf("close stderr writer: %v", err)
			}
		}
		if !readerClosed {
			if err := reader.Close(); err != nil {
				t.Errorf("close stderr reader: %v", err)
			}
		}
	}()
	run()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	writerClosed = true
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	readerClosed = true
	return string(output)
}
