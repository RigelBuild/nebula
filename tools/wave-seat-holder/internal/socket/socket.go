// Package socket resolves seat socket paths and the directory that holds them.
package socket

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// Environment reports a variable's value and whether it is set, as os.LookupEnv does.
type Environment func(string) (string, bool)

const (
	linuxPathLimit  = 107
	darwinPathLimit = 103
)

var validName = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// Path returns the socket path for name under dir. It rejects names and paths the platform cannot bind.
func Path(name, dir, platform string) (string, error) {
	if !validName.MatchString(name) {
		quoted, err := QuoteJSON(name)
		if err != nil {
			return "", fmt.Errorf("quote seat name: %w", err)
		}
		return "", fmt.Errorf("invalid seat name: %s", quoted)
	}
	path := filepath.Join(dir, name+".sock")
	maxBytes := linuxPathLimit
	if platform == "darwin" {
		maxBytes = darwinPathLimit
	}
	if len(path) > maxBytes {
		return "", fmt.Errorf("seat socket path is %d bytes; maximum is %d bytes", len(path), maxBytes)
	}
	return path, nil
}

// Directory returns WAVE_SEAT_SOCKET_DIR when set, else cotal-seats under XDG_RUNTIME_DIR.
func Directory(getenv Environment) (string, error) {
	if configured, ok := getenv("WAVE_SEAT_SOCKET_DIR"); ok {
		return configured, nil
	}
	runtimeDir, _ := getenv("XDG_RUNTIME_DIR") // An unset or empty XDG_RUNTIME_DIR both fail below.
	if runtimeDir == "" {
		return "", errors.New("XDG_RUNTIME_DIR must be set when no socket directory is given")
	}
	return filepath.Join(runtimeDir, "cotal-seats"), nil
}

// QuoteJSON returns value as a JSON string literal. HTML is not escaped, so names round-trip byte for byte.
func QuoteJSON(value string) (string, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "", fmt.Errorf("quote JSON string: %w", err)
	}
	return strings.TrimSuffix(buf.String(), "\n"), nil
}
