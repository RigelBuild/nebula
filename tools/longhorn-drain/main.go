// Command longhorn-drain tears down k3s pods on a Longhorn node at shutdown,
// after k3s has stopped. It unmounts Longhorn volumes while the node's
// instance-manager (the iSCSI target) is still alive, then kills it.
package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	shimBinary       = "containerd-shim-runc-v2"
	taskRoot         = "/run/k3s/containerd/io.containerd.runtime.v2.task"
	cgroupRoot       = "/sys/fs/cgroup"
	kubeletDir       = "/var/lib/kubelet"
	longhornDevDir   = "/dev/longhorn/"
	longhornNS       = "longhorn-system"
	annoSandboxNS    = "io.kubernetes.cri.sandbox-namespace"
	annoPodNamespace = "io.kubernetes.pod.namespace"
	// emptyTimeout bounds each wait for killed cgroups to empty. The two waits,
	// the unmounts and k3s-killall.sh all share the unit's TimeoutStopSec=120s.
	emptyTimeout = 30 * time.Second
	pollInterval = 100 * time.Millisecond
)

// system is the host seam: /proc and cgroupfs reads, cgroup writes, unmount,
// sync and the clock. Tests replace it with a fake process tree.
type system interface {
	readFile(name string) ([]byte, error)
	writeFile(name string, data []byte) error
	glob(pattern string) ([]string, error)
	unmount(target string) error
	sync()
	now() time.Time
	sleep(d time.Duration)
}

type osSystem struct{}

func (osSystem) readFile(name string) ([]byte, error) { return os.ReadFile(name) }
func (osSystem) writeFile(name string, data []byte) error {
	return os.WriteFile(name, data, 0)
}
func (osSystem) glob(pattern string) ([]string, error) { return filepath.Glob(pattern) }
func (osSystem) unmount(target string) error           { return syscall.Unmount(target, 0) }
func (osSystem) sync()                                 { syscall.Sync() }
func (osSystem) now() time.Time                        { return time.Now() }

// sleep blocks between waitEmpty polls; tests advance a fake clock instead.
func (osSystem) sleep(d time.Duration) { time.Sleep(d) }

// shim is one containerd runc shim and the pod cgroups of its containers.
type shim struct {
	pid       int
	namespace string
	cgroups   []string
}

type mount struct {
	source string
	target string
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	run(logger, osSystem{})
	// Never block shutdown: every failure is logged, the exit is always 0.
	os.Exit(0)
}

// run drains the node: app pods, sync, Longhorn unmounts, then longhorn-system
// pods. It kills containers by cgroup and leaves the shims, so k3s-killall.sh
// still finds them and cleans up after it.
func run(logger *slog.Logger, sys system) {
	shims := listShims(logger, sys)
	apps, late := partitionShims(shims)
	logger.Info("step 1: killing app pods", "app", len(apps), "late", len(late))
	killFailed := killPods(logger, sys, apps)

	logger.Info("step 2: sync")
	sys.sync()

	unmountFailed := 0
	data, err := sys.readFile("/proc/self/mountinfo")
	if err != nil {
		logger.Error("read mountinfo", "err", err)
	} else {
		targets := longhornKubeletMounts(parseMountinfo(string(data)))
		logger.Info("step 3: unmounting Longhorn kubelet mounts", "count", len(targets))
		for _, target := range targets {
			if err := sys.unmount(target); err != nil {
				unmountFailed++
				logger.Error("unmount", "target", target, "err", err)
				continue
			}
			logger.Info("unmounted", "target", target)
		}
	}

	logger.Info("step 4: killing longhorn-system and unknown pods", "count", len(late))
	killFailed += killPods(logger, sys, late)
	logger.Info("done", "unmount_failed", unmountFailed, "kill_failed", killFailed)
}

// proc is the part of /proc/<pid> the drain needs.
type proc struct {
	pid     int
	ppid    int
	cmdline []byte
	cgroup  string
}

