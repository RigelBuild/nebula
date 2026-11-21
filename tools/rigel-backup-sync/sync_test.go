// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var testGitEnvironment = map[string]string{
	"GIT_AUTHOR_NAME":     "t",
	"GIT_AUTHOR_EMAIL":    "t@example.com",
	"GIT_COMMITTER_NAME":  "t",
	"GIT_COMMITTER_EMAIL": "t@example.com",
	"GIT_CONFIG_GLOBAL":   "/dev/null",
	"GIT_CONFIG_NOSYSTEM": "1",
}

func runGit(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	command := exec.CommandContext(t.Context(), "git", args...)
	command.Dir = cwd
	command.Env = mergedEnvironment(os.Environ(), testGitEnvironment)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

type backupFixture struct {
	root     string
	upstream string
	mirror   string
	work     string
}

func newBackupFixture(t *testing.T) backupFixture {
	t.Helper()
	root := t.TempDir()
	fixture := backupFixture{
		root:     root,
		upstream: filepath.Join(root, "factory.git"),
		mirror:   filepath.Join(root, "mirror.git"),
		work:     filepath.Join(root, "work"),
	}
	runGit(t, root, "init", "-q", "--bare", "-b", "main", fixture.upstream)
	runGit(t, root, "init", "-q", "--bare", "-b", "main", fixture.mirror)
	runGit(t, root, "init", "-q", "-b", "main", fixture.work)
	return fixture
}

func (f backupFixture) commit(t *testing.T, message, remote string) string {
	t.Helper()
	if remote == "" {
		remote = f.upstream
	}
	runGit(t, f.work, "commit", "-q", "--allow-empty", "-m", message)
	runGit(t, f.work, "push", "-q", remote, "main")
	return runGit(t, f.work, "rev-parse", "HEAD")
}

func (f backupFixture) mirrorMain(t *testing.T) string {
	t.Helper()
	return runGit(t, f.root, "--git-dir", f.mirror, "rev-parse", "refs/heads/main")
}

func (f backupFixture) syncConfig(upstream string) syncConfig {
	return syncConfig{mirror: f.mirror, upstream: upstream, env: testGitEnvironment}
}
func TestReadSyncConfig(t *testing.T) {
	getenv := func(name string) string {
		return map[string]string{
			"RIGEL_BACKUP_KEY_FILE":    "/keys/example-deploy-key",
			"RIGEL_BACKUP_KNOWN_HOSTS": "/etc/ssh/ssh_known_hosts",
			"RIGEL_BACKUP_MIRROR":      "/srv/example/mirror.git",
			"RIGEL_BACKUP_UPSTREAM":    "git@example.com:example/upstream.git",
		}[name]
	}

	config, err := readSyncConfig(getenv)
	if err != nil {
		t.Fatal(err)
	}
	if config.mirror != "/srv/example/mirror.git" || config.upstream != "git@example.com:example/upstream.git" {
		t.Errorf("mirror/upstream = %q/%q", config.mirror, config.upstream)
	}
	wantEnv := map[string]string{
		"GIT_SSH_COMMAND":     "ssh -i /keys/example-deploy-key -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile=/etc/ssh/ssh_known_hosts -o BatchMode=yes",
		"GIT_TERMINAL_PROMPT": "0",
	}
	if !maps.Equal(config.env, wantEnv) {
		t.Errorf("environment = %#v, want %#v", config.env, wantEnv)
	}
}

func TestReadSyncConfigRequiresEnvironment(t *testing.T) {
	for _, name := range []string{
		"RIGEL_BACKUP_KEY_FILE",
		"RIGEL_BACKUP_KNOWN_HOSTS",
		"RIGEL_BACKUP_MIRROR",
		"RIGEL_BACKUP_UPSTREAM",
	} {
		t.Run(name, func(t *testing.T) {
			getenv := func(key string) string {
				if key == name {
					return ""
				}
				return "set"
			}
			_, err := readSyncConfig(getenv)
			if err == nil || !strings.Contains(err.Error(), name+" is not set") {
				t.Fatalf("readSyncConfig() error = %v, want missing %s", err, name)
			}
		})
	}
}

func TestSync(t *testing.T) {
	t.Run("fast-forwards local mirror from GitHub main", func(t *testing.T) {
		fixture := newBackupFixture(t)
		fixture.commit(t, "one", "")
		sha := fixture.commit(t, "two", "")
		result := sync(t.Context(), fixture.syncConfig(fixture.upstream))
		if want := successPrefix + " main=" + sha; result.code != 0 || result.line != want {
			t.Fatalf("sync() = %+v, want success for %s", result, sha)
		}
		if got := fixture.mirrorMain(t); got != sha {
			t.Errorf("mirror main = %q, want %q", got, sha)
		}
	})

	t.Run("succeeds when local mirror is already up to date", func(t *testing.T) {
		fixture := newBackupFixture(t)
		sha := fixture.commit(t, "one", "")
		config := fixture.syncConfig(fixture.upstream)
		if result := sync(t.Context(), config); result.code != 0 {
			t.Fatalf("initial sync() = %+v, want success", result)
		}
		result := sync(t.Context(), config)
		if want := successPrefix + " main=" + sha; result.code != 0 || result.line != want {
			t.Fatalf("sync() = %+v, want success for %s", result, sha)
		}
	})

	t.Run("refuses divergence without changing local main", func(t *testing.T) {
		fixture := newBackupFixture(t)
		base := fixture.commit(t, "base", "")
		config := fixture.syncConfig(fixture.upstream)
		if result := sync(t.Context(), config); result.code != 0 {
			t.Fatalf("initial sync() = %+v, want success", result)
		}
		runGit(t, fixture.work, "reset", "-q", "--hard", base)
		foreign := fixture.commit(t, "mirror only", fixture.mirror)
		runGit(t, fixture.work, "reset", "-q", "--hard", base)
		fixture.commit(t, "GitHub only", "")
		result := sync(t.Context(), config)
		if result.code != 1 || !strings.Contains(result.line, "refusing non-fast-forward update") {
			t.Fatalf("sync() = %+v, want clear non-fast-forward refusal", result)
		}
		if got := fixture.mirrorMain(t); got != foreign {
			t.Errorf("mirror main = %q, want preserved commit %q", got, foreign)
		}
	})

	t.Run("reports fetch failure", func(t *testing.T) {
		fixture := newBackupFixture(t)
		result := sync(t.Context(), fixture.syncConfig(filepath.Join(fixture.root, "missing.git")))
		if result.code != 1 || !strings.HasPrefix(result.line, "rigel-backup-sync: fetch failed") {
			t.Fatalf("sync() = %+v, want fetch failure", result)
		}
	})
}

func TestSyncCommand(t *testing.T) {
	for _, test := range []struct {
		name     string
		args     []string
		wantCode int
		wantText string
	}{
		{name: "help", args: []string{"--help"}, wantText: "Exit codes:"},
		{name: "missing configuration", wantCode: 2, wantText: "RIGEL_BACKUP_KEY_FILE is not set"},
		{name: "rejects arguments", args: []string{"extra"}, wantCode: 1, wantText: "extra"},
		{name: "rejects completion command", args: []string{"completion", "bash"}, wantCode: 1, wantText: "completion"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout strings.Builder
			command := newRootCommand(t.Context(), func(string) string { return "" }, &stdout, &stdout)
			command.SetArgs(test.args)
			err := command.ExecuteContext(t.Context())
			code := 0
			output := stdout.String()
			if err != nil {
				output = err.Error()
				code = 1
				if exitErr, ok := errors.AsType[exitCodeError](err); ok {
					code = exitErr.code
				}
			}
			if code != test.wantCode {
				t.Errorf("exit code = %d, want %d", code, test.wantCode)
			}
			if !strings.Contains(output, test.wantText) {
				t.Errorf("command output = %q, want %q", output, test.wantText)
			}
		})
	}
}
