package main

import (
	"encoding/json"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/RigelBuild/nebula/tools/go-lib/cotalreach"
	"github.com/RigelBuild/nebula/tools/go-lib/relaunch"
)

type SupervisorState struct {
	Kind      string
	OrphanSet []string
}

type parsedRows struct {
	names map[string]struct{}
	live  map[string]struct{}
	valid bool
}

func ParsePsAgentNames(psNDJSON string) map[string]struct{} {
	return parseRows(psNDJSON).names
}

func parseRows(output string) parsedRows {
	names := make(map[string]struct{})
	_, rows, valid := cotalreach.ParsePsItems(output)
	for _, row := range rows {
		var name string
		if json.Unmarshal(row["name"], &name) != nil {
			valid = false
			continue
		}
		names[name] = struct{}{}
	}
	return parsedRows{names: names, live: cotalreach.ParsePresentAgentNames(output), valid: valid}
}

type endpointRows struct {
	eligible          map[string]struct{}
	supervisorPresent bool
}

var bracketedEndpointID = regexp.MustCompile(`^\[[^\]]+\]$`)
var offlineWord = regexp.MustCompile(`(?i)\boffline\b`)

func parseEndpoints(text, name, endpointKind string, eligibleNames map[string]struct{}) endpointRows {
	eligible := make(map[string]struct{})
	supervisorPresent := false
	for line := range strings.SplitSeq(text, "\n") {
		endpointName := onlineAgentEndpoint(line, endpointKind)
		if endpointName == "" {
			continue
		}
		isSupervisor := endpointName == name
		if isSupervisor {
			supervisorPresent = true
		}
		if isSupervisor {
			eligible[endpointName] = struct{}{}
		} else if _, ok := eligibleNames[endpointName]; ok {
			eligible[endpointName] = struct{}{}
		}
	}
	return endpointRows{eligible: eligible, supervisorPresent: supervisorPresent}
}

func onlineAgentEndpoint(line, endpointKind string) string {
	columns := strings.Fields(strings.TrimSpace(line))
	if len(columns) == 0 {
		return ""
	}
	endpointName, _, _ := strings.Cut(columns[0], "/")
	kindIndex := -1
	for index, column := range columns {
		if index > 0 && (column == endpointKind || column == "endpoint") {
			kindIndex = index
			break
		}
	}
	if kindIndex < 0 && len(columns) > 1 && bracketedEndpointID.MatchString(columns[1]) {
		for index, column := range columns {
			if index > 1 && (column == endpointKind || column == "endpoint") {
				kindIndex = index
				break
			}
		}
	}
	if kindIndex < 0 || kindIndex+1 > len(columns) || endpointName == "" || columns[kindIndex] != endpointKind {
		return ""
	}
	if offlineWord.MatchString(strings.Join(columns[kindIndex+1:], " ")) {
		return ""
	}
	return endpointName
}

func ParseEligibleEndpointNames(text, rootSeat, endpointKind string, eligibleNames map[string]struct{}) map[string]struct{} {
	return parseEndpoints(text, rootSeat, endpointKind, eligibleNames).eligible
}

func classifySupervisor(psNDJSON, endpointsText, rootSeat, endpointKind string, eligibleNames map[string]struct{}) SupervisorState {
	return classifySupervisorForRoot(psNDJSON, endpointsText, eligibleNames, rootSeat, endpointKind)
}

func classifySupervisorForRoot(psNDJSON, endpointsText string, eligibleNames map[string]struct{}, name, endpointKind string) SupervisorState {
	ps := parseRows(psNDJSON)
	if !ps.valid {
		return SupervisorState{Kind: stateInvalid}
	}
	endpoints := parseEndpoints(endpointsText, name, endpointKind, eligibleNames)
	orphans := make([]string, 0)
	for endpointName := range endpoints.eligible {
		if _, ok := ps.names[endpointName]; !ok {
			orphans = append(orphans, endpointName)
		}
	}
	sort.Strings(orphans)
	if _, ok := ps.names[name]; ok {
		if _, live := ps.live[name]; !live {
			return SupervisorState{Kind: stateStarting}
		}
	}
	if slices.Contains(orphans, name) {
		return SupervisorState{Kind: stateOrphaned, OrphanSet: orphans}
	}
	if _, ok := ps.live[name]; ok {
		return SupervisorState{Kind: stateLive}
	}
	if endpoints.supervisorPresent {
		return SupervisorState{Kind: stateStarting}
	}
	return SupervisorState{Kind: "absent"}
}
func holdActive(deps runDeps) bool {
	hold, expired, err := relaunch.ReadHold(deps.Paths.Hold, deps.now(), deps.FS)
	return err == nil && hold != nil && !expired
}
func readRestartRequest(deps runDeps) (*relaunch.RestartRequest, bool) {
	request, stale, err := relaunch.ReadRestartRequest(deps.Paths.RestartRequest, deps.now(), deps.FS)
	if err != nil {
		return nil, true
	}
	return request, stale
}
