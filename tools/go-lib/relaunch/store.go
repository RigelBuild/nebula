// SPDX-License-Identifier: MIT OR Apache-2.0

package relaunch

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var restartIdentityRE = regexp.MustCompile(`^[0-9]+:[0-9]+$`)

type RelaunchState struct {
	Version         int      `json:"version"`
	Spawns          []string `json:"spawns"`
	LastFingerprint string   `json:"lastFingerprint,omitempty"`
	LastNoticeAt    string   `json:"lastNoticeAt,omitempty"`
	Burned          []string `json:"burned"`
}

type Hold struct {
	Until  string `json:"until"`
	Reason string `json:"reason"`
}

type RestartRequest struct {
	At       string `json:"at"`
	Reason   string `json:"reason"`
	Cause    string `json:"cause"`
	Identity string `json:"identity,omitempty"`
}

func EmptyState() RelaunchState {
	return RelaunchState{Version: 1, Spawns: []string{}, Burned: []string{}}
}

func missing(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || (err != nil && err.Error() == "missing")
}

func decodeObject(data []byte) (map[string]any, bool) {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, false
	}
	row, ok := value.(map[string]any)
	return row, ok
}

func validState(value any) (RelaunchState, bool) {
	row, ok := value.(map[string]any)
	if !ok || row["version"] != float64(1) {
		return RelaunchState{}, false
	}
	spawnsRaw, ok := row["spawns"].([]any)
	if !ok {
		return RelaunchState{}, false
	}
	burnedRaw, ok := row["burned"].([]any)
	if !ok {
		return RelaunchState{}, false
	}
	state := EmptyState()
	state.Spawns = make([]string, 0, len(spawnsRaw))
	for _, item := range spawnsRaw {
		switch value := item.(type) {
		case string:
			state.Spawns = append(state.Spawns, value)
		case float64:
			stamp, valid := spawnStamp(value)
			if !valid {
				return RelaunchState{}, false
			}
			state.Spawns = append(state.Spawns, stamp)
		default:
			return RelaunchState{}, false
		}
	}
	state.Burned = make([]string, 0, len(burnedRaw))
	for _, item := range burnedRaw {
		value, ok := item.(string)
		if !ok {
			return RelaunchState{}, false
		}
		state.Burned = append(state.Burned, value)
	}
	if value, exists := row["lastFingerprint"]; exists {
		state.LastFingerprint, ok = value.(string)
		if !ok {
			return RelaunchState{}, false
		}
	}
	if value, exists := row["lastNoticeAt"]; exists {
		state.LastNoticeAt, ok = value.(string)
		if !ok {
			return RelaunchState{}, false
		}
	}
	return state, true
}

func ReadState(path string, fileSystem FileSystem) (RelaunchState, bool) {
	if fileSystem == nil {
		fileSystem = NodeFileSystem{}
	}
	data, err := fileSystem.ReadFile(path)
	if err != nil {
		if missing(err) {
			return EmptyState(), true
		}
		return RelaunchState{}, false
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return RelaunchState{}, false
	}
	state, ok := validState(value)
	return state, ok
}

func WriteState(path string, state RelaunchState, fileSystem FileSystem) error {
	return atomicJSON(path, state, fileSystem)
}

// Store readers treat unreadable files as absent without deleting them.
func readStoreFile(path string, fileSystem FileSystem) ([]byte, bool) {
	data, err := fileSystem.ReadFile(path)
	return data, err == nil
}

func parseTimestamp(value string) (time.Time, bool) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	return parsed, err == nil
}

func ReadHold(path string, now time.Time, fileSystem FileSystem) (*Hold, bool, error) {
	if fileSystem == nil {
		fileSystem = NodeFileSystem{}
	}
	data, readable := readStoreFile(path, fileSystem)
	if !readable {
		return nil, false, nil
	}
	row, valid := decodeObject(data)
	if !valid {
		return nil, false, nil
	}
	until, untilOK := row["until"].(string)
	reason, reasonOK := row["reason"].(string)
	if !untilOK || !reasonOK {
		return nil, false, nil
	}
	parsed, valid := parseTimestamp(until)
	if !valid {
		return nil, false, nil
	}
	if !parsed.After(now) {
		return nil, true, nil
	}
	hold := &Hold{Until: parsed.UTC().Format("2006-01-02T15:04:05.000Z"), Reason: reason}
	return hold, false, nil
}

