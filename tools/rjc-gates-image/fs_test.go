//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// mountinfoFixture uses the kernel's /proc/self/mountinfo row layout. Field 5
// (index 4) is the mount point; the rest are fixed-width noise the parser must
// skip. The second row is a prefix trap: /var/lib/rjc2 must not match
// /var/lib/rjc.
const mountinfoFixture = `22 1 0:21 / / rw,relatime shared:1 - ext4 /dev/root rw
35 22 0:33 / /var/lib/rjc2 rw,relatime shared:9 - btrfs /dev/sdb rw
36 22 0:34 / /var/lib/rjc rw,relatime shared:10 - btrfs /dev/sdb rw
`

func TestRealFSMountedReadsMountPointField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(path, []byte(mountinfoFixture), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	fsys := realFS{mountinfo: path}
	tests := []struct {
		name  string
		mount string
		want  bool
	}{
		{name: "active mount at field 5", mount: "/var/lib/rjc", want: true},
		{name: "trailing slash is cleaned", mount: "/var/lib/rjc/", want: true},
		{name: "prefix of an active mount is not mounted", mount: "/var/lib", want: false},
		{name: "longer path sharing the prefix is not mounted", mount: "/var/lib/rj", want: false},
		{name: "unlisted path is not mounted", mount: "/srv/rjc", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := fsys.Mounted(tc.mount)
			if err != nil {
				t.Fatalf("Mounted(%q): %v", tc.mount, err)
			}
			if got != tc.want {
				t.Fatalf("Mounted(%q) = %v, want %v", tc.mount, got, tc.want)
			}
		})
	}
}

func TestRealFSMountedMissingTableFailsClosed(t *testing.T) {
	// A missing mount table is an error, never "not mounted": a silent false
	// would let a mounted btrfs image be rebuilt under a live mount.
	fsys := realFS{mountinfo: filepath.Join(t.TempDir(), "absent")}
	if _, err := fsys.Mounted("/var/lib/rjc"); err == nil {
		t.Fatal("Mounted returned no error for a missing mount table")
	}
}

func TestFSTypeName(t *testing.T) {
	tests := []struct {
		name  string
		input int64
		want  string
	}{
		{name: "btrfs magic", input: 0x9123683e, want: fsBtrfs},
		{name: "ext4 magic", input: 0xef53, want: "0xef53"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := fsTypeName(tc.input); got != tc.want {
				t.Errorf("fsTypeName(0x%x) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
