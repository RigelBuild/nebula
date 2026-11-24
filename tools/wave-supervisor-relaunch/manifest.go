package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/relaunch"
)

type WaveManifest struct {
	At          string               `json:"at"`
	Fingerprint string               `json:"fingerprint"`
	Agents      []relaunch.WaveAgent `json:"agents"`
}

type WaveCensus struct {
	At          string               `json:"at"`
	Fingerprint string               `json:"fingerprint"`
	Agents      []relaunch.WaveAgent `json:"agents"`
	Unmapped    int                  `json:"unmapped"`
}

const seatStopDeadlineMS = 60_000
const seatStopOuterDeadlineMS = seatStopDeadlineMS + 5_000

type stopWaveDeps struct {
	CheckSubagents    func(context.Context) error
	CheckDeadline     time.Duration
	StopSeats         func(context.Context) error
	StopSeatsDeadline time.Duration
	WithTimeout       func(context.Context, time.Duration) (context.Context, context.CancelFunc)
	StopSlice         func(context.Context) (bool, error)
	PresenceGone      func(context.Context) (bool, error)
	Now               func() time.Time
	Sleep             func(context.Context, time.Duration) error
	OnWarning         func(string, error)
	Deadline          time.Duration
	Poll              time.Duration
}

func readText(fileSystem relaunch.FileSystem, path string) ([]byte, bool) {
	if fileSystem == nil {
		fileSystem = relaunch.NodeFileSystem{}
	}
	data, err := fileSystem.ReadFile(path)
	return data, err == nil
}

// inWaveSlice reports whether pid's cgroup lies under the configured slice. A systemd slice's
// cgroup directory carries the unit name, so the match is "/<slice>/" or a path ending "/<slice>".
func inWaveSlice(fileSystem relaunch.FileSystem, procRoot, pid, slice string) bool {
	data, ok := readText(fileSystem, filepath.Join(procRoot, pid, "cgroup"))
	return ok && cgroupInWaveSlice(string(data), slice)
}

func cgroupInWaveSlice(cgroup, slice string) bool {
	segment := "/" + slice + "/"
	for line := range strings.SplitSeq(cgroup, "\n") {
		if strings.Contains(line, segment) || strings.HasSuffix(line, "/"+slice) {
			return true
		}
	}
	return false
}

func slicePIDs(procRoot string, fileSystem relaunch.FileSystem, slice string) ([]string, error) {
	if fileSystem == nil {
		fileSystem = relaunch.NodeFileSystem{}
	}
	entries, err := fileSystem.ReadDir(procRoot)
	if err != nil {
		return nil, fmt.Errorf("cannot enumerate proc: %w", err)
	}
	pids := make([]string, 0)
	for _, entry := range entries {
		if decimalDigits(entry.Name()) && inWaveSlice(fileSystem, procRoot, entry.Name(), slice) {
			pids = append(pids, entry.Name())
		}
	}
	return pids, nil
}

func isOmpProcess(cmdline string) bool {
	argv := strings.Split(cmdline, "\x00")
	if len(argv) == 0 {
		return false
	}
	return filepath.Base(argv[0]) == "omp" || argv[0] == "omp"
}

func listSliceOmpPIDs(procRoot string, fileSystem relaunch.FileSystem, slice string) ([]string, error) {
	pids, err := slicePIDs(procRoot, fileSystem, slice)
	if err != nil {
		return nil, err
	}
	result := make([]string, 0)
	for _, pid := range pids {
		if cmdline, ok := readText(fileSystem, filepath.Join(procRoot, pid, "cmdline")); ok && isOmpProcess(string(cmdline)) {
			result = append(result, pid)
		}
	}
	return result, nil
}

func processAgentName(environment string) string {
	for item := range strings.SplitSeq(environment, "\x00") {
		value, ok := strings.CutPrefix(item, "COTAL_AGENT_FILE=")
		if !ok {
			continue
		}
		if value == "" {
			return ""
		}
		name := path.Base(value)
		if name == "/" {
			return ""
		}
		name = strings.TrimSuffix(name, ".md")
		if name != "" {
			return name
		}
	}
	return ""
}

func argumentValue(args []string, flag string) string {
	for index, arg := range args {
		if arg == flag && index+1 < len(args) {
			value := args[index+1]
			if value != "" {
				return value
			}
			return ""
		}
	}
	return ""
}

func decimalDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func readWaveManifest(procRoot string, fileSystem relaunch.FileSystem, sessionDir, slice string) (WaveCensus, error) {
	pids, err := slicePIDs(procRoot, fileSystem, slice)
	if err != nil {
		return WaveCensus{}, err
	}
	agents := make([]relaunch.WaveAgent, 0)
	unmapped := 0
	for _, pid := range pids {
		base := filepath.Join(procRoot, pid)
		cmdlineBytes, ok := readText(fileSystem, filepath.Join(base, "cmdline"))
		if !ok {
			return WaveCensus{}, fmt.Errorf("cannot read command line for wave process %s", pid)
		}
		cmdline := string(cmdlineBytes)
		if !isOmpProcess(cmdline) {
			continue
		}
		environmentBytes, envOK := readText(fileSystem, filepath.Join(base, "environ"))
		name := ""
		if envOK {
			name = processAgentName(string(environmentBytes))
		}
		if name == "" {
			unmapped++
			continue
		}
		resolved := resolveProcessSession(base, cmdline, environmentBytes, envOK, sessionDir, fileSystem)
		if resolved == nil || resolved.ID == "" {
			unmapped++
			continue
		}
		agent := relaunch.WaveAgent{Name: name, SessionID: resolved.ID}
		if resolved.Stale {
			agent.Stale = true
		}
		agents = append(agents, agent)
	}
	return WaveCensus{At: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), Fingerprint: "", Agents: agents, Unmapped: unmapped}, nil
}

func resolveProcessSession(base, cmdline string, environmentBytes []byte, envOK bool, sessionDir string, fileSystem relaunch.FileSystem) *relaunch.SessionResult {
	args := strings.Split(cmdline, "\x00")
	sessionID := argumentValue(args, "--session-id")
	forkID := argumentValue(args, "--fork")
	fdDirectory := filepath.Join(base, "fd")
	openFiles := make([]string, 0)
	fdEntries, err := fileSystem.ReadDir(fdDirectory)
	if err == nil {
		for _, fd := range fdEntries {
			path, err := fileSystem.Readlink(filepath.Join(fdDirectory, fd.Name()))
			if err == nil && strings.HasPrefix(path, sessionDir+"/") && strings.HasSuffix(path, ".jsonl") {
				openFiles = append(openFiles, path)
			}
		}
	}
	cwd, cwdErr := fileSystem.Readlink(filepath.Join(base, "cwd"))
	home := ""
	if envOK {
		for item := range strings.SplitSeq(string(environmentBytes), "\x00") {
			if value, ok := strings.CutPrefix(item, "HOME="); ok {
				home = value
				break
			}
		}
	}
	topLevelFiles := []string{}
	if cwdErr == nil {
		topLevelFiles = relaunch.TopLevelSessionFiles(relaunch.SessionDirFor(cwd, home, sessionDir), fileSystem)
	}
	topLevelOpen := make([]string, 0)
	for _, path := range openFiles {
		if slices.Contains(topLevelFiles, path) {
			topLevelOpen = append(topLevelOpen, path)
		}
	}
	resolved := relaunch.ResolveForkSession(openFiles, topLevelOpen, topLevelFiles, sessionID, forkID, fileSystem, nil, nil)
	return resolved
}

func waveProcessesPresent(procRoot string, fileSystem relaunch.FileSystem, slice string) bool {
	pids, err := slicePIDs(procRoot, fileSystem, slice)
	if err != nil {
		return true
	}
	for _, pid := range pids {
		cmdline, ok := readText(fileSystem, filepath.Join(procRoot, pid, "cmdline"))
		if ok && isOmpProcess(string(cmdline)) {
			return true
		}
	}
	return false
}

func writeWaveManifest(path string, manifest WaveManifest, fileSystem relaunch.FileSystem) error {
	return writeAtomicJSON(path, manifest, fileSystem)
}

func writeAtomicJSON(path string, value any, fileSystem relaunch.FileSystem) error {
	if fileSystem == nil {
		fileSystem = relaunch.NodeFileSystem{}
	}
	if err := fileSystem.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating manifest directory: %w", err)
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding manifest: %w", err)
	}
	data = append(data, '\n')
	temporary := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := fileSystem.WriteFile(temporary, data, 0o666); err != nil {
		return fmt.Errorf("writing temporary manifest: %w", err)
	}
	if err := fileSystem.Rename(temporary, path); err != nil {
		removeErr := fileSystem.Remove(temporary)
		if removeErr != nil {
			return fmt.Errorf("renaming manifest: %w (removing temporary manifest: %w)", err, removeErr)
		}
		return fmt.Errorf("renaming manifest: %w", err)
	}
	return nil
}