func WriteHold(path string, hold Hold, fileSystem FileSystem) error {
	return atomicJSON(path, hold, fileSystem)
}

func ReleaseHold(path string, fileSystem FileSystem) error {
	return remove(fileSystem, path)
}

func ReadRestartRequest(path string, now time.Time, fileSystem FileSystem) (*RestartRequest, bool, error) {
	if fileSystem == nil {
		fileSystem = NodeFileSystem{}
	}
	data, readable := readStoreFile(path, fileSystem)
	if !readable {
		return nil, false, nil
	}
	row, valid := decodeObject(data)
	if !valid {
		return nil, true, nil
	}
	at, atOK := row["at"].(string)
	reason, reasonOK := row["reason"].(string)
	if !atOK || !reasonOK || strings.TrimSpace(reason) == "" {
		return nil, true, nil
	}
	parsed, valid := parseTimestamp(at)
	if !valid || parsed.After(now) || now.Sub(parsed) >= 30*time.Minute {
		return nil, true, nil
	}
	cause := "wedge"
	if value, exists := row["cause"]; exists {
		var ok bool
		cause, ok = value.(string)
		if !ok {
			return nil, true, nil
		}
	}
	if cause != "wedge" && cause != "rss" {
		return nil, true, nil
	}
	request := &RestartRequest{At: parsed.UTC().Format("2006-01-02T15:04:05.000Z"), Reason: reason, Cause: cause}
	if cause == "rss" {
		identity, ok := row["identity"].(string)
		if !ok || !restartIdentityRE.MatchString(identity) {
			return nil, true, nil
		}
		request.Identity = identity
	}
	return request, false, nil
}

func WriteRestartRequest(path string, request RestartRequest, fileSystem FileSystem) error {
	return atomicJSON(path, request, fileSystem)
}

func ReleaseRestartRequest(path string, fileSystem FileSystem) error {
	return remove(fileSystem, path)
}

func remove(fileSystem FileSystem, path string) error {
	if fileSystem == nil {
		fileSystem = NodeFileSystem{}
	}
	err := fileSystem.Remove(path)
	if err != nil && !missing(err) {
		return fmt.Errorf("removing %q: %w", path, err)
	}
	return nil
}

func atomicJSON(path string, value any, fileSystem FileSystem) error {
	if fileSystem == nil {
		fileSystem = NodeFileSystem{}
	}
	if err := fileSystem.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating state directory: %w", err)
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding state: %w", err)
	}
	data = append(data, '\n')
	temporary := path + "." + strconv.Itoa(os.Getpid()) + ".tmp"
	if err := fileSystem.WriteFile(temporary, data, 0o666); err != nil {
		return fmt.Errorf("writing temporary state: %w", err)
	}
	if err := fileSystem.Rename(temporary, path); err != nil {
		removeErr := fileSystem.Remove(temporary)
		return errors.Join(fmt.Errorf("renaming state: %w", err), removeErr)
	}
	return nil
}

func spawnStamp(value any) (string, bool) {
	number, ok := value.(float64)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) || math.Abs(number) > 8.64e15 {
		return "", false
	}
	clipped := math.Trunc(number)
	if math.Abs(clipped) > 8.64e15 {
		return "", false
	}
	return javascriptISOString(int64(clipped)), true
}

func javascriptISOString(milliseconds int64) string {
	timestamp := time.UnixMilli(milliseconds).UTC()
	year := timestamp.Year()
	var prefix string
	switch {
	case year >= 0 && year <= 9999:
		prefix = fmt.Sprintf("%04d", year)
	case year < 0:
		prefix = fmt.Sprintf("-%06d", -year)
	default:
		prefix = fmt.Sprintf("+%06d", year)
	}
	return prefix + timestamp.Format("-01-02T15:04:05.000Z")
}
