//go:build windows

package envstore

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var user32 = windows.NewLazySystemDLL("user32.dll")
var sendMessageTimeout = user32.NewProc("SendMessageTimeoutW")

type windowsEnvStore struct {
	key registry.Key
}

// New opens the current user's environment store.
func New() (Store, error) {
	key, _, err := registry.CreateKey(registry.CURRENT_USER, environmentKey, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", environmentRegistryPath, err)
	}
	return &windowsEnvStore{key: key}, nil
}

func (s *windowsEnvStore) Close() error {
	return s.key.Close()
}

func (s *windowsEnvStore) Get(name string) (string, error) {
	value, _, err := s.key.GetStringValue(name)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("reading environment variable %s: %w", name, err)
	}
	return value, nil
}

func (s *windowsEnvStore) Set(name, value string) error {
	_, existingType, err := s.key.GetValue(name, nil)
	found := err == nil
	if err != nil && !errors.Is(err, registry.ErrNotExist) {
		return fmt.Errorf("reading environment variable type %s: %w", name, err)
	}
	if valueType(existingType, found) == registryExpandString {
		err = s.key.SetExpandStringValue(name, value)
	} else {
		err = s.key.SetStringValue(name, value)
	}
	if err != nil {
		return fmt.Errorf("writing environment variable %s: %w", name, err)
	}
	return nil
}

func (s *windowsEnvStore) Delete(name string) error {
	if err := s.key.DeleteValue(name); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return fmt.Errorf("deleting environment variable %s: %w", name, err)
	}
	return nil
}

func (s *windowsEnvStore) Broadcast() error {
	message, err := windows.UTF16PtrFromString(environmentSettingName)
	if err != nil {
		return fmt.Errorf("encoding environment setting name: %w", err)
	}
	result, _, callErr := sendMessageTimeout.Call(
		0xffff,
		uintptr(wmSettingChange),
		0,
		uintptr(unsafe.Pointer(message)), //nolint:gosec // G103: Win32 LPARAM takes the UTF-16 setting name pointer.
		0x0002,
		5000,
		0,
	)
	if result == 0 {
		if !errors.Is(callErr, syscall.Errno(0)) {
			return fmt.Errorf("broadcasting environment change: %w", callErr)
		}
		return errors.New("broadcasting environment change failed")
	}
	return nil
}

// ReplaceFile atomically replaces dst with src.
func ReplaceFile(src, dst string) error {
	from, err := windows.UTF16PtrFromString(src)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(dst)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING)
}
