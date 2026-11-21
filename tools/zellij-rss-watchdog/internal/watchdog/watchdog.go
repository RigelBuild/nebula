package watchdog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"

	"github.com/RigelBuild/nebula/tools/zellij-rss-watchdog/internal/proc"
)

// Times are Unix milliseconds and sizes are bytes; the state file depends on both.
const (
	sampleIntervalMS         int64 = 30_000
	windowMS                 int64 = 60 * 60 * 1_000
	captureThresholdBytes    int64 = 3 << 29 // 1.5 GiB
	rearmThresholdBytes      int64 = 3 << 28 // 0.75 GiB
	escalationThresholdBytes int64 = 4 << 30
	rssRestartThresholdBytes int64 = 10 << 30
	rssRestartDelayMS        int64 = 10 * 60_000
	rssRestartRetryMS        int64 = 30 * 60_000
)

type observation struct {
	Time     int64 `json:"time"`
	RSSBytes int64 `json:"rssBytes"`
}

// watchState is the persisted watch-state.json; its field order is part of the file format.
type watchState struct {
	Identity         string        `json:"identity"`
	Armed            bool          `json:"armed"`
	Observations     []observation `json:"observations"`
	OverSince        *int64        `json:"overSince,omitempty"`
	LastRequestAt    *int64        `json:"lastRequestAt,omitempty"`
	LastCaptureBytes *int64        `json:"lastCaptureBytes,omitempty"`
}

type sample struct {
	time      int64
	identity  string
	rssBytes  int64
	swapBytes int64
}

type decision struct {
	state         watchState
	capture       bool
	minimumBytes  int64
	shouldRequest bool
}

// decideSample advances the watch state by one sample; it does no IO.
func decideSample(previous *watchState, s sample) decision {
	changedServer := previous == nil || previous.Identity != s.identity
	observations, minimum := windowObservations(previous, changedServer, s)
	armed := armedAfter(previous, changedServer, minimum, s.rssBytes)
	// The rolling minimum can re-arm a slow leak without a drop, so the last capture RSS
	// persists until the server changes and every further 4 GiB captures again.
	lastCapture := lastCaptureBytes(previous, changedServer, s.rssBytes)
	capture := (armed && s.rssBytes > minimum+captureThresholdBytes) ||
		(lastCapture != nil && s.rssBytes > *lastCapture+escalationThresholdBytes)
	overSince, shouldRequest := restartTiming(previous, changedServer, s)
	var lastRequestAt *int64
	if !changedServer {
		lastRequestAt = previous.LastRequestAt
	}
	state := watchState{
		Identity:      s.identity,
		Armed:         armed && !capture,
		Observations:  observations,
		OverSince:     overSince,
		LastRequestAt: lastRequestAt,
	}
	mark := lastCapture
	if capture {
		rss := s.rssBytes
		mark = &rss
	}
	state.LastCaptureBytes = mark
	return decision{state: state, capture: capture, minimumBytes: minimum, shouldRequest: shouldRequest}
}

// windowObservations keeps the observations within the rolling window and appends this sample.
// A changed server starts a new window.
func windowObservations(previous *watchState, changedServer bool, s sample) ([]observation, int64) {
	var observations []observation
	if !changedServer {
		for _, item := range previous.Observations {
			if item.Time > s.time-windowMS {
				observations = append(observations, item)
			}
		}
	}
	observations = append(observations, observation{Time: s.time, RSSBytes: s.rssBytes})
	minimum := observations[0].RSSBytes
	for _, item := range observations[1:] {
		minimum = min(minimum, item.RSSBytes)
	}
	return observations, minimum
}

// armedAfter re-arms after a capture once RSS falls near the rolling minimum.
func armedAfter(previous *watchState, changedServer bool, minimum, rssBytes int64) bool {
	armed := true
	if !changedServer {
		armed = previous.Armed
	}
	if !armed && rssBytes < minimum+rearmThresholdBytes {
		armed = true
	}
	return armed
}

// lastCaptureBytes carries the last capture RSS across samples of the same server.
func lastCaptureBytes(previous *watchState, changedServer bool, rssBytes int64) *int64 {
	if changedServer {
		return nil
	}
	switch {
	case previous.LastCaptureBytes != nil:
		return previous.LastCaptureBytes
	case !previous.Armed:
		rss := rssBytes
		return &rss
	}
	return nil
}

