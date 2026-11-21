package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"path"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

const (
	podApp      = "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podapp.slice"
	podLonghorn = "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podlh.slice"
	podUnknown  = "/kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-podunk.slice"
	appMount    = "/var/lib/kubelet/pods/u/volumes/kubernetes.io~csi/pvc-1/mount"
	globalMount = "/var/lib/kubelet/plugins/kubernetes.io/csi/driver.longhorn.io/h/globalmount"
)

// fakeSystem is a process tree plus cgroupfs. A cgroup keeps reporting
// populated 1 for drainPolls reads after cgroup.kill, then empties.
// failUnmount and failKill name the unmount targets and cgroup dirs whose
// write fails.
type fakeSystem struct {
	files       map[string]string
	events      []string
	killed      map[string]int
	drainPolls  int
	stuck       bool
	clock       time.Time
	failUnmount map[string]bool
	failKill    map[string]bool
}

func newFakeSystem() *fakeSystem {
	f := &fakeSystem{
		files: map[string]string{}, killed: map[string]int{}, drainPolls: 1, clock: time.Unix(0, 0),
		failUnmount: map[string]bool{}, failKill: map[string]bool{},
	}
	shim := func(id string) string {
		return "/var/lib/rancher/k3s/data/x/bin/containerd-shim-runc-v2\x00-namespace\x00k8s.io\x00-id\x00" + id + "\x00-address\x00/run/k3s/containerd/containerd.sock\x00"
	}
	f.addProc(1, 0, "systemd", "/sbin/init\x00", "/init.scope")
	f.addProc(50, 1, "k3s-agent", "/bin/k3s\x00agent\x00", "/system.slice/k3s.service")
	// App pod: the container has its own process group, so only the parent tree reaches it.
	f.addProc(100, 1, "containerd-shim", shim("app"), "/system.slice/k3s.service")
	f.addProc(101, 100, "pause", "/pause\x00", podApp+"/cri-containerd-a1.scope")
	f.addProc(102, 100, "postgres", "postgres\x00", podApp+"/cri-containerd-a2.scope")
	// Longhorn instance-manager: a helper outside kubepods is the shim's only
	// direct child, so a walk that stops there misses tini, instance-manager and tgtd.
	f.addProc(200, 1, "containerd-shim", shim("lh"), "/system.slice/k3s.service")
	f.addProc(204, 200, "helper", "helper\x00", "/system.slice/k3s.service")
	f.addProc(201, 204, "tini", "tini\x00", podLonghorn+"/cri-containerd-l1.scope")
	f.addProc(202, 201, "instance-manager", "instance-manager\x00", podLonghorn+"/cri-containerd-l1.scope")
	f.addProc(203, 202, "tgtd", "tgtd\x00-f\x00", podLonghorn+"/cri-containerd-l1.scope")
	// A shim whose bundle config is unreadable.
	f.addProc(300, 1, "containerd-shim", shim("unk"), "/system.slice/k3s.service")
	f.addProc(301, 300, "app (x)", "x\x00", podUnknown+"/cri-containerd-u1.scope")

	f.files[taskRoot+"/k8s.io/app/config.json"] = `{"annotations":{"io.kubernetes.cri.sandbox-namespace":"apps"}}`
	f.files[taskRoot+"/k8s.io/lh/config.json"] = `{"annotations":{"io.kubernetes.cri.sandbox-namespace":"longhorn-system"}}`
	f.files["/proc/self/mountinfo"] = strings.Join([]string{
		"22 1 259:2 / / rw - ext4 /dev/nvme0n1p2 rw",
		"300 22 8:16 / " + globalMount + " rw - ext4 /dev/longhorn/pvc-1 rw",
		"301 22 8:16 / " + appMount + " rw - ext4 /dev/longhorn/pvc-1 rw",
	}, "\n")
	return f
}

func (f *fakeSystem) addProc(pid, ppid int, comm, cmdline, cgroup string) {
	dir := fmt.Sprintf("/proc/%d", pid)
	f.files[dir+"/stat"] = fmt.Sprintf("%d (%s) S %d %d %d", pid, comm, ppid, pid, pid)
	f.files[dir+"/cmdline"] = cmdline
	f.files[dir+"/cgroup"] = "0::" + cgroup + "\n"
}

