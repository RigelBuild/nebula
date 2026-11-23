package transfer

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// printedFallback returns the staging command as the operator is told to run
// it, with the sudo word removed so the test can run it unprivileged. Only that
// word is removed; the rest is the printed text, byte for byte.
func printedFallback(t *testing.T, opts Options) string {
	t.Helper()
	printed := stageCredentialsCommand(opts)
	const sudo = "sudo "
	if !strings.Contains(printed, sudo) {
		t.Fatalf("printed fallback has no sudo stage: %q", printed)
	}
	return strings.Replace(printed, sudo, "", 1)
}

// F1: the printed fallback must be the whole pipeline. Running only the root
// tar streamer dumps every refresh token to the terminal and stages nothing.
func TestPrintedFallbackStagesToken(t *testing.T) {
	requireDarwin(t)
	home := t.TempDir()
	stage := filepath.Join(home, ".local", "state", "nemo-move-staging")
	source := fixtureSource(t, `{"refresh":"fallback"}`, "workspace")
	opts := Options{SourceHome: home, MacUser: "example-user", Layout: testLayout}
	opts.Layout.Google.Accounts = []string{"workspace"}
	opts.Layout.Google.Source = source
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", printedFallback(t, opts))
	cmd.Env = append(os.Environ(), "HOME="+home)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("printed fallback failed: %v\n%s", err, out)
	}
	token := filepath.Join(stage, "workspace", "credentials", "token.json")
	got, err := os.ReadFile(token)
	if err != nil {
		t.Fatalf("printed fallback staged no token: %v", err)
	}
	if string(got) != `{"refresh":"fallback"}` {
		t.Fatalf("staged token = %q", got)
	}
}

// F2: a read error part-way through the root stream must fail the run. Without
// pipefail the unpack exits 0 and a partial tree is staged as fresh.
func TestStagingFailsOnUnreadableSource(t *testing.T) {
	requireDarwin(t)
	home := t.TempDir()
	stage := filepath.Join(home, ".local", "state", "nemo-move-staging")
	source := fixtureSource(t, `{"refresh":"new"}`, "workspace")
	unreadable := filepath.Join(source, "workspace", "credentials", "locked.json")
	if err := os.WriteFile(unreadable, []byte("{}"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o600) })
	if _, err := rootHalf(t, source, "workspace"); err == nil {
		t.Fatal("root half read an unreadable source file without error")
	}
	pipeline := stagePipelineFor(t, home, source, "workspace")
	if err := runPipeline(t, home, pipeline); err == nil {
		t.Fatal("pipeline exited 0 after an unreadable source file; pipefail is missing")
	}
	if _, err := os.Stat(filepath.Join(stage, "workspace", "credentials")); !os.IsNotExist(err) {
		t.Fatalf("partial tree was staged after a failed read: %v", err)
	}
}

// F2: an account whose credentials directory is empty must not stage as fresh.
func TestStagingRefusesEmptyCredentials(t *testing.T) {
	requireDarwin(t)
	home := t.TempDir()
	stage := filepath.Join(home, ".local", "state", "nemo-move-staging")
	source := filepath.Join(t.TempDir(), "accounts")
	if err := os.MkdirAll(filepath.Join(source, "workspace", "credentials"), 0o700); err != nil {
		t.Fatal(err)
	}
	pipeline := stagePipelineFor(t, home, source, "workspace")
	if err := runPipeline(t, home, pipeline); err == nil {
		t.Fatal("pipeline staged an empty credentials directory")
	}
	if _, err := os.Stat(filepath.Join(stage, "workspace", "credentials")); !os.IsNotExist(err) {
		t.Fatalf("empty credentials were staged: %v", err)
	}
}

