//go:build windows

package envstore

import (
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestPrivateDACL(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secrets")
	if err := MkdirPrivate(dir); err != nil {
		t.Fatal(err)
	}
	// A second call fixes an existing directory instead of failing.
	if err := MkdirPrivate(dir); err != nil {
		t.Fatal(err)
	}
	file, err := CreatePrivateFile(dir, ".example-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	name := file.Name()
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dir, name} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			assertPrivateDACL(t, path)
		})
	}
}

func assertPrivateDACL(t *testing.T, path string) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := sd.Control()
	if err != nil {
		t.Fatal(err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatal("DACL is not protected")
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	owner, err := windows.CreateWellKnownSid(windows.WinCreatorOwnerRightsSid)
	if err != nil {
		t.Fatal(err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatal(err)
	}
	if dacl.AceCount != 2 {
		t.Fatalf("ACE count = %d, want 2", dacl.AceCount)
	}
	seen := map[string]bool{}
	for index := range uint32(dacl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			t.Fatal(err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			t.Fatalf("ACE %d type = %d, want allow", index, ace.Header.AceType)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)) //nolint:gosec // G103: the SID starts at SidStart inside the ACE.
		switch {
		case sid.Equals(owner):
			seen["owner"] = true
		case sid.Equals(system):
			seen["system"] = true
		default:
			t.Fatalf("ACE %d grants unexpected SID %s", index, sid.String())
		}
	}
	if !seen["owner"] || !seen["system"] {
		t.Fatalf("ACEs = %v, want owner and SYSTEM", seen)
	}
}
