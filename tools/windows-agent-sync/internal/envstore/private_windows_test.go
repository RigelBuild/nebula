//go:build windows

package envstore

import (
	"os"
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
	committed := filepath.Join(dir, "example")
	// The second run replaces the export the first run committed.
	for run := range 2 {
		temp := writePrivateFile(t, dir, []byte{byte('a' + run)})
		assertPrivateDACL(t, temp)
		if err := ReplaceFile(temp, committed); err != nil {
			t.Fatal(err)
		}
		if _, err := os.ReadFile(committed); err != nil { //nolint:gosec // committed is under t.TempDir.
			t.Fatalf("run %d: reading committed export: %v", run, err)
		}
		assertPrivateDACL(t, committed)
	}
	assertPrivateDACL(t, dir)
}

func writePrivateFile(t *testing.T, dir string, data []byte) string {
	t.Helper()
	file, err := CreatePrivateFile(dir, ".example-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	name := file.Name()
	if _, err := file.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return name
}

func assertPrivateDACL(t *testing.T, path string) {
	t.Helper()
	userSID, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	user, err := windows.StringToSid(userSID)
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		t.Fatal(err)
	}
	if !owner.Equals(user) {
		t.Fatalf("%s owner = %s, want %s", path, owner.String(), userSID)
	}
	control, _, err := sd.Control()
	if err != nil {
		t.Fatal(err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("%s DACL is not protected", path)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatal(err)
	}
	if dacl.AceCount != 2 {
		t.Fatalf("%s ACE count = %d, want 2", path, dacl.AceCount)
	}
	seen := map[string]bool{}
	for index := range uint32(dacl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			t.Fatal(err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			t.Fatalf("%s ACE %d type = %d, want allow", path, index, ace.Header.AceType)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)) //nolint:gosec // G103: the SID starts at SidStart inside the ACE.
		switch {
		case sid.Equals(user):
			seen["user"] = true
		case sid.Equals(system):
			seen["system"] = true
		default:
			t.Fatalf("%s ACE %d grants unexpected SID %s", path, index, sid.String())
		}
	}
	if !seen["user"] || !seen["system"] {
		t.Fatalf("%s ACEs = %v, want user and SYSTEM", path, seen)
	}
}
