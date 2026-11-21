// SPDX-License-Identifier: MIT OR Apache-2.0

package waveseats

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
)

type orderedOperations struct {
	values map[string]OperationResult
	order  []string
}

func (operations orderedOperations) MarshalJSON() ([]byte, error) {
	var out bytes.Buffer
	out.WriteByte('{')
	seen := make(map[string]bool, len(operations.values))
	keys := slices.Clone(operations.order)
	remaining := make([]string, 0, len(operations.values))
	for key := range operations.values {
		if !slices.Contains(keys, key) {
			remaining = append(remaining, key)
		}
	}
	slices.Sort(remaining)
	keys = append(keys, remaining...)
	for _, key := range keys {
		value, exists := operations.values[key]
		if !exists || seen[key] {
			continue
		}
		if out.Len() > 1 {
			out.WriteByte(',')
		}
		encodedKey, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		encodedValue, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		out.Write(encodedKey)
		out.WriteByte(':')
		out.Write(encodedValue)
		seen[key] = true
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}

type orderedModels struct {
	values map[string]ModelLimit
	order  []string
}

func (models orderedModels) MarshalJSON() ([]byte, error) {
	var out bytes.Buffer
	out.WriteByte('{')
	keys := slices.Clone(models.order)
	var added []string
	for key := range models.values {
		if !slices.Contains(keys, key) {
			added = append(added, key)
		}
	}
	// Keys absent from the parsed order go last, sorted, so new maps serialize deterministically.
	slices.Sort(added)
	keys = append(keys, added...)
	seen := make(map[string]bool, len(keys))
	for _, key := range keys {
		value, ok := models.values[key]
		if !ok || seen[key] {
			continue
		}
		if out.Len() > 1 {
			out.WriteByte(',')
		}
		name, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		out.Write(name)
		out.WriteByte(':')
		out.Write(encoded)
		seen[key] = true
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}

func (limits KeyLimits) MarshalJSON() ([]byte, error) {
	var models *orderedModels
	if limits.PerModel != nil {
		models = &orderedModels{values: limits.PerModel, order: limits.modelOrder}
	}
	return json.Marshal(struct {
		KeyCap   *KeyCap        `json:"keyCap,omitempty"`
		PerModel *orderedModels `json:"perModel,omitempty"`
	}{KeyCap: limits.KeyCap, PerModel: models})
}

func (list SeatListV2) MarshalJSON() ([]byte, error) {
	var operations *orderedOperations
	if list.Operations != nil {
		operations = &orderedOperations{values: list.Operations, order: list.operationOrder}
	}
	return json.Marshal(struct {
		Version        int                `json:"version"`
		Revision       uint64             `json:"revision"`
		GenerationBase uint64             `json:"generationBase,omitempty"`
		Seats          []SeatEntryV2      `json:"seats"`
		Operations     *orderedOperations `json:"operations,omitempty"`
	}{Version: list.Version, Revision: list.Revision, GenerationBase: list.GenerationBase, Seats: list.Seats, Operations: operations})
}

// SerializeSeatListV2 returns the validated, indented on-disk representation.
func SerializeSeatListV2(list SeatListV2) ([]byte, error) {
	list.Version = 2
	if list.Seats == nil {
		list.Seats = []SeatEntryV2{}
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("seat list v2: encode JSON: %w", err)
	}
	data = bytes.ReplaceAll(data, []byte(`\u003c`), []byte("<"))
	data = bytes.ReplaceAll(data, []byte(`\u003e`), []byte(">"))
	data = bytes.ReplaceAll(data, []byte(`\u0026`), []byte("&"))
	data = bytes.ReplaceAll(data, []byte(`\u2028`), []byte(" "))
	data = bytes.ReplaceAll(data, []byte(`\u2029`), []byte(" "))
	data = append(data, '\n')
	if _, err := ParseSeatListV2(string(data)); err != nil {
		return nil, err
	}
	return data, nil
}

// DigestText returns the lowercase SHA-256 digest used by the seat journal.
func DigestText(text string) string {
	digest := sha256.Sum256([]byte(text))
	return hex.EncodeToString(digest[:])
}
