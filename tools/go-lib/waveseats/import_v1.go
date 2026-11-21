// SPDX-License-Identifier: MIT OR Apache-2.0

package waveseats

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ParseImportMap strictly parses every v2-only seat field required for v1 migration.
func ParseImportMap(text string) (map[string]ImportSeatFields, error) {
	var raw json.RawMessage
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return nil, fmt.Errorf("import map: invalid JSON: %w", err)
	}
	var rows map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil || rows == nil {
		return nil, errors.New("import map: must be an object keyed by seat name")
	}
	result := make(map[string]ImportSeatFields, len(rows))
	for name, rawRow := range rows {
		where := "import map." + name
		row, err := v2Object(rawRow, where, "tab", "profile", "role", "roleTreeRevision", "workspace", "persona", "key")
		if err != nil {
			return nil, err
		}
		fields := ImportSeatFields{}
		if fields.Tab, err = v2Text(row, "tab", where); err != nil {
			return nil, err
		}
		if fields.Profile, err = v2Text(row, "profile", where); err != nil {
			return nil, err
		}
		if fields.Role, err = v2Text(row, "role", where); err != nil {
			return nil, err
		}
		if fields.RoleTreeRevision, err = v2Uint(row, "roleTreeRevision", where); err != nil {
			return nil, err
		}
		if fields.Workspace, err = v2Text(row, "workspace", where); err != nil {
			return nil, err
		}
		if fields.Persona, err = v2Text(row, "persona", where); err != nil {
			return nil, err
		}
		if fields.Key, err = ParseKeyRequest(string(row["key"])); err != nil {
			return nil, err
		}
		result[name] = fields
	}
	return result, nil
}

// ParseSeatListV1ForImport strictly parses the legacy list accepted by import-v1.
func ParseSeatListV1ForImport(text string) ([]V1Entry, error) {
	var raw json.RawMessage
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return nil, fmt.Errorf("seat list v1: invalid JSON: %w", err)
	}
	root, err := v2Object(raw, "v1 root", "version", "seats")
	if err != nil {
		return nil, err
	}
	var version int
	if err := json.Unmarshal(root["version"], &version); err != nil || version != 1 {
		return nil, errors.New("seat list v1: version must be 1")
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(root["seats"], &rows); err != nil || rows == nil {
		return nil, errors.New("seat list v1: seats must be an array")
	}
	entries := make([]V1Entry, 0, len(rows))
	for index, row := range rows {
		entry, err := parseV1Entry(row, index)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func parseV1Entry(raw json.RawMessage, index int) (V1Entry, error) {
	where := fmt.Sprintf("v1 seats[%d]", index)
	row, err := v2Object(raw, where, "name", "cwd", "tab", "state", "freshAfter", "note", "profile", "updatedAt", "parent")
	if err != nil {
		return V1Entry{}, err
	}
	entry, err := parseV1RequiredFields(row, where)
	if err != nil {
		return V1Entry{}, err
	}
	if err := parseV1OptionalFields(row, where, &entry); err != nil {
		return V1Entry{}, err
	}
	return entry, nil
}

func parseV1RequiredFields(row map[string]json.RawMessage, where string) (V1Entry, error) {
	entry := V1Entry{}
	var err error
	if entry.Name, err = v2Text(row, "name", where); err != nil {
		return V1Entry{}, err
	}
	if entry.CWD, err = v2Text(row, "cwd", where); err != nil {
		return V1Entry{}, err
	}
	if entry.State, err = v2Text(row, "state", where); err != nil {
		return V1Entry{}, err
	}
	if entry.State != "active" && entry.State != "dormant" && entry.State != "retired" {
		return V1Entry{}, fmt.Errorf("seat list v1: %s.state must be active, dormant or retired", where)
	}
	if entry.UpdatedAt, err = v2Text(row, "updatedAt", where); err != nil {
		return V1Entry{}, err
	}
	if err := v2Timestamp(entry.UpdatedAt, where+".updatedAt"); err != nil {
		return V1Entry{}, err
	}
	return entry, nil
}

func parseV1OptionalFields(row map[string]json.RawMessage, where string, entry *V1Entry) error {
	if raw, ok := row["parent"]; ok {
		parent, err := rawText(raw)
		if err != nil {
			return v2Fail(where+".parent", "must be a non-empty string")
		}
		entry.Parent = &parent
	}
	if raw, ok := row["note"]; ok {
		note, err := rawText(raw)
		if err != nil {
			return v2Fail(where+".note", "must be a string")
		}
		entry.Note = &note
	}
	for _, field := range []string{"tab", "profile"} {
		if raw, ok := row[field]; ok {
			if _, err := rawText(raw); err != nil {
				return v2Fail(where+"."+field, "must be a non-empty string")
			}
		}
	}
	if raw, ok := row["freshAfter"]; ok {
		fresh, err := rawText(raw)
		if err != nil {
			return err
		}
		if err := v2Timestamp(fresh, where+".freshAfter"); err != nil {
			return err
		}
	}
	return nil
}

// ImportSeatListV1 maps a legacy list with the explicit import map and role policies.
func ImportSeatListV1(text string, inputs ImportInputs) (SeatListV2, error) {
	legacy, err := ParseSeatListV1ForImport(text)
	if err != nil {
		return SeatListV2{}, err
	}
	seen := make(map[string]struct{}, len(legacy))
	for _, entry := range legacy {
		seen[entry.Name] = struct{}{}
	}
	for name := range inputs.Seats {
		if _, ok := seen[name]; !ok {
			return SeatListV2{}, fmt.Errorf("seat list v1: import map has unknown seat %s; import refused", name)
		}
	}
	seats := make([]SeatEntryV2, 0, len(inputs.Seats))
	for _, old := range legacy {
		fields, ok := inputs.Seats[old.Name]
		if !ok {
			continue
		}
		policy, ok := inputs.Policy[fields.Role]
		if !ok {
			return SeatListV2{}, fmt.Errorf("seat list v1: role %s has no policy; import refused", fields.Role)
		}
		state := SeatStateV2(old.State)
		if state == "dormant" {
			state = SeatV2Paused
		}
		entry := SeatEntryV2{Name: old.Name, SeatRevision: 1, CWD: old.CWD, Tab: fields.Tab, State: state, UpdatedAt: old.UpdatedAt, Profile: fields.Profile, Role: fields.Role, RoleTreeRevision: fields.RoleTreeRevision, Key: KeySettings{Routing: fields.Key.Routing, Limits: fields.Key.Limits, PolicyID: policy.PolicyID}, Workspace: fields.Workspace, Persona: fields.Persona}
		entry.Parent = old.Parent
		if entry.Key.Routing == nil {
			entry.Key.Routing = map[Provider]any{}
		}
		if old.Note != nil {
			entry.Note = old.Note
		}
		seats = append(seats, entry)
	}
	list := SeatListV2{Version: 2, Revision: 1, Seats: seats}
	if _, err := SerializeSeatListV2(list); err != nil {
		return SeatListV2{}, err
	}
	return list, nil
}
