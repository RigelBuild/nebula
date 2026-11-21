// SPDX-License-Identifier: MIT OR Apache-2.0

package waveseats

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// ParsePolicyTable parses CUE's rolePolicies export.
func ParsePolicyTable(text string) (map[string]RolePolicy, error) {
	var raw json.RawMessage
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return nil, fmt.Errorf("policy table: invalid JSON: %w", err)
	}
	root, err := v2Object(raw, "policy table", "rolePolicies", "policies")
	if err != nil {
		return nil, err
	}
	var policies map[string]json.RawMessage
	if err := json.Unmarshal(root["policies"], &policies); err != nil || policies == nil {
		return nil, errors.New("policy table: policies must be an object")
	}
	var roles map[string]json.RawMessage
	if err := json.Unmarshal(root["rolePolicies"], &roles); err != nil || roles == nil {
		return nil, errors.New("policy table: must be an object of role to {team, policyId}")
	}
	result := make(map[string]RolePolicy, len(roles))
	for role, rawRow := range roles {
		where := "policy table." + role
		row, err := v2Object(rawRow, where, "team", "policyId")
		if err != nil {
			return nil, err
		}
		team, err := v2Text(row, "team", where)
		if err != nil {
			return nil, err
		}
		policyID, err := v2Text(row, "policyId", where)
		if err != nil {
			return nil, err
		}
		result[role] = RolePolicy{Team: team, PolicyID: policyID}
	}
	return result, nil
}

// ValidateEntitlement checks a seat's role and requested provider routing.
func ValidateEntitlement(text, name, role string, key KeyRequest) error {
	var table map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &table); err != nil || table == nil {
		return errors.New("permission-denied: invalid entitlement roles")
	}
	raw, ok := table[name]
	if !ok {
		return errors.New("permission-denied: missing seat entitlement")
	}
	row, err := v2Object(raw, "entitlements."+name, "revision", "roles", "routing")
	if err != nil {
		return err
	}
	if _, err := v2Uint(row, "revision", "entitlements."+name); err != nil {
		return err
	}
	var roles []string
	if err := json.Unmarshal(row["roles"], &roles); err != nil || roles == nil {
		return errors.New("permission-denied: invalid entitlement roles")
	}
	if !slices.Contains(roles, role) {
		return errors.New("permission-denied: role not entitled")
	}
	var allowed map[string][]json.RawMessage
	if err := json.Unmarshal(row["routing"], &allowed); err != nil || allowed == nil {
		return errors.New("permission-denied: invalid entitlement routing")
	}
	for provider, selection := range key.Routing {
		if !entitlementAllowsRouting(allowed[string(provider)], selection) {
			return fmt.Errorf("permission-denied: routing not entitled for %s", provider)
		}
	}
	return nil
}

func entitlementAllowsRouting(options []json.RawMessage, selection any) bool {
	for _, option := range options {
		var shared string
		selected, isString := selection.(string)
		if json.Unmarshal(option, &shared) == nil && shared == sharedRouting && isString && selected == sharedRouting {
			return true
		}
		var accounts []string
		if json.Unmarshal(option, &accounts) != nil {
			continue
		}
		requested, ok := selection.([]string)
		if ok && equalStrings(accounts, requested) {
			return true
		}
	}
	return false
}

// ValidatePolicy rejects requested model and key limits that exceed the selected policy.
func ValidatePolicy(text, policyID string, key KeyRequest) error {
	policy, err := selectedPolicy(text, policyID)
	if err != nil {
		return err
	}
	var models []string
	if err := json.Unmarshal(policy["models"], &models); err != nil || models == nil {
		return errors.New("permission-denied: invalid policy models")
	}
	var allowedModels map[string]json.RawMessage
	if err := json.Unmarshal(policy["perModel"], &allowedModels); err != nil || allowedModels == nil {
		return errors.New("permission-denied: invalid policy model limits")
	}
	for model, limits := range key.Limits.PerModel {
		if !slices.Contains(models, model) {
			return fmt.Errorf("permission-denied: model not allowed: %s", model)
		}
		rawAllowed, ok := allowedModels[model]
		if !ok {
			return fmt.Errorf("permission-denied: model not allowed: %s", model)
		}
		var allowed map[string]json.RawMessage
		if err := json.Unmarshal(rawAllowed, &allowed); err != nil || allowed == nil {
			return errors.New("permission-denied: invalid policy model limits")
		}
		if err := validateModelLimits(allowed, model, limits); err != nil {
			return err
		}
	}
	return validateKeyCap(policy, key.Limits.KeyCap)
}

func selectedPolicy(text, policyID string) (map[string]json.RawMessage, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &root); err != nil || root == nil {
		return nil, errors.New("permission-denied: missing key policy")
	}
	var policies map[string]json.RawMessage
	if err := json.Unmarshal(root["policies"], &policies); err != nil || policies == nil {
		return nil, errors.New("permission-denied: missing key policy")
	}
	raw, ok := policies[policyID]
	if !ok {
		return nil, errors.New("permission-denied: missing key policy")
	}
	var policy map[string]json.RawMessage
	if err := json.Unmarshal(raw, &policy); err != nil || policy == nil {
		return nil, errors.New("permission-denied: invalid key policy")
	}
	return policy, nil
}

func validateModelLimits(allowed map[string]json.RawMessage, model string, limits ModelLimit) error {
	for _, field := range []struct {
		name      string
		requested *uint64
	}{{"tokens", limits.Tokens}, {"tpm", limits.TPM}, {"rpm", limits.RPM}} {
		if field.requested == nil {
			continue
		}
		var maximum uint64
		if err := json.Unmarshal(allowed[field.name], &maximum); err != nil || *field.requested > maximum {
			return fmt.Errorf("permission-denied: model %s %s exceeds policy", model, field.name)
		}
	}
	return nil
}

func validateKeyCap(policy map[string]json.RawMessage, requested *KeyCap) error {
	if requested == nil {
		return nil
	}
	rawAllowed, ok := policy["keyCap"]
	if !ok {
		return errors.New("permission-denied: missing policy key cap")
	}
	var allowed KeyCap
	if err := json.Unmarshal(rawAllowed, &allowed); err != nil {
		return errors.New("permission-denied: missing policy key cap")
	}
	if requested.USD > allowed.USD {
		return errors.New("permission-denied: key cap exceeds policy")
	}
	if !sameWindow(requested.Window, allowed.Window) {
		return errors.New("permission-denied: key cap window exceeds policy")
	}
	return nil
}

func sameWindow(left, right Window) bool {
	return left.Kind == right.Kind && left.Seconds == right.Seconds && left.AnchorUTC == right.AnchorUTC && left.Timezone == right.Timezone
}
