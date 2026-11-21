package main

import (
	"fmt"
	"path/filepath"
	"regexp"
)

var seatName = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func socketPath(name, dir, platform string) (string, error) {
	if !seatName.MatchString(name) {
		return "", fmt.Errorf("invalid seat name %q", name)
	}
	maxLen := 107
	if platform == "darwin" {
		maxLen = 103
	}
	path := filepath.Join(dir, name+".sock")
	if len(path) > maxLen {
		return "", fmt.Errorf("seat socket path is %d bytes; maximum is %d bytes", len(path), maxLen)
	}
	return path, nil
}
