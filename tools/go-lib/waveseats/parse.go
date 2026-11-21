// SPDX-License-Identifier: MIT OR Apache-2.0

package waveseats

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
)

func v2Fail(where, reason string) error {
	return fmt.Errorf("seat list v2: %s %s", where, reason)
}

func v2Object(raw json.RawMessage, where string, allowed ...string) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, v2Fail(where, "must be an object")
	}
	keys := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		keys[key] = struct{}{}
	}
	for key := range object {
		if _, ok := keys[key]; !ok {
			return nil, v2Fail(where, fmt.Sprintf("has unknown key %q", key))
		}
	}
	return object, nil
}

func v2Text(object map[string]json.RawMessage, key, where string) (string, error) {
	var value string
	if err := json.Unmarshal(object[key], &value); err != nil || value == "" {
		return "", v2Fail(where+"."+key, "must be a non-empty string")
	}
	return value, nil
}

func v2Uint(object map[string]json.RawMessage, key, where string) (uint64, error) {
	decoder := json.NewDecoder(bytes.NewReader(object[key]))
	decoder.UseNumber()
	var value json.Number
	if err := decoder.Decode(&value); err != nil {
		return 0, v2Fail(where+"."+key, "must be a safe unsigned integer")
	}
	parsed, err := value.Float64()
	if err != nil || math.IsInf(parsed, 0) || math.IsNaN(parsed) || parsed < 0 || math.Trunc(parsed) != parsed || parsed > maxSafeInteger {
		return 0, v2Fail(where+"."+key, "must be a safe unsigned integer")
	}
	return uint64(parsed), nil
}

func v2Timestamp(value, where string) error {
	if !validSeatTimestamp(value) {
		return v2Fail(where, "must be an ISO-8601 UTC timestamp")
	}
	return nil
}

func validSeatTimestamp(value string) bool {
	if !v2TimestampPattern.MatchString(value) || len(value) < 20 {
		return false
	}
	parse := func(start, end int) (int, bool) {
		if end > len(value) {
			return 0, false
		}
		result := 0
		for i := start; i < end; i++ {
			if value[i] < '0' || value[i] > '9' {
				return 0, false
			}
			result = result*10 + int(value[i]-'0')
		}
		return result, true
	}
	month, monthOK := parse(5, 7)
	day, dayOK := parse(8, 10)
	hour, hourOK := parse(11, 13)
	minute, minuteOK := parse(14, 16)
	second, secondOK := parse(17, 19)
	if !monthOK || !dayOK || !hourOK || !minuteOK || !secondOK || month < 1 || month > 12 || day < 1 || day > 31 || hour > 24 || minute > 59 || second > 59 {
		return false
	}
	return hour != 24 || (minute == 0 && second == 0 && seatTimestampFractionIsZero(value))
}

func seatTimestampFractionIsZero(value string) bool {
	if len(value) == 20 {
		return true
	}
	for _, digit := range value[20 : len(value)-1] {
		if digit != '0' {
			return false
		}
	}
	return true
}

// ParseSeatListV2 parses a closed v2 list, refusing unknown keys and duplicate names.
func ParseSeatListV2(text string) (SeatListV2, error) {
	var raw json.RawMessage
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return SeatListV2{}, fmt.Errorf("seat list v2: invalid JSON: %w", err)
	}
	root, err := v2Object(raw, "root", "version", "revision", "generationBase", "seats", "operations")
	if err != nil {
		return SeatListV2{}, err
	}
	var version int
	if err := json.Unmarshal(root["version"], &version); err != nil || version != 2 {
		return SeatListV2{}, v2Fail("version", "must be 2")
	}
	revision, err := v2Uint(root, "revision", "root")
	if err != nil {
		return SeatListV2{}, err
	}
	seats, err := parseV2Seats(root["seats"])
	if err != nil {
		return SeatListV2{}, err
	}
	list := SeatListV2{Version: 2, Revision: revision, Seats: seats}
	if _, ok := root["generationBase"]; ok {
		list.GenerationBase, err = v2Uint(root, "generationBase", "root")
		if err != nil {
			return SeatListV2{}, err
		}
	}
	if rawOperations, ok := root["operations"]; ok {
		list.Operations, list.operationOrder, err = parseV2Operations(rawOperations)
		if err != nil {
			return SeatListV2{}, err
		}
	}
	return list, nil
}

