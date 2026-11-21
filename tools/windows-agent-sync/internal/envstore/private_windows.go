//go:build windows

package envstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// currentUserSID returns the string SID of the user in the process token.
func currentUserSID() (sid string, err error) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return "", fmt.Errorf("opening process token: %w", err)
	}
	defer func() {
		if closeErr := token.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("closing process token: %w", closeErr))
		}
	}()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("reading token user: %w", err)
	}
	return user.User.Sid.String(), nil
}

func privateAttributes() (*windows.SecurityAttributes, *windows.SECURITY_DESCRIPTOR, error) {
	userSID, err := currentUserSID()
	if err != nil {
		return nil, nil, err
	}
	sd, err := windows.SecurityDescriptorFromString(privateSDDL(userSID))
	if err != nil {
		return nil, nil, fmt.Errorf("parsing private security descriptor: %w", err)
	}
	attributes := &windows.SecurityAttributes{SecurityDescriptor: sd}
	attributes.Length = uint32(unsafe.Sizeof(*attributes))
	return attributes, sd, nil
}

// MkdirPrivate creates dir owned by the current user with a protected DACL that
// grants only that user and SYSTEM, and applies the same owner and DACL to an
// existing dir.
func MkdirPrivate(dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return err
	}
	attributes, sd, err := privateAttributes()
	if err != nil {
		return err
	}
	name, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	if err := windows.CreateDirectory(name, attributes); err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return fmt.Errorf("creating private directory: %w", err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return fmt.Errorf("reading private owner: %w", err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("reading private DACL: %w", err)
	}
	info := windows.SECURITY_INFORMATION(windows.OWNER_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION)
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, info, owner, nil, dacl, nil); err != nil {
		return fmt.Errorf("applying private DACL: %w", err)
	}
	return nil
}

// CreatePrivateFile creates a new file in dir, named by pattern with its "*"
// replaced by a random string, with a protected DACL that grants only the owner
// and SYSTEM.
func CreatePrivateFile(dir, pattern string) (*os.File, error) {
	attributes, _, err := privateAttributes()
	if err != nil {
		return nil, err
	}
	for range privateNameAttempts {
		base, err := privateTempName(pattern)
		if err != nil {
			return nil, err
		}
		full := filepath.Join(dir, base)
		name, err := windows.UTF16PtrFromString(full)
		if err != nil {
			return nil, err
		}
		handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, attributes, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
		if errors.Is(err, windows.ERROR_FILE_EXISTS) || errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("creating private file: %w", err)
		}
		return os.NewFile(uintptr(handle), full), nil
	}
	return nil, errPrivateNameExhausted
}
