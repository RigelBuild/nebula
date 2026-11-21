package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"strings"
)

func Identify(pid int) (Identity, error) {
	return identifyLinux(pid, os.ReadFile)
}

func identifyLinux(pid int, readFile func(string) ([]byte, error)) (Identity, error) {
	if pid <= 0 {
		return Identity{}, errors.New("invalid pid")
	}
	data, err := readFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return Identity{}, fmt.Errorf("read process stat: %w", identityReadError(pid, err))
	}
	start, err := parseLinuxStat(string(data))
	if err != nil {
		return Identity{}, err
	}
	bootID, err := readFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return Identity{}, fmt.Errorf("read boot id: %w", err)
	}
	bootToken := strings.TrimSpace(string(bootID))
	if bootToken == "" {
		return Identity{}, errors.New("empty boot id")
	}
	return Identity{PID: pid, StartToken: bootToken + ":" + start}, nil
}

func Children(pid int) ([]int, error) {
	if pid <= 0 {
		return nil, errors.New("invalid pid")
	}
	return childrenWith(pid, listProcessThreads, readThreadChildren, statThreadTask)
}

func childrenWith(
	pid int,
	listThreads func(int) ([]int, error),
	readChildren func(int, int) ([]byte, error),
	statTask func(int, int) error,
) ([]int, error) {
	threads, err := listThreads(pid)
	if err != nil {
		return nil, fmt.Errorf("list process threads: %w", err)
	}

	unique := make(map[int]struct{})
	for _, tid := range threads {
		data, err := readChildren(pid, tid)
		if errors.Is(err, fs.ErrNotExist) {
			if taskErr := statTask(pid, tid); errors.Is(taskErr, fs.ErrNotExist) {
				continue
			} else if taskErr != nil {
				return nil, fmt.Errorf("check thread %d task: %w", tid, taskErr)
			}
			return nil, fmt.Errorf("read thread %d children while task exists: %w", tid, err)
		}
		if err != nil {
			return nil, fmt.Errorf("read thread %d children: %w", tid, err)
		}
		for field := range strings.FieldsSeq(string(data)) {
			child, err := strconv.Atoi(field)
			if err != nil || child <= 0 {
				return nil, fmt.Errorf("invalid child pid %q", field)
			}
			unique[child] = struct{}{}
		}
	}

	children := make([]int, 0, len(unique))
	for child := range unique {
		children = append(children, child)
	}
	slices.Sort(children)
	return children, nil
}

func listProcessThreads(pid int) ([]int, error) {
	entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
	if err != nil {
		return nil, fmt.Errorf("read process tasks: %w", err)
	}
	threads := make([]int, 0, len(entries))
	for _, entry := range entries {
		tid, err := strconv.Atoi(entry.Name())
		if err != nil || tid <= 0 {
			return nil, fmt.Errorf("invalid thread id %q", entry.Name())
		}
		threads = append(threads, tid)
	}
	return threads, nil
}

func readThreadChildren(pid, tid int) ([]byte, error) {
	return os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/children", pid, tid))
}

func statThreadTask(pid, tid int) error {
	_, err := os.Stat(fmt.Sprintf("/proc/%d/task/%d", pid, tid))
	return err
}
