// SPDX-License-Identifier: MIT OR Apache-2.0

package waveseats

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"slices"
)

func validateV2Window(raw json.RawMessage, where string) (Window, error) {
	row, err := v2Object(raw, where, "kind", "seconds", "anchorUtc", "timezone")
	if err != nil {
		return Window{}, err
	}
	kind, err := v2Text(row, "kind", where)
	if err != nil {
		return Window{}, err
	}
	seconds, err := v2Uint(row, "seconds", where)
	if err != nil {
		return Window{}, err
	}
	window := Window{Kind: kind, Seconds: seconds}
	switch kind {
	case "rolling":
		if _, ok := row["anchorUtc"]; ok {
			return Window{}, v2Fail(where, "rolling window has fixed fields")
		}
		if _, ok := row["timezone"]; ok {
			return Window{}, v2Fail(where, "rolling window has fixed fields")
		}
	case "fixed":
		window.AnchorUTC, err = v2Text(row, "anchorUtc", where)
		if err != nil {
			return Window{}, err
		}
		window.Timezone, err = v2Text(row, "timezone", where)
		if err != nil {
			return Window{}, err
		}
	default:
		return Window{}, v2Fail(where, "has invalid window kind")
	}
	return window, nil
}

func parseV2Routing(raw json.RawMessage, where string) (map[Provider]any, error) {
	var input map[string]json.RawMessage
	if err := json.Unmarshal(raw, &input); err != nil || input == nil {
		return nil, v2Fail(where+".routing", "must be an object")
	}
	routing := make(map[Provider]any, len(input))
	for provider, rawSelection := range input {
		if provider != string(ProviderClaude) && provider != string(ProviderCodex) {
			return nil, v2Fail(where+".routing", fmt.Sprintf("has unknown key %q", provider))
		}
		var selection any
		if err := json.Unmarshal(rawSelection, &selection); err != nil {
			return nil, v2Fail(where+".routing."+provider, `must be "shared" or string[]`)
		}
		if shared, ok := selection.(string); ok && shared == sharedRouting {
			routing[Provider(provider)] = sharedRouting
			continue
		}
		var accounts []string
		if err := json.Unmarshal(rawSelection, &accounts); err != nil || accounts == nil {
			return nil, v2Fail(where+".routing."+provider, `must be "shared" or string[]`)
		}
		if slices.Contains(accounts, "") {
			return nil, v2Fail(where+".routing."+provider, `must be "shared" or string[]`)
		}
		routing[Provider(provider)] = accounts
	}
	return routing, nil
}

func parseV2Limits(limits map[string]json.RawMessage, where string) (KeyLimits, error) {
	keyLimits := KeyLimits{}
	if capRaw, ok := limits["keyCap"]; ok {
		keyCap, err := parseV2KeyCap(capRaw, where)
		if err != nil {
			return KeyLimits{}, err
		}
		keyLimits.KeyCap = keyCap
	}
	if modelsRaw, ok := limits["perModel"]; ok {
		models, err := parseV2ModelLimits(modelsRaw, where)
		if err != nil {
			return KeyLimits{}, err
		}
		keyLimits.PerModel = models
		decoder := json.NewDecoder(bytes.NewReader(modelsRaw))
		if _, err := decoder.Token(); err != nil {
			return KeyLimits{}, err
		}
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return KeyLimits{}, err
			}
			var value json.RawMessage
			if err := decoder.Decode(&value); err != nil {
				return KeyLimits{}, err
			}
			name, ok := key.(string)
			if !ok {
				return KeyLimits{}, v2Fail(where+".limits.perModel", "must be an object")
			}
			keyLimits.modelOrder = append(keyLimits.modelOrder, name)
		}
	}
	return keyLimits, nil
}

func parseV2KeyCap(raw json.RawMessage, where string) (*KeyCap, error) {
	capWhere := where + ".limits.keyCap"
	row, err := v2Object(raw, capWhere, "usd", "window")
	if err != nil {
		return nil, err
	}
	var usd float64
	if err := json.Unmarshal(row["usd"], &usd); err != nil || math.IsNaN(usd) || math.IsInf(usd, 0) || usd < 0 {
		return nil, v2Fail(capWhere+".usd", "must be a non-negative number")
	}
	window, err := validateV2Window(row["window"], capWhere+".window")
	if err != nil {
		return nil, err
	}
	return &KeyCap{USD: usd, Window: window}, nil
}

func parseV2ModelLimits(raw json.RawMessage, where string) (map[string]ModelLimit, error) {
	const modelsSuffix = ".limits.perModel"
	var rows map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil || rows == nil {
		return nil, v2Fail(where+modelsSuffix, "must be an object")
	}
	result := make(map[string]ModelLimit, len(rows))
	for model, rawLimits := range rows {
		limits, err := parseV2ModelLimit(rawLimits, where+modelsSuffix+"."+model)
		if err != nil {
			return nil, err
		}
		result[model] = limits
	}
	return result, nil
}

func parseV2ModelLimit(raw json.RawMessage, where string) (ModelLimit, error) {
	row, err := v2Object(raw, where, "tokens", "tpm", "rpm")
	if err != nil {
		return ModelLimit{}, err
	}
	var result ModelLimit
	for _, field := range []struct {
		name  string
		value **uint64
	}{{"tokens", &result.Tokens}, {"tpm", &result.TPM}, {"rpm", &result.RPM}} {
		if _, ok := row[field.name]; !ok {
			continue
		}
		value, err := v2Uint(row, field.name, where)
		if err != nil {
			return ModelLimit{}, err
		}
		*field.value = &value
	}
	return result, nil
}

func validateV2Key(raw json.RawMessage, where string) (KeySettings, error) {
	row, err := v2Object(raw, where, "routing", "limits", "policyId")
	if err != nil {
		return KeySettings{}, err
	}
	routing, err := parseV2Routing(row["routing"], where)
	if err != nil {
		return KeySettings{}, err
	}
	limits, err := v2Object(row["limits"], where+".limits", "keyCap", "perModel")
	if err != nil {
		return KeySettings{}, err
	}
	keyLimits, err := parseV2Limits(limits, where)
	if err != nil {
		return KeySettings{}, err
	}
	policyID, err := v2Text(row, "policyId", where)
	if err != nil {
		return KeySettings{}, err
	}
	return KeySettings{Routing: routing, Limits: keyLimits, PolicyID: policyID}, nil
}
