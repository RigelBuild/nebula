// Package envstore persists user environment variables in the Windows registry.
package envstore

const (
	registryString       uint32 = 1
	registryExpandString uint32 = 2
	wmSettingChange             = 0x001A
	// environmentSettingName is the WM_SETTINGCHANGE lParam, not a registry key.
	environmentSettingName = "Environment"
	// environmentKey is the subkey under HKCU that holds user environment values.
	environmentKey          = "Environment"
	environmentRegistryPath = `HKCU\` + environmentKey
)

func valueType(existing uint32, found bool) uint32 {
	if found && existing == registryExpandString {
		return registryExpandString
	}
	return registryString
}

// Store reads and writes persistent user environment variables.
type Store interface {
	Get(name string) (string, error)
	Set(name, value string) error
	Delete(name string) error
	Broadcast() error
}
