package main

import (
	"reflect"
	"testing"
)

func TestLonghornKubeletMounts(t *testing.T) {
	mountinfo := `22 1 259:2 / / rw,relatime shared:1 - ext4 /dev/nvme0n1p2 rw
300 22 8:16 / /var/lib/kubelet/plugins/kubernetes.io/csi/driver.longhorn.io/abc/globalmount rw shared:200 - ext4 /dev/longhorn/pvc-1 rw
301 22 8:16 / /var/lib/kubelet/pods/uid1/volumes/kubernetes.io~csi/pvc-1/mount rw shared:200 - ext4 /dev/longhorn/pvc-1 rw
302 22 0:50 / /var/lib/kubelet/pods/uid1/volumes/kubernetes.io~projected/kube-api-access rw - tmpfs tmpfs rw
303 22 8:32 / /mnt/other rw - ext4 /dev/longhorn/pvc-2 rw
304 22 8:48 / /var/lib/kubelet/pods/uid\0402/volumes/kubernetes.io~csi/pvc-3/mount rw - xfs /dev/longhorn/pvc-3 rw
`
	got := longhornKubeletMounts(parseMountinfo(mountinfo))
	want := []string{
		"/var/lib/kubelet/pods/uid 2/volumes/kubernetes.io~csi/pvc-3/mount",
		"/var/lib/kubelet/pods/uid1/volumes/kubernetes.io~csi/pvc-1/mount",
		"/var/lib/kubelet/plugins/kubernetes.io/csi/driver.longhorn.io/abc/globalmount",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("longhornKubeletMounts = %q, want %q", got, want)
	}
}

func TestLonghornKubeletMountsPodBeforeGlobalmount(t *testing.T) {
	got := longhornKubeletMounts([]mount{
		{source: "/dev/longhorn/pvc-1", target: "/var/lib/kubelet/plugins/kubernetes.io/csi/driver.longhorn.io/abc/globalmount"},
		{source: "/dev/longhorn/pvc-1", target: "/var/lib/kubelet/pods/u/volumes/kubernetes.io~csi/pvc-1/mount"},
		{source: "/dev/longhorn/pvc-1", target: "/var/lib/kubelet/pods/u/volumes/kubernetes.io~csi/pvc-1/mount/sub"},
	})
	want := []string{
		"/var/lib/kubelet/pods/u/volumes/kubernetes.io~csi/pvc-1/mount/sub",
		"/var/lib/kubelet/pods/u/volumes/kubernetes.io~csi/pvc-1/mount",
		"/var/lib/kubelet/plugins/kubernetes.io/csi/driver.longhorn.io/abc/globalmount",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %q, want %q", got, want)
	}
	for i, target := range got {
		for _, later := range got[i+1:] {
			if len(later) > len(target) && later[:len(target)+1] == target+"/" {
				t.Fatalf("%q unmounted before its child %q", target, later)
			}
		}
	}
}

func TestShimArgs(t *testing.T) {
	cases := []struct {
		name      string
		cmdline   string
		namespace string
		id        string
		ok        bool
	}{
		{"shim", "/var/lib/rancher/k3s/data/x/bin/containerd-shim-runc-v2\x00-namespace\x00k8s.io\x00-id\x00abc123\x00-address\x00/run/k3s/containerd/containerd.sock\x00", "k8s.io", "abc123", true},
		{"other namespace", "containerd-shim-runc-v2\x00-namespace\x00moby\x00-id\x00x\x00", "moby", "x", true},
		{"other binary", "/usr/bin/containerd\x00-id\x00abc\x00", "", "", false},
		{"no id", "containerd-shim-runc-v2\x00-namespace\x00k8s.io\x00", "k8s.io", "", false},
		{"empty", "", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			namespace, id, ok := shimArgs([]byte(tc.cmdline))
			if namespace != tc.namespace || id != tc.id || ok != tc.ok {
				t.Fatalf("shimArgs = %q, %q, %v; want %q, %q, %v", namespace, id, ok, tc.namespace, tc.id, tc.ok)
			}
		})
	}
}

func TestPodNamespace(t *testing.T) {
	cases := []struct {
		name   string
		config string
		want   string
	}{
		{"sandbox key", `{"annotations":{"io.kubernetes.cri.sandbox-namespace":"longhorn-system"}}`, "longhorn-system"},
		{"pod key", `{"annotations":{"io.kubernetes.pod.namespace":"apps"}}`, "apps"},
		{"sandbox wins", `{"annotations":{"io.kubernetes.cri.sandbox-namespace":"a","io.kubernetes.pod.namespace":"b"}}`, "a"},
		{"missing", `{"annotations":{"other":"x"}}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := podNamespace([]byte(tc.config))
			if err != nil || got != tc.want {
				t.Fatalf("podNamespace = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	if _, err := podNamespace([]byte("{")); err == nil {
		t.Fatal("podNamespace accepted invalid JSON")
	}
}

func TestPartitionShims(t *testing.T) {
	shims := []shim{
		{pid: 1, namespace: "longhorn-system"},
		{pid: 2, namespace: "apps"},
		{pid: 3, namespace: ""},
		{pid: 4, namespace: "longhorn-system"},
	}
	apps, late := partitionShims(shims)
	if !reflect.DeepEqual(apps, []shim{shims[1]}) {
		t.Fatalf("apps = %v", apps)
	}
	// An unknown namespace fails closed: it might be the iSCSI target.
	if !reflect.DeepEqual(late, []shim{shims[0], shims[2], shims[3]}) {
		t.Fatalf("late = %v", late)
	}
}
