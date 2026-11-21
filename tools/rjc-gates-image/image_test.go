package main

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// fakeFS records every mutating call and returns scripted results.
type fakeFS struct {
	exists     bool
	existsErr  error
	fsType     string
	fsTypeErr  error
	nocow      bool
	nocowErr   error
	setErr     error
	mounted    bool
	mountedErr error
	truncErr   error
	mkfsErr    error
	renameErr  error
	syncErr    error

	calls []string
}

func (f *fakeFS) Exists(string) (bool, error) {
	f.record("exists")
	return f.exists, f.existsErr
}

func (f *fakeFS) FilesystemType(string) (string, error) {
	f.record("fstype")
	return f.fsType, f.fsTypeErr
}

func (f *fakeFS) NoCOW(string) (bool, error) {
	f.record("nocow")
	return f.nocow, f.nocowErr
}

func (f *fakeFS) CreateEmpty(string) error {
	f.record("create")
	return nil
}

func (f *fakeFS) SetNoCOW(string) error {
	f.record("setnocow")
	return f.setErr
}

func (f *fakeFS) Truncate(path string, _ int64) error {
	f.record("truncate:" + path)
	return f.truncErr
}

func (f *fakeFS) Mkfs(string) error {
	f.record("mkfs")
	return f.mkfsErr
}

func (f *fakeFS) Rename(from, to string) error {
	f.record("rename:" + from + "->" + to)
	return f.renameErr
}

func (f *fakeFS) Sync(path string) error {
	f.record("sync:" + path)
	return f.syncErr
}

func (f *fakeFS) Mounted(string) (bool, error) {
	f.record("mounted")
	return f.mounted, f.mountedErr
}

func (f *fakeFS) record(call string) { f.calls = append(f.calls, call) }

var testConfig = config{
	image:     "/var/lib/rjc.img",
	temporary: "/var/lib/rjc.img.tmp",
	mount:     "/var/lib/rjc",
	size:      200 << 30,
}

func noLog(string) {}

func TestPrepareBuildsMissingImageSparseNOCOW(t *testing.T) {
	// The new image is truncated to its apparent size, never preallocated.
	// Only the temporary file is sized; the live image is only renamed.
	fsys := &fakeFS{exists: false}
	if err := prepare(fsys, testConfig, noLog); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	want := []string{
		"exists",
		"create", "setnocow", "truncate:/var/lib/rjc.img.tmp", "mkfs",
		"sync:/var/lib/rjc.img.tmp",
		"rename:/var/lib/rjc.img.tmp->/var/lib/rjc.img",
		"sync:/var/lib",
	}
	if !reflect.DeepEqual(fsys.calls, want) {
		t.Fatalf("calls = %v, want %v", fsys.calls, want)
	}
}

func TestPrepareExistingImageIsNeverPreallocated(t *testing.T) {
	// An image that already exists must not be sized or preallocated again:
	// truncate and fallocate are both absent from the ext4 path.
	fsys := &fakeFS{exists: true, fsType: "0xef53", nocow: false}
	if err := prepare(fsys, testConfig, noLog); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	for _, call := range fsys.calls {
		if strings.HasPrefix(call, "truncate:") {
			t.Fatalf("existing image was sized again: %v", fsys.calls)
		}
	}
}

func TestPrepareFsyncFailureDoesNotRename(t *testing.T) {
	// An unsynced image must never take the live name: a crash after the
	// rename could leave a partly written ext4 at the image path.
	syncErr := errors.New("fsync: EIO")
	fsys := &fakeFS{exists: false, syncErr: syncErr}
	if err := prepare(fsys, testConfig, noLog); !errors.Is(err, syncErr) {
		t.Fatalf("err = %v, want wrapped %v", err, syncErr)
	}
	if contains(fsys.calls, "rename:/var/lib/rjc.img.tmp->/var/lib/rjc.img") {
		t.Fatalf("unsynced image was renamed into place: %v", fsys.calls)
	}
}

func TestPrepareExt4KeepsImage(t *testing.T) {
	// ext4 can never carry NOCOW, so rebuilding would wipe the caches on every
	// boot. The image must be kept.
	fsys := &fakeFS{exists: true, fsType: "0xef53", nocow: false}
	if err := prepare(fsys, testConfig, noLog); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if slices.Contains(fsys.calls, "create") || slices.Contains(fsys.calls, "rename:/var/lib/rjc.img.tmp->/var/lib/rjc.img") {
		t.Fatalf("ext4 image was rebuilt: %v", fsys.calls)
	}
}

func TestPrepareBtrfsMigratesImageWithoutNOCOW(t *testing.T) {
	fsys := &fakeFS{exists: true, fsType: fsBtrfs, nocow: false}
	if err := prepare(fsys, testConfig, noLog); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if !contains(fsys.calls, "rename:/var/lib/rjc.img.tmp->/var/lib/rjc.img") {
		t.Fatalf("btrfs image without NOCOW was not migrated: %v", fsys.calls)
	}
}