// F3: the fixture must start loose, so the chmod steps can fail. A 0755/0644
// tree with a nested dir must come out 700 and 600 at every level.
func TestStagingNormalizesModesOfNestedTree(t *testing.T) {
	requireDarwin(t)
	home := t.TempDir()
	stage := filepath.Join(home, ".local", "state", "nemo-move-staging")
	source := filepath.Join(t.TempDir(), "accounts")
	nested := filepath.Join(source, "workspace", "credentials", "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "token.json"), []byte(`{"refresh":"loose"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(source, "workspace", "credentials"), 0o755); err != nil {
		t.Fatal(err)
	}
	archive, err := rootHalf(t, source, "workspace")
	if err != nil {
		t.Fatalf("root half failed: %v", err)
	}
	if err := unpackStagingAsUser(t, home, stage, archive, "workspace"); err != nil {
		t.Fatalf("user unpack failed: %v", err)
	}
	assertMode(t, filepath.Join(stage, "workspace"), os.Getuid(), 0o700)
	assertMode(t, filepath.Join(stage, "workspace", "credentials"), os.Getuid(), 0o700)
	assertMode(t, filepath.Join(stage, "workspace", "credentials", "nested"), os.Getuid(), 0o700)
	assertMode(t, filepath.Join(stage, "workspace", "credentials", "nested", "token.json"), os.Getuid(), 0o600)
}

// stagePipelineFor builds the production pipeline for one account, with the
// Google source redirected to source and staging rooted in home. The root stage
// runs without sudo so the test is unprivileged; the unpack stage is unchanged.
func stagePipelineFor(t *testing.T, home, source string, accounts ...string) string {
	t.Helper()
	opts := Options{SourceHome: home, MacUser: "example-user", Layout: testLayout}
	opts.Layout.Google.Accounts = accounts
	opts.Layout.Google.Source = source
	pipeline := stagePipeline(opts)
	pipeline = strings.Replace(pipeline, "/usr/bin/sudo -n ", "", 1)
	return pipeline
}

// runPipeline runs a pipeline under /bin/sh with the test HOME, returning its
// error. It uses the same bash pipefail the production command must use.
func runPipeline(t *testing.T, home, pipeline string) error {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("pipeline staging runs on macOS")
	}
	cmd := exec.CommandContext(t.Context(), "/bin/bash", "-o", "pipefail", "-c", pipeline)
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return &pipelineError{err: err, out: string(out)}
	}
	return nil
}

// A credential file name with a space must stage when readable and refuse when
// not. A word split on the name would refuse the readable file as two missing
// paths, so the readable case is the one that proves the name is kept whole.
func TestStagingHandlesSpacedCredentialNames(t *testing.T) {
	requireDarwin(t)
	t.Run("readable", func(t *testing.T) {
		home := t.TempDir()
		stage := filepath.Join(home, ".local", "state", "nemo-move-staging")
		source := fixtureSource(t, `{"refresh":"new"}`, "workspace")
		spaced := filepath.Join(source, "workspace", "credentials", "my token.json")
		if err := os.WriteFile(spaced, []byte(`{"refresh":"spaced"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		pipeline := stagePipelineFor(t, home, source, "workspace")
		if err := runPipeline(t, home, pipeline); err != nil {
			t.Fatalf("readable spaced-name credential was refused: %v", err)
		}
		got, err := os.ReadFile(filepath.Join(stage, "workspace", "credentials", "my token.json"))
		if err != nil {
			t.Fatalf("readable spaced-name credential was not staged: %v", err)
		}
		if string(got) != `{"refresh":"spaced"}` {
			t.Fatalf("staged spaced-name credential = %q", got)
		}
	})
	t.Run("unreadable", func(t *testing.T) {
		home := t.TempDir()
		stage := filepath.Join(home, ".local", "state", "nemo-move-staging")
		source := fixtureSource(t, `{"refresh":"new"}`, "workspace")
		spaced := filepath.Join(source, "workspace", "credentials", "my token.json")
		if err := os.WriteFile(spaced, []byte("{}"), 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(spaced, 0o600) })
		pipeline := stagePipelineFor(t, home, source, "workspace")
		if err := runPipeline(t, home, pipeline); err == nil {
			t.Fatal("pipeline staged an unreadable credential whose name has a space")
		}
		if _, err := os.Stat(filepath.Join(stage, "workspace", "credentials")); !os.IsNotExist(err) {
			t.Fatalf("spaced-name credentials were staged: %v", err)
		}
	})
}

type pipelineError struct {
	err error
	out string
}

func (e *pipelineError) Error() string { return e.err.Error() + ": " + e.out }