// restartTiming tracks how long RSS plus swap has been over the restart threshold.
func restartTiming(previous *watchState, changedServer bool, s sample) (overSince *int64, shouldRequest bool) {
	if s.rssBytes+s.swapBytes > rssRestartThresholdBytes {
		if !changedServer && previous.OverSince != nil {
			overSince = previous.OverSince
		} else {
			t := s.time
			overSince = &t
		}
	}
	var lastRequestAt *int64
	if !changedServer {
		lastRequestAt = previous.LastRequestAt
	}
	shouldRequest = overSince != nil &&
		s.time-*overSince >= rssRestartDelayMS &&
		(lastRequestAt == nil || s.time-*lastRequestAt >= rssRestartRetryMS)
	return overSince, shouldRequest
}

var (
	vmSwapPattern = regexp.MustCompile(`(?m)^VmSwap:\s+(\d+)\s+kB\s*$`)
	vmRssPattern  = regexp.MustCompile(`(?m)^VmRSS:\s+(\d+)\s+kB\s*$`)
)

// parseVmSwap returns VmSwap in bytes; a missing field counts as zero.
func parseVmSwap(status string) (int64, error) {
	match := vmSwapPattern.FindStringSubmatch(status)
	if match == nil {
		return 0, nil
	}
	return kibibytes(match[1], "VmSwap")
}

// parseVmRss returns VmRSS in bytes; a missing field is an error.
func parseVmRss(status string) (int64, error) {
	match := vmRssPattern.FindStringSubmatch(status)
	if match == nil {
		return 0, errors.New("VmRSS is missing from proc status")
	}
	return kibibytes(match[1], "VmRSS")
}

func kibibytes(digits, field string) (int64, error) {
	kilobytes, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || kilobytes > proc.MaxSafeInteger {
		return 0, fmt.Errorf("%s is not a safe integer", field)
	}
	return kilobytes * 1024, nil
}

// rawObservation uses pointers so a missing or null field fails validation.
type rawObservation struct {
	Time     *int64 `json:"time"`
	RSSBytes *int64 `json:"rssBytes"`
}

// rawWatchState keeps optional fields as RawMessage: absent is nil, explicit null is "null".
type rawWatchState struct {
	Identity         *string           `json:"identity"`
	Armed            *bool             `json:"armed"`
	Observations     *[]rawObservation `json:"observations"`
	OverSince        json.RawMessage   `json:"overSince"`
	LastRequestAt    json.RawMessage   `json:"lastRequestAt"`
	LastCaptureBytes json.RawMessage   `json:"lastCaptureBytes"`
}

// parseWatchState accepts only a complete, well-typed state; anything else reads as absent.
func parseWatchState(data []byte) (*watchState, bool) {
	var raw rawWatchState
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, false
	}
	if raw.Identity == nil || raw.Armed == nil || raw.Observations == nil {
		return nil, false
	}
	state := watchState{
		Identity:     *raw.Identity,
		Armed:        *raw.Armed,
		Observations: make([]observation, 0, len(*raw.Observations)),
	}
	for _, item := range *raw.Observations {
		if item.Time == nil || item.RSSBytes == nil {
			return nil, false
		}
		state.Observations = append(state.Observations, observation{Time: *item.Time, RSSBytes: *item.RSSBytes})
	}
	var ok bool
	if state.OverSince, ok = optionalInt(raw.OverSince); !ok {
		return nil, false
	}
	if state.LastRequestAt, ok = optionalInt(raw.LastRequestAt); !ok {
		return nil, false
	}
	if state.LastCaptureBytes, ok = optionalInt(raw.LastCaptureBytes); !ok {
		return nil, false
	}
	return &state, true
}

// optionalInt returns nil for an absent field; null or a non-number is invalid.
func optionalInt(raw json.RawMessage) (*int64, bool) {
	if raw == nil {
		return nil, true
	}
	if bytes.Equal(raw, []byte("null")) {
		return nil, false
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, false
	}
	return &value, true
}
