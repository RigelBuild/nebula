package main

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/RigelBuild/nebula/tools/go-lib/relaunch"
)

type UnitFact struct {
	Unit         string
	ActiveSince  string
	InvocationID string
	NRestarts    int
	ActiveState  string
	Result       string
}

type Trigger struct {
	BootID       string
	UptimeS      float64
	Units        []UnitFact
	ZellijServer string
	// ManagerUnit is the configured cotal manager unit name. legacyPrefix reads its InvocationID.
	ManagerUnit string
}

func parsePropertyBlocks(text string) ([]string, map[string]map[string]string) {
	blocks := make(map[string]map[string]string)
	order := make([]string, 0, 2)
	current := make(map[string]string)
	save := func() {
		if id := current["Id"]; id != "" {
			if _, exists := blocks[id]; !exists {
				order = append(order, id)
			}
			blocks[id] = current
		}
		current = make(map[string]string)
	}
	for line := range strings.SplitSeq(text, "\n") {
		if line == "" {
			save()
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if ok && key != "" {
			current[key] = value
		}
	}
	save()
	return order, blocks
}

func parseUnitFacts(text string) []UnitFact {
	order, blocks := parsePropertyBlocks(text)
	facts := make([]UnitFact, 0, len(order))
	for _, id := range order {
		properties := blocks[id]
		unit := properties["Id"]
		if unit == "" {
			continue
		}
		restarts, err := strconv.Atoi(properties["NRestarts"])
		if err != nil {
			restarts = 0
		}
		facts = append(facts, UnitFact{
			Unit:         unit,
			ActiveSince:  properties["ActiveEnterTimestamp"],
			InvocationID: properties["InvocationID"],
			NRestarts:    restarts,
			ActiveState:  properties["ActiveState"],
			Result:       properties["Result"],
		})
	}
	return facts
}

func fingerprint(trigger Trigger) string {
	return legacyPrefix(trigger) + ":devbox=" + trigger.ZellijServer
}

func legacyPrefix(trigger Trigger) string {
	manager := ""
	for _, unit := range trigger.Units {
		if unit.Unit == trigger.ManagerUnit {
			manager = unit.InvocationID
			break
		}
	}
	return trigger.BootID + ":" + manager
}

func startTime(stat string) string {
	end := strings.LastIndex(stat, ")")
	if end < 0 || len(stat) < end+2 {
		return ""
	}
	fields := strings.Fields(stat[end+2:])
	if len(fields) <= 19 {
		return ""
	}
	return fields[19]
}

func exited(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH)
}

// Mirrors tools/zellij-rss-watchdog/internal/proc/identity.go zellijServerIdentity; keep the two in step.
// zellijServerIdentity returns "<pid>:<starttime>" for the zellij viewer server, the MainPID of
// zellijUnit. It returns "none" when the unit has no main process, and unavailable when
// systemctl or /proc cannot be read.
func zellijServerIdentity(ctx context.Context, procRoot, runtimeDir, zellijUnit string, fileSystem relaunch.FileSystem, runner processRunner) string {
	pid, err := zellijMainPID(ctx, runner, zellijUnit)
	switch {
	case err != nil:
		return identityUnavailable
	case pid == 0:
		return identityNone
	}
	return zellijPIDIdentity(procRoot, strings.TrimRight(runtimeDir, "/"), strconv.FormatUint(pid, 10), fileSystem)
}

// zellijMainPID reads the MainPID of zellijUnit. An error means the read failed or the
// value did not parse; pid 0 means the unit has no main process.
func zellijMainPID(ctx context.Context, runner processRunner, zellijUnit string) (uint64, error) {
	result, err := runner(ctx, []string{systemctlCommand, userFlag, "show", "-p", "MainPID", "--value", zellijUnit}, "")
	if err != nil {
		return 0, err
	}
	if result.ExitCode != 0 {
		return 0, errors.New("systemctl show MainPID failed")
	}
	text := strings.TrimSpace(result.Stdout)
	return strconv.ParseUint(text, 10, 64)
}

// zellijPIDIdentity returns "<pid>:<starttime>" when pid is a zellij server on the runtime dir's
// socket and its start time is stable across a re-check. A vanished pid is none; any other read
// error is unavailable.
func zellijPIDIdentity(procRoot, runtimeDir, pid string, fileSystem relaunch.FileSystem) string {
	base := filepath.Join(procRoot, pid)
	matches, err := isZellijServer(base, runtimeDir, fileSystem)
	if err != nil {
		return procReadIdentity(err)
	}
	if !matches {
		return identityNone
	}
	before, err := startTimeOf(base, fileSystem)
	if err != nil {
		return procReadIdentity(err)
	}
	matches, err = isZellijServer(base, runtimeDir, fileSystem)
	if err != nil {
		return procReadIdentity(err)
	}
	if !matches {
		return identityNone
	}
	after, err := startTimeOf(base, fileSystem)
	if err != nil {
		return procReadIdentity(err)
	}
	if before == "" || before != after {
		return identityUnavailable
	}
	return pid + ":" + before
}

func procReadIdentity(err error) string {
	if exited(err) {
		return identityNone
	}
	return identityUnavailable
}

func startTimeOf(base string, fileSystem relaunch.FileSystem) (string, error) {
	stat, err := fileSystem.ReadFile(filepath.Join(base, "stat"))
	if err != nil {
		return "", err
	}
	return startTime(string(stat)), nil
}

func isZellijServer(base, runtimeDir string, fileSystem relaunch.FileSystem) (bool, error) {
	cmdline, err := fileSystem.ReadFile(filepath.Join(base, "cmdline"))
	if err != nil {
		return false, err
	}
	args := strings.Split(string(cmdline), "\x00")
	if filepath.Base(args[0]) != zellijCommand {
		return false, nil
	}
	for index, arg := range args {
		if arg == "--server" && index+1 < len(args) && isZellijSocket(args[index+1], runtimeDir) {
			return true, nil
		}
	}
	return false, nil
}

// isZellijSocket reports whether socket is <runtimeDir>/zellij/contract_version_<N>/<name>, where N is
// an unsigned integer and <name> is one non-empty path component (hashed or devbox).
func isZellijSocket(socket, runtimeDir string) bool {
	rest, ok := strings.CutPrefix(socket, runtimeDir+"/zellij/contract_version_")
	if !ok {
		return false
	}
	version, name, ok := strings.Cut(rest, "/")
	if !ok || name == "" || strings.Contains(name, "/") {
		return false
	}
	_, err := strconv.ParseUint(version, 10, 64)
	return err == nil
}

func parseUptime(text string) float64 {
	field, _, _ := strings.Cut(text, " ")
	value, err := strconv.ParseFloat(field, 64)
	if err != nil {
		return 0
	}
	return value
}

func unitFact(trigger Trigger, name string) *UnitFact {
	for i := range trigger.Units {
		if trigger.Units[i].Unit == name {
			return &trigger.Units[i]
		}
	}
	return nil
}
