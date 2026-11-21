package reconcile

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var (
	nodeNamePattern = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$`)
	integerPattern  = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)
)

// ParseDesired returns the desired set from the mounted ConfigMap keys. It fails on bad values,
// overlapping sets, or minNodes below one.
func ParseDesired(files map[string]string) (DesiredSet, error) {
	members, err := parseNames(files["members"], "members")
	if err != nil {
		return DesiredSet{}, err
	}
	retiring, err := parseNames(files["retiring"], "retiring")
	if err != nil {
		return DesiredSet{}, err
	}
	renamed, err := parseNames(files["renamed"], "renamed")
	if err != nil {
		return DesiredSet{}, err
	}
	if name, ok := firstShared(members, retiring); ok {
		return DesiredSet{}, fmt.Errorf("node %s is in both members and retiring", name)
	}
	if name, ok := firstShared(renamed, members); ok {
		return DesiredSet{}, fmt.Errorf("node %s is in both renamed and members", name)
	}
	mode := files["mode"]
	if mode != string(ModeDryRun) && mode != string(ModeEnforce) {
		return DesiredSet{}, errors.New("mode must be dry-run or enforce")
	}
	minNodes, err := parseInteger(files["min-nodes"], "min-nodes", 1)
	if err != nil {
		return DesiredSet{}, err
	}
	grace, err := parseInteger(files["not-ready-grace-seconds"], "not-ready-grace-seconds", 0)
	if err != nil {
		return DesiredSet{}, err
	}
	return DesiredSet{
		Members:              members,
		Retiring:             retiring,
		Renamed:              renamed,
		Mode:                 Mode(mode),
		MinNodes:             minNodes,
		NotReadyGraceSeconds: grace,
	}, nil
}

// parseNames splits on newlines and trims, then checks each name's format before any duplicate.
func parseNames(value, field string) ([]string, error) {
	names := []string{}
	for line := range strings.SplitSeq(strings.ReplaceAll(value, "\r\n", "\n"), "\n") {
		if name := strings.TrimSpace(line); name != "" {
			names = append(names, name)
		}
	}
	for _, name := range names {
		if !nodeNamePattern.MatchString(name) {
			return nil, fmt.Errorf("%s contains an invalid node name", field)
		}
	}
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			return nil, fmt.Errorf("%s contains duplicate node names", field)
		}
		seen[name] = true
	}
	return names, nil
}

func parseInteger(value, field string, minimum int) (int, error) {
	if !integerPattern.MatchString(value) {
		return 0, fmt.Errorf("%s must be a non-negative integer string", field)
	}
	number, err := strconv.Atoi(value)
	if err != nil || number < minimum {
		return 0, fmt.Errorf("%s must be at least %d", field, minimum)
	}
	return number, nil
}

func firstShared(left, right []string) (string, bool) {
	for _, name := range left {
		if slices.Contains(right, name) {
			return name, true
		}
	}
	return "", false
}
