package main

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
)

const (
	probeTimeoutMS        = 10_000
	probeIntervalMS       = 30_000
	probeCount            = 4
	probeFailures         = 3
	probeGood             = 2
	restartLiveDeadlineMS = 2 * 60_000
)

type ProbeCall struct {
	ExitCode int
	Stdout   string
	Timeout  bool
}

func probeGoodOutput(tabs, panes ProbeCall) bool {
	return jsonArray(tabs) && jsonArray(panes)
}

func jsonArray(call ProbeCall) bool {
	if call.Timeout || call.ExitCode != 0 {
		return false
	}
	var value any
	if err := json.Unmarshal([]byte(call.Stdout), &value); err != nil {
		return false
	}
	_, ok := value.([]any)
	return ok
}

type WedgeVerdict struct {
	Kind     string
	Identity string
}

type WedgeDeps struct {
	Probe    func() (bool, error)
	Identity func(context.Context) string
	Sleep    func(milliseconds int64) error
}

func checkWedge(ctx context.Context, deps WedgeDeps) WedgeVerdict {
	good := 0
	failed := 0
	identities := make(map[string]struct{})
	for attempt := range probeCount {
		if attempt > 0 {
			if err := deps.Sleep(probeIntervalMS); err != nil {
				return WedgeVerdict{Kind: wedgeUnverified}
			}
		}
		ok, err := deps.Probe()
		if err != nil {
			ok = false
		}
		identities[deps.Identity(ctx)] = struct{}{}
		if ok && attempt == 0 {
			return WedgeVerdict{Kind: wedgeHealthy}
		}
		if ok {
			good++
		} else {
			failed++
		}
		if good >= probeGood {
			return WedgeVerdict{Kind: wedgeHealthy}
		}
		if failed >= probeFailures {
			break
		}
	}
	if failed < probeFailures {
		return WedgeVerdict{Kind: wedgeHealthy}
	}
	return wedgeIdentityVerdict(identities)
}

func wedgeIdentityVerdict(identities map[string]struct{}) WedgeVerdict {
	if len(identities) == 1 {
		for identity := range identities {
			if identity == identityNone {
				return WedgeVerdict{Kind: "down"}
			}
		}
	}
	identityPattern := regexp.MustCompile(`^\d+:\d+$`)
	var only string
	for identity := range identities {
		if only == "" {
			only = identity
			continue
		}
		return WedgeVerdict{Kind: wedgeUnverified}
	}
	if !identityPattern.MatchString(only) {
		return WedgeVerdict{Kind: wedgeUnverified}
	}
	return WedgeVerdict{Kind: wedgeWedged, Identity: only}
}

func ownedByUnit(identity, cgroupProcs string) bool {
	pid, _, ok := strings.Cut(identity, ":")
	if !ok || pid == "" {
		return false
	}
	for line := range strings.SplitSeq(cgroupProcs, "\n") {
		if strings.TrimSpace(line) == pid {
			return true
		}
	}
	return false
}
