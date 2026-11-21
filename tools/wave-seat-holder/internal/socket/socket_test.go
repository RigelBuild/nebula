package socket

import (
	"strings"
	"testing"
)

func TestPathBuildsValidatedPathAndRejectsInvalidNames(t *testing.T) {
	got, err := Path("seat_1", "/run/cotal-seats", "linux")
	if err != nil || got != "/run/cotal-seats/seat_1.sock" {
		t.Fatalf("Path = %q, %v; want /run/cotal-seats/seat_1.sock", got, err)
	}
	for _, name := range []string{"", "bad/name", "bad name", "bad:name"} {
		if _, err := Path(name, "/run/cotal-seats", "linux"); err == nil {
			t.Errorf("Path(%q) accepted an invalid name", name)
		}
	}
}

func TestPathAllowsLinuxPathsUpTo107BytesAndRejectsLonger(t *testing.T) {
	exact := "/" + strings.Repeat("a", 99)
	got, err := Path("x", exact, "linux")
	if err != nil || len(got) != 107 {
		t.Fatalf("exact path = %q (%d bytes), %v; want 107 bytes", got, len(got), err)
	}
	_, err = Path("x", "/"+strings.Repeat("a", 100), "linux")
	assertErrorContains(t, err, "107 bytes")
}

func TestPathEnforcesDarwinAndLinuxByteLimitsIndependently(t *testing.T) {
	darwin, err := Path("x", "/"+strings.Repeat("a", 95), "darwin")
	if err != nil || len(darwin) != 103 {
		t.Fatalf("darwin exact path = %q (%d bytes), %v; want 103 bytes", darwin, len(darwin), err)
	}
	_, err = Path("x", "/"+strings.Repeat("a", 96), "darwin")
	assertErrorContains(t, err, "103 bytes")

	linux, err := Path("x", "/"+strings.Repeat("a", 99), "linux")
	if err != nil || len(linux) != 107 {
		t.Fatalf("linux exact path = %q (%d bytes), %v; want 107 bytes", linux, len(linux), err)
	}
	_, err = Path("x", "/"+strings.Repeat("a", 100), "linux")
	assertErrorContains(t, err, "107 bytes")
}

func TestDirectoryUsesConfiguredOrRuntimeDirectory(t *testing.T) {
	configured, err := Directory(lookupMap(map[string]string{"WAVE_SEAT_SOCKET_DIR": "/run/configured"}))
	if err != nil || configured != "/run/configured" {
		t.Fatalf("configured directory = %q, %v; want /run/configured", configured, err)
	}
	runtimeDir, err := Directory(lookupMap(map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000"}))
	if err != nil || runtimeDir != "/run/user/1000/cotal-seats" {
		t.Fatalf("runtime directory = %q, %v; want /run/user/1000/cotal-seats", runtimeDir, err)
	}
	_, err = Directory(lookupMap(nil))
	assertErrorContains(t, err, "XDG_RUNTIME_DIR")
}

// lookupMap returns an Environment over values; a missing key reports unset.
func lookupMap(values map[string]string) Environment {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func assertErrorContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want one containing %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q, want one containing %q", err.Error(), want)
	}
}