// readProcs reads every process's parent, cmdline and cgroup v2 path.
func readProcs(logger *slog.Logger, sys system) []proc {
	dirs, err := sys.glob("/proc/[0-9]*")
	if err != nil {
		logger.Error("glob /proc", "err", err)
		return nil
	}
	var procs []proc
	for _, dir := range dirs {
		pid, err := strconv.Atoi(path.Base(dir))
		if err != nil {
			continue
		}
		stat, err := sys.readFile(dir + "/stat")
		if err != nil {
			continue
		}
		ppid, ok := parsePPid(stat)
		if !ok {
			continue
		}
		cmdline, _ := sys.readFile(dir + "/cmdline")
		cgroup, _ := sys.readFile(dir + "/cgroup")
		procs = append(procs, proc{pid: pid, ppid: ppid, cmdline: cmdline, cgroup: parseCgroup(cgroup)})
	}
	return procs
}

// listShims finds running runc shims, their pod namespaces and the cgroups of
// their descendant processes.
func listShims(logger *slog.Logger, sys system) []shim {
	procs := readProcs(logger, sys)
	children := map[int][]proc{}
	for _, p := range procs {
		children[p.ppid] = append(children[p.ppid], p)
	}
	var shims []shim
	for _, p := range procs {
		ns, id, ok := shimArgs(p.cmdline)
		if !ok {
			continue
		}
		namespace := ""
		config, err := sys.readFile(path.Join(taskRoot, ns, id, "config.json"))
		if err != nil {
			logger.Error("read shim config", "pid", p.pid, "id", id, "err", err)
		} else if namespace, err = podNamespace(config); err != nil {
			logger.Error("parse shim config", "pid", p.pid, "id", id, "err", err)
		}
		shims = append(shims, shim{
			pid:       p.pid,
			namespace: namespace,
			cgroups:   descendantCgroups(p.pid, children),
		})
	}
	return shims
}

// descendantCgroups returns the sorted, distinct pod cgroups of every
// descendant of pid. Containers run in their own process groups and sessions,
// so only the parent tree reaches them.
func descendantCgroups(pid int, children map[int][]proc) []string {
	seen := map[string]bool{}
	queue := []int{pid}
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		for _, child := range children[next] {
			queue = append(queue, child.pid)
			if cg := podCgroup(child.cgroup); cg != "" {
				seen[cg] = true
			}
		}
	}
	cgroups := make([]string, 0, len(seen))
	for cg := range seen {
		cgroups = append(cgroups, cg)
	}
	sort.Strings(cgroups)
	return cgroups
}

// podCgroup maps a container's cgroup to its pod cgroup under kubepods.
// It returns "" for a process outside kubepods, so killing never reaches
// k3s or system cgroups.
func podCgroup(cgroup string) string {
	if !strings.Contains(cgroup, "/kubepods") {
		return ""
	}
	if base := path.Base(cgroup); strings.HasPrefix(base, "cri-containerd-") {
		return path.Dir(cgroup)
	}
	return cgroup
}

// parsePPid reads the parent pid from /proc/<pid>/stat. The command name may
// contain spaces and parentheses, so parse after the last ')'.
func parsePPid(stat []byte) (int, bool) {
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return 0, false
	}
	fields := strings.Fields(string(stat[i+1:]))
	if len(fields) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(fields[1])
	return ppid, err == nil
}

// parseCgroup returns the cgroup v2 path from /proc/<pid>/cgroup.
func parseCgroup(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "0::"); ok {
			return rest
		}
	}
	return ""
}

// shimArgs returns the -namespace and -id arguments of a runc shim cmdline.
func shimArgs(cmdline []byte) (namespace, id string, ok bool) {
	args := strings.Split(string(bytes.TrimRight(cmdline, "\x00")), "\x00")
	if len(args) == 0 || path.Base(args[0]) != shimBinary {
		return "", "", false
	}
	namespace = "k8s.io"
	for i := 1; i+1 < len(args); i++ {
		switch args[i] {
		case "-id":
			id = args[i+1]
		case "-namespace":
			namespace = args[i+1]
		}
	}
	return namespace, id, id != "" && namespace != ""
}