func parseV2Seats(raw json.RawMessage) ([]SeatEntryV2, error) {
	var input []json.RawMessage
	if err := json.Unmarshal(raw, &input); err != nil || input == nil {
		return nil, v2Fail("seats", "must be an array")
	}
	seats := make([]SeatEntryV2, 0, len(input))
	seen := make(map[string]struct{}, len(input))
	for index, rawSeat := range input {
		seat, err := parseV2Entry(rawSeat, index)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[seat.Name]; exists {
			return nil, v2Fail("seat "+seat.Name, "is duplicated")
		}
		seen[seat.Name] = struct{}{}
		seats = append(seats, seat)
	}
	if err := validateV2Parents(seats); err != nil {
		return nil, err
	}
	return seats, nil
}

// validateV2Parents refuses self-parents, dangling parents, root parents, and cycles.
func validateV2Parents(seats []SeatEntryV2) error {
	parents := make(map[string]*string, len(seats))
	for _, seat := range seats {
		parents[seat.Name] = seat.Parent
	}
	for _, seat := range seats {
		if seat.Parent == nil {
			continue
		}
		if seat.Name == ForemanRoot {
			return v2Fail("seat "+seat.Name, "takes no parent")
		}
		if *seat.Parent == ForemanRoot {
			return v2Fail("seat "+seat.Name, "cannot name the root foreman as parent")
		}
		if *seat.Parent == seat.Name {
			return v2Fail("seat "+seat.Name, "cannot be its own parent")
		}
		if _, exists := parents[*seat.Parent]; !exists {
			return v2Fail("seat "+seat.Name, fmt.Sprintf("has dangling parent %q", *seat.Parent))
		}
	}
	for _, seat := range seats {
		visited := make(map[string]struct{})
		for current := seat.Name; current != ""; {
			if _, exists := visited[current]; exists {
				return v2Fail("seat parent", fmt.Sprintf("cycle through %q", current))
			}
			visited[current] = struct{}{}
			parent, exists := parents[current]
			if !exists || parent == nil {
				break
			}
			current = *parent
		}
	}
	return nil
}

func parseV2Operations(raw json.RawMessage) (map[string]OperationResult, []string, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return nil, nil, v2Fail("operations", "must be an object")
	}
	operations := make(map[string]OperationResult)
	order := []string{}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, nil, v2Fail("operations", "must be an object")
		}
		id, ok := key.(string)
		if !ok {
			return nil, nil, v2Fail("operations", "must be an object")
		}
		var rawRecord json.RawMessage
		if err := decoder.Decode(&rawRecord); err != nil {
			return nil, nil, v2Fail("operations", "must be an object")
		}
		operation, err := parseV2Operation(id, rawRecord)
		if err != nil {
			return nil, nil, err
		}
		if _, exists := operations[id]; !exists {
			order = append(order, id)
		}
		operations[id] = operation
	}
	return operations, order, nil
}

func parseV2Operation(id string, raw json.RawMessage) (OperationResult, error) {
	where := "operations." + id
	row, err := v2Object(raw, where, "requestDigest", "result")
	if err != nil {
		return OperationResult{}, err
	}
	requestDigest, err := v2Text(row, "requestDigest", where)
	if err != nil {
		return OperationResult{}, err
	}
	resultRaw, err := v2Object(row["result"], where+".result", "revision", "entry", "policyDigest", "entitlementDigest")
	if err != nil {
		return OperationResult{}, err
	}
	result, err := parseV2Mutation(resultRaw, where)
	if err != nil {
		return OperationResult{}, err
	}
	return OperationResult{RequestDigest: requestDigest, Result: result}, nil
}

func parseV2Mutation(raw map[string]json.RawMessage, where string) (SeatMutation, error) {
	revision, err := v2Uint(raw, "revision", where+".result")
	if err != nil {
		return SeatMutation{}, err
	}
	entry, err := parseV2Entry(raw["entry"], 0)
	if err != nil {
		return SeatMutation{}, err
	}
	result := SeatMutation{Revision: revision, Entry: entry}
	if digest, ok := raw["policyDigest"]; ok {
		result.PolicyDigest, err = parseV2Digest(digest, where)
		if err != nil {
			return SeatMutation{}, err
		}
	}
	if digest, ok := raw["entitlementDigest"]; ok {
		result.EntitlementDigest, err = parseV2Digest(digest, where)
		if err != nil {
			return SeatMutation{}, err
		}
	}
	return result, nil
}

func parseV2Digest(raw json.RawMessage, where string) (*string, error) {
	value, err := v2Text(map[string]json.RawMessage{"digest": raw}, "digest", where+".result")
	if err != nil {
		return nil, err
	}
	return &value, nil
}