func TestPrepareBtrfsKeepsNOCOWImage(t *testing.T) {
	fsys := &fakeFS{exists: true, fsType: fsBtrfs, nocow: true}
	if err := prepare(fsys, testConfig, noLog); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if contains(fsys.calls, "create") {
		t.Fatalf("NOCOW image was rebuilt: %v", fsys.calls)
	}
}

func TestPrepareBtrfsMountedWithoutNOCOWKeepsImageAndWarns(t *testing.T) {
	fsys := &fakeFS{exists: true, fsType: fsBtrfs, nocow: false, mounted: true}
	var logged []string
	log := func(message string) { logged = append(logged, message) }
	if err := prepare(fsys, testConfig, log); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if contains(fsys.calls, "create") {
		t.Fatalf("mounted image was rebuilt: %v", fsys.calls)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "rebuild it on the next boot") {
		t.Fatalf("expected one next-boot warning, got %v", logged)
	}
}

func TestPrepareAttrReadFailureFailsClosed(t *testing.T) {
	// A failed attribute read must be an error, never a rebuild.
	readErr := errors.New("ioctl FS_IOC_GETFLAGS: permission denied")
	fsys := &fakeFS{exists: true, fsType: fsBtrfs, nocowErr: readErr}
	err := prepare(fsys, testConfig, noLog)
	if !errors.Is(err, readErr) {
		t.Fatalf("err = %v, want wrapped %v", err, readErr)
	}
	if contains(fsys.calls, "create") {
		t.Fatalf("image was rebuilt after a failed read: %v", fsys.calls)
	}
}

func TestPrepareFilesystemTypeFailureFailsClosed(t *testing.T) {
	statErr := errors.New("statfs: EIO")
	fsys := &fakeFS{exists: true, fsTypeErr: statErr}
	if err := prepare(fsys, testConfig, noLog); !errors.Is(err, statErr) {
		t.Fatalf("err = %v, want wrapped %v", err, statErr)
	}
	if contains(fsys.calls, "create") {
		t.Fatalf("image was rebuilt after a failed statfs: %v", fsys.calls)
	}
}

func TestPrepareChattrNotSupportedIsIgnored(t *testing.T) {
	// EOPNOTSUPP is what ext4 returns for chattr +C. The build continues.
	fsys := &fakeFS{exists: false, setErr: errOperationNotSupported}
	if err := prepare(fsys, testConfig, noLog); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if !contains(fsys.calls, "rename:/var/lib/rjc.img.tmp->/var/lib/rjc.img") {
		t.Fatalf("build stopped on EOPNOTSUPP: %v", fsys.calls)
	}
}

func TestPrepareChattrOtherErrorFails(t *testing.T) {
	chattrErr := errors.New("chattr: Permission denied")
	fsys := &fakeFS{exists: false, setErr: chattrErr}
	err := prepare(fsys, testConfig, noLog)
	if !errors.Is(err, chattrErr) {
		t.Fatalf("err = %v, want wrapped %v", err, chattrErr)
	}
	if contains(fsys.calls, "rename:/var/lib/rjc.img.tmp->/var/lib/rjc.img") {
		t.Fatalf("image was renamed into place after chattr failed: %v", fsys.calls)
	}
}

func TestPrepareTruncateFailureDoesNotRename(t *testing.T) {
	// A failed truncate leaves only the temporary file. It must never take the
	// live image name, or a half-built image would be mounted at boot.
	truncErr := errors.New("ftruncate: EFBIG")
	fsys := &fakeFS{exists: false, truncErr: truncErr}
	if err := prepare(fsys, testConfig, noLog); !errors.Is(err, truncErr) {
		t.Fatalf("err = %v, want wrapped %v", err, truncErr)
	}
	if contains(fsys.calls, "rename:/var/lib/rjc.img.tmp->/var/lib/rjc.img") {
		t.Fatalf("image was renamed into place after truncate failed: %v", fsys.calls)
	}
	if contains(fsys.calls, "mkfs") {
		t.Fatalf("image was formatted after truncate failed: %v", fsys.calls)
	}
}

func TestPrepareMkfsFailureDoesNotRename(t *testing.T) {
	mkfsErr := errors.New("mkfs.ext4: no space")
	fsys := &fakeFS{exists: false, mkfsErr: mkfsErr}
	if err := prepare(fsys, testConfig, noLog); !errors.Is(err, mkfsErr) {
		t.Fatalf("err = %v, want wrapped %v", err, mkfsErr)
	}
	if contains(fsys.calls, "rename:/var/lib/rjc.img.tmp->/var/lib/rjc.img") {
		t.Fatalf("unformatted image was renamed into place: %v", fsys.calls)
	}
}

func contains(calls []string, want string) bool {
	return slices.Contains(calls, want)
}
