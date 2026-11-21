//go:build linux

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"slices"
	"testing"
)

func TestIdentifyLinuxIncludesBootID(t *testing.T) {
	files := map[string][]byte{
		"/proc/42/stat":                   []byte("42 (test process) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 987 20"),
		"/proc/sys/kernel/random/boot_id": []byte("boot-identity\n"),
	}
	readFile := func(path string) ([]byte, error) {
		data, ok := files[path]
		if !ok {
			return nil, fmt.Errorf("unexpected path %q", path)
		}
		return data, nil
	}
	id, err := identifyLinux(42, readFile)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := id.StartToken, "boot-identity:987"; got != want {
		t.Fatalf("start token = %q, want %q", got, want)
	}
	files["/proc/sys/kernel/random/boot_id"] = []byte("next-boot\n")
	otherBoot, err := identifyLinux(42, readFile)
	if err != nil {
		t.Fatal(err)
	}
	if otherBoot.StartToken == id.StartToken {
		t.Fatalf("start token matched across boots: %q", id.StartToken)
	}
}

func TestChildrenUnionsThreadChildren(t *testing.T) {
	got, err := childrenWith(
		42,
		func(pid int) ([]int, error) {
			if pid != 42 {
				return nil, fmt.Errorf("pid = %d, want 42", pid)
			}
			return []int{42, 43}, nil
		},
		func(pid, tid int) ([]byte, error) {
			if pid != 42 {
				return nil, fmt.Errorf("pid = %d, want 42", pid)
			}
			switch tid {
			case 42:
				return []byte("300 100"), nil
			case 43:
				return []byte("200 100"), nil
			default:
				return nil, fmt.Errorf("unexpected tid %d", tid)
			}
		},
		func(int, int) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []int{100, 200, 300}
	if len(got) != len(want) {
		t.Fatalf("children = %v, want %v", got, want)
	}
	for i, child := range got {
		if child != want[i] {
			t.Fatalf("children = %v, want %v", got, want)
		}
	}
}

func TestChildrenSkipsVanishedThread(t *testing.T) {
	got, err := childrenWith(
		42,
		func(int) ([]int, error) { return []int{42, 43}, nil },
		func(_, tid int) ([]byte, error) {
			if tid == 43 {
				return nil, fs.ErrNotExist
			}
			return []byte("300 100"), nil
		},
		func(_, tid int) error {
			if tid == 43 {
				return fs.ErrNotExist
			}
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []int{100, 300}
	if !slices.Equal(got, want) {
		t.Fatalf("children = %v, want %v", got, want)
	}
}

func TestChildrenErrorsWhenTaskExistsWithoutChildrenFile(t *testing.T) {
	_, err := childrenWith(
		42,
		func(int) ([]int, error) { return []int{42}, nil },
		func(int, int) ([]byte, error) { return nil, fs.ErrNotExist },
		func(int, int) error { return nil },
	)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("error = %v, want not-exist for missing children file", err)
	}
}

func TestChildrenIncludesChildFromLockedThread(t *testing.T) {
	type spawnResult struct {
		child *processTestChild
		err   error
	}
	result := make(chan spawnResult, 1)
	release := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		child, err := startProcessHelperCommand()
		result <- spawnResult{child: child, err: err}
		if err == nil {
			<-release
		}
	}()

	var child *processTestChild
	t.Cleanup(func() {
		close(release)
		<-finished
		if child != nil {
			if err := child.stop(); err != nil {
				t.Errorf("stop child helper: %v", err)
			}
		}
	})

	spawn := <-result
	if spawn.err != nil {
		t.Fatal(spawn.err)
	}
	child = spawn.child
	children, err := Children(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(children, child.cmd.Process.Pid) {
		t.Fatalf("children %v do not include locked-thread child pid %d", children, child.cmd.Process.Pid)
	}
}