// podNamespace reads the pod namespace from an OCI bundle config.json.
func podNamespace(config []byte) (string, error) {
	var spec struct {
		Annotations map[string]string `json:"annotations"`
	}
	if err := json.Unmarshal(config, &spec); err != nil {
		return "", err
	}
	if ns := spec.Annotations[annoSandboxNS]; ns != "" {
		return ns, nil
	}
	return spec.Annotations[annoPodNamespace], nil
}

// partitionShims splits shims into app shims, killed first, and late shims:
// longhorn-system and unknown-namespace shims. Unknown fails closed, since it
// may be the iSCSI target. Its Longhorn mounts may return EBUSY when unmounted.
func partitionShims(shims []shim) (apps, late []shim) {
	for _, s := range shims {
		if s.namespace == "" || s.namespace == longhornNS {
			late = append(late, s)
		} else {
			apps = append(apps, s)
		}
	}
	return apps, late
}

// killPods kills every pod cgroup of the shims through cgroup.kill, then waits
// for them to empty, bounded by emptyTimeout. It returns how many kills
// failed. The shims are left for k3s-killall.sh.
func killPods(logger *slog.Logger, sys system, shims []shim) int {
	var killed []string
	var failed int
	for _, s := range shims {
		for _, cg := range s.cgroups {
			dir := cgroupRoot + cg
			if err := sys.writeFile(dir+"/cgroup.kill", []byte("1")); err != nil {
				failed++
				logger.Error("kill cgroup", "cgroup", cg, "shim", s.pid, "err", err)
				continue
			}
			logger.Info("killed cgroup", "cgroup", cg, "shim", s.pid, "namespace", s.namespace)
			killed = append(killed, dir)
		}
	}
	waitEmpty(logger, sys, killed)
	return failed
}

// waitEmpty polls cgroup.events until each cgroup reports populated 0 or is
// gone, or the deadline passes.
func waitEmpty(logger *slog.Logger, sys system, dirs []string) {
	deadline := sys.now().Add(emptyTimeout)
	pending := slices.Clone(dirs)
	for len(pending) > 0 {
		pending = slices.DeleteFunc(pending, func(dir string) bool {
			events, err := sys.readFile(dir + "/cgroup.events")
			return err != nil || bytes.Contains(events, []byte("populated 0"))
		})
		if len(pending) == 0 {
			return
		}
		if !sys.now().Before(deadline) {
			logger.Error("cgroups still populated after deadline", "cgroups", pending)
			return
		}
		sys.sleep(pollInterval)
	}
}

// parseMountinfo parses /proc/self/mountinfo text into mounts.
func parseMountinfo(text string) []mount {
	var mounts []mount
	for _, line := range strings.Split(text, "\n") {
		pre, post, ok := strings.Cut(line, " - ")
		if !ok {
			continue
		}
		preFields := strings.Fields(pre)
		postFields := strings.Fields(post)
		if len(preFields) < 5 || len(postFields) < 2 {
			continue
		}
		mounts = append(mounts, mount{
			source: unescapeMountinfo(postFields[1]),
			target: unescapeMountinfo(preFields[4]),
		})
	}
	return mounts
}

// unescapeMountinfo decodes the kernel's octal escapes (\040 etc.).
func unescapeMountinfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// longhornKubeletMounts selects Longhorn-backed mounts under the kubelet dir in
// unmount order: pod volume mounts, then CSI globalmount/staging, deepest first.
func longhornKubeletMounts(mounts []mount) []string {
	var targets []string
	for _, m := range mounts {
		if !strings.HasPrefix(m.source, longhornDevDir) {
			continue
		}
		if m.target != kubeletDir && !strings.HasPrefix(m.target, kubeletDir+"/") {
			continue
		}
		targets = append(targets, m.target)
	}
	podsDir := kubeletDir + "/pods/"
	sort.SliceStable(targets, func(i, j int) bool {
		pi, pj := strings.HasPrefix(targets[i], podsDir), strings.HasPrefix(targets[j], podsDir)
		if pi != pj {
			return pi
		}
		di, dj := strings.Count(targets[i], "/"), strings.Count(targets[j], "/")
		if di != dj {
			return di > dj
		}
		return targets[i] < targets[j]
	})
	return targets
}
