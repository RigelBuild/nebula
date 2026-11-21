package inventory

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var sessionIDPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type sessionEntry struct {
	Session struct {
		ID string `json:"id"`
	} `json:"session"`
}

func sessionID(absolutePath, relativePath string) string {
	file, err := os.Open(absolutePath) //nolint:gosec // Paths come from the configured walk roots.
	if err != nil {
		return ""
	}
	defer func() {
		_ = file.Close()
	}()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		var entry sessionEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if entry.Session.ID != "" {
			return entry.Session.ID
		}
		var kind struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &kind); err == nil && kind.Type == "session" && kind.ID != "" {
			return kind.ID
		}
	}
	stem := strings.TrimSuffix(filepath.Base(relativePath), filepath.Ext(relativePath))
	if sessionIDPattern.MatchString(stem) {
		return stem
	}
	return ""
}
