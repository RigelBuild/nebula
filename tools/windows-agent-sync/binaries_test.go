package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestInstallBuilt(t *testing.T) {
	const revision = "fixture-revision"
	tests := []struct {
		name   string
		failAt int
	}{
		{name: "jj-vine failure stops later installers", failAt: 1},
		{name: "jj-hooks failure skips Linear install", failAt: 2},
		{name: "Linear failure is returned", failAt: 3},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("AWS_LC_SYS_PREBUILT_NASM", "existing")
			runner := &recordingBuiltRunner{failAt: test.failAt}
			err := installBuilt(context.Background(), runner.run, revision, fixtureBuiltPins)
			if err == nil {
				t.Fatal("installBuilt() succeeded after installer failure")
			}
			if runner.calls != test.failAt {
				t.Fatalf("installer calls = %d, want %d", runner.calls, test.failAt)
			}
			if runner.envDuringFirstCall != "1" {
				t.Fatalf("AWS_LC_SYS_PREBUILT_NASM during first install = %q, want 1", runner.envDuringFirstCall)
			}
			if got := os.Getenv("AWS_LC_SYS_PREBUILT_NASM"); got != "existing" {
				t.Fatalf("AWS_LC_SYS_PREBUILT_NASM after install = %q, want existing", got)
			}
		})
	}
	t.Run("restores initially absent AWS_LC_SYS_PREBUILT_NASM", func(t *testing.T) {
		const name = "AWS_LC_SYS_PREBUILT_NASM"
		if _, exists := os.LookupEnv(name); exists {
			t.Skip("test process starts with AWS_LC_SYS_PREBUILT_NASM set")
		}
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
		runner := &recordingBuiltRunner{}
		if err := installBuilt(context.Background(), runner.run, revision, fixtureBuiltPins); err != nil {
			t.Fatal(err)
		}
		if runner.envDuringFirstCall != "1" {
			t.Fatalf("AWS_LC_SYS_PREBUILT_NASM during first install = %q, want 1", runner.envDuringFirstCall)
		}
		if got, exists := os.LookupEnv(name); exists {
			t.Fatalf("%s after install = %q, exists=%v; want unset", name, got, exists)
		}
	})
}

// fixtureBuiltPins is the version pair the installer must pass to cargo and bun.
var fixtureBuiltPins = builtPins{JJHooks: "9.9.9", LinearCLI: "8.8.8"}

func TestInstallBuiltPinsVersions(t *testing.T) {
	var calls [][]string
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		return nil, nil
	}
	if err := installBuilt(context.Background(), run, "fixture-revision", fixtureBuiltPins); err != nil {
		t.Fatal(err)
	}
	wantJJHooks := []string{"cargo", "install", "--locked", "jj-hooks", "--version", "9.9.9"}
	wantLinear := []string{"bun", "install", "-g", "@schpet/linear-cli@8.8.8"}
	if len(calls) < 3 || !reflect.DeepEqual(calls[1], wantJJHooks) || !reflect.DeepEqual(calls[2], wantLinear) {
		t.Fatalf("installer calls = %v; want jj-hooks %v then linear %v", calls, wantJJHooks, wantLinear)
	}
}

type recordingBuiltRunner struct {
	calls              int
	failAt             int
	envDuringFirstCall string
}

func (r *recordingBuiltRunner) run(_ context.Context, _ string, _ ...string) ([]byte, error) {
	r.calls++
	if r.calls == 1 {
		r.envDuringFirstCall = os.Getenv("AWS_LC_SYS_PREBUILT_NASM")
	}
	if r.calls == r.failAt && r.failAt > 0 {
		return nil, errors.New("command failed")
	}
	return nil, nil
}

func TestReadNebulaRevision(t *testing.T) {
	t.Run("reads locked revision from temporary flake fixture", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "flake.lock")
		const fixture = `{"nodes":{"nebula":{"locked":{"rev":"fixture-revision"}}}}`
		if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
			t.Fatal(err)
		}
		revision, err := readNebulaRevision(path)
		if err != nil {
			t.Fatal(err)
		}
		if revision != "fixture-revision" {
			t.Fatalf("nebula revision = %q, want fixture revision", revision)
		}
	})
	t.Run("reports missing node", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "flake.lock")
		if err := os.WriteFile(path, []byte(`{"nodes":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readNebulaRevision(path); err == nil || !strings.Contains(err.Error(), "no nebula locked revision") {
			t.Fatalf("readNebulaRevision() error = %v, want missing nebula error", err)
		}
	})
}

func TestEnsurePath(t *testing.T) {
	for _, test := range []struct {
		name       string
		initial    string
		want       string
		wantWrites int
	}{
		{name: "adds absent paths once", initial: `C:\Windows`, want: `C:\Windows;C:\Users\me\.local\bin;C:\Users\me\.bun\bin`, wantWrites: 1},
		{name: "existing paths do not duplicate", initial: `C:\Windows;C:\Users\me\.local\bin;C:\Users\me\.bun\bin`, want: `C:\Windows;C:\Users\me\.local\bin;C:\Users\me\.bun\bin`},
		{name: "path comparison ignores case and slash", initial: `C:\Users\me\.LOCAL\bin;C:\Users\me\.bun\BIN`, want: `C:\Users\me\.LOCAL\bin;C:\Users\me\.bun\BIN`},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newFakeEnvStore()
			store.values["Path"] = test.initial
			if err := ensurePath(store, `C:\Users\me\.local\bin`, `C:\Users\me\.bun\bin`); err != nil {
				t.Fatal(err)
			}
			if store.values["Path"] != test.want {
				t.Fatalf("Path = %q, want %q", store.values["Path"], test.want)
			}
			if len(store.sets) != test.wantWrites {
				t.Fatalf("writes = %v, want %d", store.sets, test.wantWrites)
			}
		})
	}
	t.Run("empty PATH adds no leading separator", func(t *testing.T) {
		store := newFakeEnvStore()
		if err := ensurePath(store, `C:\Users\me\.local\bin`); err != nil {
			t.Fatal(err)
		}
		if got := store.values["Path"]; got != `C:\Users\me\.local\bin` {
			t.Fatalf("Path = %q", got)
		}
	})
}