func (f *fakeSystem) readFile(name string) ([]byte, error) {
	if cg, ok := strings.CutSuffix(name, "/cgroup.events"); ok {
		n, killed := f.killed[cg]
		if !killed || f.stuck || n < f.drainPolls {
			f.killed[cg] = n + 1
			return []byte("populated 1\nfrozen 0\n"), nil
		}
		f.events = append(f.events, "empty "+strings.TrimPrefix(cg, cgroupRoot))
		return []byte("populated 0\nfrozen 0\n"), nil
	}
	data, ok := f.files[name]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return []byte(data), nil
}

func (f *fakeSystem) writeFile(name string, data []byte) error {
	cg, ok := strings.CutSuffix(name, "/cgroup.kill")
	if !ok || string(data) != "1" {
		return errors.New("unexpected write " + name)
	}
	f.events = append(f.events, "kill "+strings.TrimPrefix(cg, cgroupRoot))
	if f.failKill[cg] {
		return errors.New("write cgroup.kill: EIO")
	}
	f.killed[cg] = 0
	return nil
}

func (f *fakeSystem) glob(pattern string) ([]string, error) {
	if pattern != "/proc/[0-9]*" {
		return nil, errors.New("unexpected glob " + pattern)
	}
	seen := map[string]bool{}
	for name := range f.files {
		if dir := path.Dir(name); strings.HasPrefix(dir, "/proc/") && dir != "/proc/self" {
			seen[dir] = true
		}
	}
	var dirs []string
	for dir := range seen {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs, nil
}

func (f *fakeSystem) unmount(target string) error {
	f.events = append(f.events, "umount "+target)
	if f.failUnmount[target] {
		return errors.New("umount: EBUSY")
	}
	return nil
}

func (f *fakeSystem) sync()                 { f.events = append(f.events, "sync") }
func (f *fakeSystem) now() time.Time        { return f.clock }
func (f *fakeSystem) sleep(d time.Duration) { f.clock = f.clock.Add(d) }

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestRunOrder(t *testing.T) {
	f := newFakeSystem()
	run(discard(), f)
	want := []string{
		"kill " + podApp,
		"empty " + podApp,
		"sync",
		"umount " + appMount,
		"umount " + globalMount,
		"kill " + podLonghorn,
		"kill " + podUnknown,
		"empty " + podLonghorn,
		"empty " + podUnknown,
	}
	if !reflect.DeepEqual(f.events, want) {
		t.Fatalf("events:\n%s\nwant:\n%s", strings.Join(f.events, "\n"), strings.Join(want, "\n"))
	}
}

func TestRunNeverKillsOutsideKubepods(t *testing.T) {
	f := newFakeSystem()
	run(discard(), f)
	for _, e := range f.events {
		if cg, ok := strings.CutPrefix(e, "kill "); ok && !strings.HasPrefix(cg, "/kubepods.slice/") {
			t.Fatalf("killed non-pod cgroup %q", cg)
		}
	}
}

func TestRunErrorsDoNotStopDrain(t *testing.T) {
	f := newFakeSystem()
	delete(f.files, "/proc/self/mountinfo")
	run(discard(), f)
	want := []string{
		"kill " + podApp,
		"empty " + podApp,
		"sync",
		"kill " + podLonghorn,
		"kill " + podUnknown,
		"empty " + podLonghorn,
		"empty " + podUnknown,
	}
	if !reflect.DeepEqual(f.events, want) {
		t.Fatalf("events = %q, want %q", f.events, want)
	}
}

func TestRunUnmountFailureDoesNotStopLaterUnmounts(t *testing.T) {
	f := newFakeSystem()
	f.failUnmount[appMount] = true
	run(discard(), f)
	want := []string{
		"kill " + podApp,
		"empty " + podApp,
		"sync",
		"umount " + appMount,
		"umount " + globalMount,
		"kill " + podLonghorn,
		"kill " + podUnknown,
		"empty " + podLonghorn,
		"empty " + podUnknown,
	}
	if !reflect.DeepEqual(f.events, want) {
		t.Fatalf("events:\n%s\nwant:\n%s", strings.Join(f.events, "\n"), strings.Join(want, "\n"))
	}
}

func TestRunKillFailureDoesNotStopLaterKills(t *testing.T) {
	f := newFakeSystem()
	f.failKill[cgroupRoot+podLonghorn] = true
	run(discard(), f)
	want := []string{
		"kill " + podApp,
		"empty " + podApp,
		"sync",
		"umount " + appMount,
		"umount " + globalMount,
		"kill " + podLonghorn,
		"kill " + podUnknown,
		"empty " + podUnknown,
	}
	if !reflect.DeepEqual(f.events, want) {
		t.Fatalf("events:\n%s\nwant:\n%s", strings.Join(f.events, "\n"), strings.Join(want, "\n"))
	}
}

// captureLog returns a logger that writes text records to a buffer.
func captureLog() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

func TestRunReportsFailureCounts(t *testing.T) {
	cases := []struct {
		name        string
		failUnmount []string
		failKill    []string
		want        string
	}{
		{name: "clean", want: "unmount_failed=0 kill_failed=0"},
		{name: "one unmount", failUnmount: []string{appMount}, want: "unmount_failed=1 kill_failed=0"},
		{name: "both unmounts", failUnmount: []string{appMount, globalMount}, want: "unmount_failed=2 kill_failed=0"},
		{name: "app pod kill", failKill: []string{cgroupRoot + podApp}, want: "unmount_failed=0 kill_failed=1"},
		{name: "both late kills", failKill: []string{cgroupRoot + podLonghorn, cgroupRoot + podUnknown}, want: "unmount_failed=0 kill_failed=2"},
		{name: "mixed steps", failUnmount: []string{appMount}, failKill: []string{cgroupRoot + podApp, cgroupRoot + podLonghorn}, want: "unmount_failed=1 kill_failed=2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeSystem()
			for _, target := range tc.failUnmount {
				f.failUnmount[target] = true
			}
			for _, cg := range tc.failKill {
				f.failKill[cg] = true
			}
			logger, buf := captureLog()
			run(logger, f)
			lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
			last := lines[len(lines)-1]
			if !strings.Contains(last, "msg=done "+tc.want) {
				t.Fatalf("final line %q, want msg=done %s", last, tc.want)
			}
		})
	}
}