// stopWave refuses to stop the wave when a subagent check fails before seat or slice shutdown.
func stopWave(ctx context.Context, deps stopWaveDeps) string {
	if err := checkWaveSubagents(ctx, deps); err != nil {
		deps.noteSubagentRefusal(err)
		return "skipped:subagents"
	}
	stopWaveSeats(ctx, deps)
	if err := checkWaveSubagents(ctx, deps); err != nil {
		deps.noteSubagentRefusal(err)
		return "skipped:subagents"
	}
	stopped, err := deps.StopSlice(ctx)
	if err != nil || !stopped {
		return "wave-stop"
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	sleep := deps.Sleep
	if sleep == nil {
		sleep = sleepContext
	}
	deadline := deps.Deadline
	if deadline == 0 {
		deadline = 30 * time.Second
	}
	poll := deps.Poll
	if poll == 0 {
		poll = 2 * time.Second
	}
	started := now()
	for {
		gone, err := deps.PresenceGone(ctx)
		if err == nil && gone {
			return "stopped"
		}
		if err != nil {
			deps.notePresenceFailure(err)
		}
		elapsed := now().Sub(started)
		if elapsed >= deadline {
			return "presence-stuck"
		}
		if err := sleep(ctx, min(poll, deadline-elapsed)); err != nil {
			return "presence-stuck"
		}
	}
}

func checkWaveSubagents(ctx context.Context, deps stopWaveDeps) error {
	if deps.CheckSubagents == nil {
		return errors.New("no subagent check wired")
	}
	deadline := deps.CheckDeadline
	if deadline == 0 {
		deadline = time.Duration(seatStopOuterDeadlineMS) * time.Millisecond
	}
	checkCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- deps.CheckSubagents(checkCtx) }()
	select {
	case err := <-finished:
		return err
	case <-checkCtx.Done():
		return fmt.Errorf("subagent check: %w", checkCtx.Err())
	}
}

func stopWaveSeats(ctx context.Context, deps stopWaveDeps) {
	if deps.StopSeats == nil {
		return
	}
	deadline := deps.StopSeatsDeadline
	if deadline == 0 {
		deadline = time.Duration(seatStopOuterDeadlineMS) * time.Millisecond
	}
	withTimeout := deps.WithTimeout
	if withTimeout == nil {
		withTimeout = context.WithTimeout
	}
	seatCtx, cancel := withTimeout(ctx, deadline)
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- deps.StopSeats(seatCtx) }()
	select {
	case err := <-finished:
		if err != nil {
			// Individual seat-stop failures are best effort; the slice stop is the backstop.
			deps.noteSeatStopFailure(err)
		}
	case <-seatCtx.Done():
		deps.noteSeatStopFailure(seatCtx.Err())
	}

}
func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (deps stopWaveDeps) noteSeatStopFailure(err error) {
	if deps.OnWarning != nil {
		deps.OnWarning("best-effort seat stop failed", err)
	} else {
		slog.Warn("best-effort seat stop failed", "error", err)
	}
}

func (deps stopWaveDeps) notePresenceFailure(err error) {
	if deps.OnWarning != nil {
		deps.OnWarning("wave presence check failed", err)
	} else {
		slog.Warn("wave presence check failed", "error", err)
	}
}

func (deps stopWaveDeps) noteSubagentRefusal(err error) {
	if deps.OnWarning != nil {
		deps.OnWarning("whole-wave stop refused: subagent check", err)
	} else {
		slog.Warn("whole-wave stop refused: subagent check", "error", err)
	}
}

func mergeManifestAgents(original, added []relaunch.WaveAgent) []relaunch.WaveAgent {
	byName := make(map[string]relaunch.WaveAgent, len(original)+len(added))
	order := make([]string, 0, len(original)+len(added))
	for _, agent := range original {
		if _, ok := byName[agent.Name]; !ok {
			order = append(order, agent.Name)
		}
		byName[agent.Name] = agent
	}
	for _, agent := range added {
		if _, ok := byName[agent.Name]; !ok {
			order = append(order, agent.Name)
		}
		byName[agent.Name] = agent
	}
	result := make([]relaunch.WaveAgent, 0, len(order))
	for _, name := range order {
		result = append(result, byName[name])
	}
	return result
}
