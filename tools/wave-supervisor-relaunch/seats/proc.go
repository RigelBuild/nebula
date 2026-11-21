package seats

import (
	"path/filepath"
	"strconv"
	"strings"

	"github.com/RigelBuild/nebula/tools/go-lib/relaunch"
)

// Process is one omp process in the wave slice, read once per pass.
type Process struct {
	Name   string
	Cgroup string
}

// scopeMemory returns each seat's anon memory, read from its omp scope's memory.stat. It is not
// memory.current, which includes page cache. A scope whose memory.stat is unreadable is skipped.
func scopeMemory(procs []Process, fileSystem relaunch.FileSystem, cgroupRoot string) map[string]int64 {
	memory := map[string]int64{}
	for _, proc := range procs {
		scope := cgroupV2Path(proc.Cgroup)
		if scope == "" {
			continue
		}
		stat, err := fileSystem.ReadFile(filepath.Join(cgroupRoot, scope, "memory.stat"))
		if err != nil {
			continue
		}
		if anon, ok := statValue(string(stat), "anon "); ok {
			memory[proc.Name] = max(memory[proc.Name], anon)
		}
	}
	return memory
}

// cgroupV2Path returns the unified-hierarchy path from /proc/<pid>/cgroup, or "" when absent.
func cgroupV2Path(cgroup string) string {
	for line := range strings.SplitSeq(cgroup, "\n") {
		if rest, ok := strings.CutPrefix(line, "0::"); ok {
			return rest
		}
	}
	return ""
}

// statValue reads one "<key> <int>" line, such as anon in memory.stat.
func statValue(body, prefix string) (int64, bool) {
	for line := range strings.SplitSeq(body, "\n") {
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			value, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
			return value, err == nil
		}
	}
	return 0, false
}
