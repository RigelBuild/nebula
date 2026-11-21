// SPDX-License-Identifier: MIT OR Apache-2.0

package waveseats

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

func parseV2Entry(raw json.RawMessage, index int, hosts HostSet) (SeatEntryV2, error) {
	where := fmt.Sprintf("seats[%d]", index)
	row, err := v2Object(raw, where, "name", "seatRevision", "cwd", "tab", "state", "restartGeneration", "parent", "note", "updatedAt", "host", "profile", "role", "roleTreeRevision", "key", "workspace", "persona")
	if err != nil {
		return SeatEntryV2{}, err
	}
	entry := SeatEntryV2{}
	if entry.Name, err = v2Text(row, "name", where); err != nil {
		return SeatEntryV2{}, err
	}
	if !v2SeatNamePattern.MatchString(entry.Name) {
		return SeatEntryV2{}, v2Fail(where+".name", "is invalid")
	}
	if entry.CWD, err = v2Text(row, "cwd", where); err != nil {
		return SeatEntryV2{}, err
	}
	if !filepath.IsAbs(entry.CWD) {
		return SeatEntryV2{}, v2Fail(where+".cwd", "must be absolute")
	}
	if entry.Tab, err = v2Text(row, "tab", where); err != nil {
		return SeatEntryV2{}, err
	}
	if strings.TrimSpace(entry.Tab) == "" || strings.TrimSpace(entry.Tab) != entry.Tab {
		return SeatEntryV2{}, v2Fail(where+".tab", "must not be blank or padded")
	}
	if err = json.Unmarshal(row["state"], &entry.State); err != nil || (entry.State != SeatV2Active && entry.State != SeatV2Paused && entry.State != SeatV2Retired) {
		return SeatEntryV2{}, v2Fail(where+".state", "must be one of active, paused, retired")
	}
	if entry.SeatRevision, err = v2Uint(row, "seatRevision", where); err != nil {
		return SeatEntryV2{}, err
	}
	if entry.RestartGeneration, err = v2Uint(row, "restartGeneration", where); err != nil {
		return SeatEntryV2{}, err
	}
	if entry.UpdatedAt, err = v2Text(row, "updatedAt", where); err != nil {
		return SeatEntryV2{}, err
	}
	if err := v2Timestamp(entry.UpdatedAt, where+".updatedAt"); err != nil {
		return SeatEntryV2{}, err
	}
	if entry.Profile, err = v2Text(row, "profile", where); err != nil {
		return SeatEntryV2{}, err
	}
	if entry.Role, err = v2Text(row, "role", where); err != nil {
		return SeatEntryV2{}, err
	}
	if entry.RoleTreeRevision, err = v2Uint(row, "roleTreeRevision", where); err != nil {
		return SeatEntryV2{}, err
	}
	if entry.Workspace, err = v2Text(row, "workspace", where); err != nil {
		return SeatEntryV2{}, err
	}
	if entry.Persona, err = v2Text(row, "persona", where); err != nil {
		return SeatEntryV2{}, err
	}
	if entry.Key, err = validateV2Key(row["key"], where+".key"); err != nil {
		return SeatEntryV2{}, err
	}
	if err := parseV2OptionalEntryFields(row, where, &entry, hosts); err != nil {
		return SeatEntryV2{}, err
	}
	return entry, nil
}

func parseV2OptionalEntryFields(row map[string]json.RawMessage, where string, entry *SeatEntryV2, hosts HostSet) error {
	var err error
	if value, ok := row["parent"]; ok {
		entry.Parent, err = parseV2Parent(value, where)
		if err != nil {
			return err
		}
	}
	if value, ok := row["note"]; ok {
		entry.Note, err = parseV2Note(value, where)
		if err != nil {
			return err
		}
	}
	if value, ok := row["host"]; ok {
		entry.Host, err = parseV2Host(value, where, hosts)
		if err != nil {
			return err
		}
	}
	return nil
}

func parseV2Parent(raw json.RawMessage, where string) (*string, error) {
	parent, err := rawText(raw)
	if err != nil || parent == "" {
		return nil, v2Fail(where+".parent", "must be a non-empty string")
	}
	return &parent, nil
}

func parseV2Note(raw json.RawMessage, where string) (*string, error) {
	var note string
	if err := json.Unmarshal(raw, &note); err != nil {
		return nil, v2Fail(where+".note", "must be a string")
	}
	return &note, nil
}

func parseV2Host(raw json.RawMessage, where string, hosts HostSet) (*SeatHost, error) {
	var host SeatHost
	if err := json.Unmarshal(raw, &host); err != nil {
		return nil, v2Fail(where+".host", "must be a configured host name")
	}
	if _, ok := hosts[host]; !ok {
		return nil, v2Fail(where+".host", "must be one of the configured hosts: "+configuredHostNames(hosts))
	}
	return &host, nil
}

// configuredHostNames lists the set's host names in sorted order for an error message.
func configuredHostNames(hosts HostSet) string {
	if len(hosts) == 0 {
		return "(none configured)"
	}
	names := make([]string, 0, len(hosts))
	for host := range hosts {
		names = append(names, string(host))
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}

func rawText(raw json.RawMessage) (string, error) {
	var text string
	err := json.Unmarshal(raw, &text)
	return text, err
}
