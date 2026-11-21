// SPDX-License-Identifier: MIT OR Apache-2.0

package woodpecker

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"slices"
)

// dispatchVariablesJSON is the checked-in list of pipeline variables a CD
// dispatch sends. The Woodpecker server's manual-run allowlist must match it.
//
//go:embed dispatch_variables.json
var dispatchVariablesJSON []byte

// CDDispatchVariables returns the pipeline variables a CD dispatch sends, in
// the order the file lists them. It returns an error when the embedded file
// is not a JSON array of strings.
func CDDispatchVariables() ([]string, error) {
	var names []string
	if err := json.Unmarshal(dispatchVariablesJSON, &names); err != nil {
		return nil, fmt.Errorf("parse dispatch variables: %w", err)
	}
	return names, nil
}

// checkDispatchKeys returns an error when the variables a dispatch is about
// to send differ from CDDispatchVariables, so a key drift fails before any
// pipeline starts.
func checkDispatchKeys(variables map[string]string) error {
	want, err := CDDispatchVariables()
	if err != nil {
		return err
	}
	got := make([]string, 0, len(variables))
	for key := range variables {
		got = append(got, key)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		return fmt.Errorf("dispatch variables %v, want %v", got, want)
	}
	return nil
}