func TestWaitEmptyIsBounded(t *testing.T) {
	f := newFakeSystem()
	f.stuck = true
	start := f.clock
	killPods(discard(), f, []shim{{pid: 100, cgroups: []string{podApp}}})
	if elapsed := f.clock.Sub(start); elapsed < emptyTimeout || elapsed > emptyTimeout+pollInterval {
		t.Fatalf("waited %v, want about %v", elapsed, emptyTimeout)
	}
}

func TestListShimsFindsContainerDescendants(t *testing.T) {
	got := listShims(discard(), newFakeSystem())
	want := []shim{
		{pid: 100, namespace: "apps", cgroups: []string{podApp}},
		{pid: 200, namespace: "longhorn-system", cgroups: []string{podLonghorn}},
		{pid: 300, namespace: "", cgroups: []string{podUnknown}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("listShims = %+v, want %+v", got, want)
	}
}

func TestParsePPid(t *testing.T) {
	cases := []struct {
		stat string
		want int
		ok   bool
	}{
		{"203 (tgtd) S 202 7350 7335", 202, true},
		{"7 (a (b) c) R 6 7 7", 6, true},
		{"garbage", 0, false},
	}
	for _, tc := range cases {
		got, ok := parsePPid([]byte(tc.stat))
		if got != tc.want || ok != tc.ok {
			t.Errorf("parsePPid(%q) = %d, %v; want %d, %v", tc.stat, got, ok, tc.want, tc.ok)
		}
	}
}

func TestPodCgroup(t *testing.T) {
	cases := map[string]string{
		podApp + "/cri-containerd-a1.scope": podApp,
		podApp:                              podApp,
		"/system.slice/k3s.service":         "",
		"/init.scope":                       "",
	}
	for in, want := range cases {
		if got := podCgroup(in); got != want {
			t.Errorf("podCgroup(%q) = %q, want %q", in, got, want)
		}
	}
}
