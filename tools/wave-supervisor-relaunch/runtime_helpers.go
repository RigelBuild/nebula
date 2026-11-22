package main

import (
	"context"
	"sort"
)

func sortedNames(values map[string]struct{}) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
func mapKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func callText(ctx context.Context, fn func(context.Context) (string, error)) string {
	if fn == nil {
		return ""
	}
	value, err := fn(ctx)
	if err != nil {
		return ""
	}
	return value
}
func probeCall(ctx context.Context, runner processRunner, args []string) (ProbeCall, error) {
	result, err := runner(ctx, args, "")
	if err != nil {
		return ProbeCall{}, err
	}
	return ProbeCall{ExitCode: result.ExitCode, Stdout: result.Stdout}, nil
}
func uncapped(ctx context.Context, deps runDeps) bool {
	identity := deps.ZellijIdentity(ctx)
	return identityRE.MatchString(identity) && !ownedByUnit(identity, callText(ctx, deps.ZellijUnitProcs))
}
