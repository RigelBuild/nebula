package watchdog

import (
	"reflect"
	"testing"
)

const gib = int64(1) << 30

func smp(time, rss int64, identity string, swap int64) sample {
	if identity == "" {
		identity = "123:456"
	}
	return sample{time: time, rssBytes: rss, identity: identity, swapBytes: swap}
}

func step(prev *watchState, next sample) decision {
	return decideSample(prev, next)
}

func TestRSSThresholdParsingAddsVmRSSAndSwap(t *testing.T) {
	status := "VmRSS:\t9000000 kB\nVmSwap:\t2000000 kB\n"
	rss, err := parseVmRss(status)
	must(t, err)
	swap, err := parseVmSwap(status)
	must(t, err)
	if rss+swap <= rssRestartThresholdBytes {
		t.Fatalf("rss+swap = %d, want > %d", rss+swap, rssRestartThresholdBytes)
	}
	missing, err := parseVmSwap("VmRSS:\t11000000 kB\n")
	must(t, err)
	if missing != 0 {
		t.Fatalf("missing VmSwap = %d, want 0", missing)
	}
}

func TestRestartWaitsTenMinutesAndRetriesAfterThirtyMinutes(t *testing.T) {
	result := step(nil, smp(0, 11*gib, "", 0))
	if result.shouldRequest {
		t.Fatal("requested at t=0")
	}
	result = step(&result.state, smp(rssRestartDelayMS-1, 11*gib, "", 0))
	if result.shouldRequest {
		t.Fatal("requested before 10 minutes")
	}
	result = step(&result.state, smp(rssRestartDelayMS, 11*gib, "", 0))
	if !result.shouldRequest {
		t.Fatal("not requested at 10 minutes")
	}
	last := rssRestartDelayMS
	prev := result.state
	prev.LastRequestAt = &last
	result = step(&prev, smp(rssRestartDelayMS+rssRestartRetryMS-1, 11*gib, "", 0))
	if result.shouldRequest {
		t.Fatal("retried before 30 minutes")
	}
	result = step(&result.state, smp(rssRestartDelayMS+rssRestartRetryMS, 11*gib, "", 0))
	if !result.shouldRequest {
		t.Fatal("not retried at 30 minutes")
	}
}

func TestThresholdDipsAndIdentityChangesResetOverSince(t *testing.T) {
	result := step(nil, smp(0, 11*gib, "", 0))
	result = step(&result.state, smp(5*60_000, 11*gib, "", 0))
	result = step(&result.state, smp(6*60_000, 10*gib, "", 0))
	if result.state.OverSince != nil {
		t.Fatalf("overSince after dip = %d, want unset", *result.state.OverSince)
	}
	result = step(&result.state, smp(7*60_000, 11*gib, "", 0))
	if result.state.OverSince == nil || *result.state.OverSince != 7*60_000 {
		t.Fatalf("overSince = %v, want 420000", result.state.OverSince)
	}
	result = step(&result.state, smp(8*60_000, 11*gib, "124:457", 0))
	if result.state.OverSince == nil || *result.state.OverSince != 8*60_000 {
		t.Fatalf("overSince after identity change = %v, want 480000", result.state.OverSince)
	}
	if result.state.LastRequestAt != nil {
		t.Fatalf("lastRequestAt = %d, want unset", *result.state.LastRequestAt)
	}
}

func TestLegacyStateWithoutRestartFieldsIsValid(t *testing.T) {
	legacy := &watchState{Identity: "123:456", Armed: true, Observations: []observation{}}
	if got := step(legacy, smp(0, gib, "", 0)).state.OverSince; got != nil {
		t.Fatalf("overSince = %d, want unset", *got)
	}
}

func TestCapturesOnceAboveMinPlus15GiBAndRearmsBelowMinPlus075GiB(t *testing.T) {
	result := step(nil, smp(0, 1*gib, "", 0))
	if result.capture {
		t.Fatal("captured at 1 GiB")
	}
	result = step(&result.state, smp(30_000, gibs(2.6), "", 0))
	if !result.capture {
		t.Fatal("no capture at 2.6 GiB")
	}
	result = step(&result.state, smp(60_000, 3*gib, "", 0))
	if result.capture {
		t.Fatal("captured again at 3 GiB")
	}
	result = step(&result.state, smp(90_000, gibs(1.7), "", 0))
	if !result.state.Armed {
		t.Fatal("not re-armed at 1.7 GiB")
	}
	result = step(&result.state, smp(120_000, gibs(2.6), "", 0))
	if !result.capture {
		t.Fatal("no second capture after re-arm")
	}
	if captureThresholdBytes != gibs(1.5) {
		t.Fatalf("capture threshold = %d", captureThresholdBytes)
	}
	if rearmThresholdBytes != gibs(0.75) {
		t.Fatalf("rearm threshold = %d", rearmThresholdBytes)
	}
}

func TestSteadyGrowthCapturesEveryFurther4GiB(t *testing.T) {
	result := step(nil, smp(0, gib/2, "", 0))
	var captures []float64
	for g := 1.0; g <= 12.0; g += 0.5 {
		result = step(&result.state, smp(int64(g*60_000), int64(g*float64(gib)), "", 0))
		if result.capture {
			captures = append(captures, g)
		}
	}
	if want := []float64{2.5, 7, 11.5}; !reflect.DeepEqual(captures, want) {
		t.Fatalf("captures = %v, want %v", captures, want)
	}
	if result.state.Armed {
		t.Fatal("still armed after steady growth")
	}
	if result.state.LastCaptureBytes == nil || *result.state.LastCaptureBytes != int64(11.5*float64(gib)) {
		t.Fatalf("lastCaptureBytes = %v, want 11.5 GiB", result.state.LastCaptureBytes)
	}
	if escalationThresholdBytes != 4*gib {
		t.Fatalf("escalation threshold = %d", escalationThresholdBytes)
	}
}

func TestLeakRearmedByWindowDriftKeepsMarkAndCapturesEachFurther4GiB(t *testing.T) {
	result := step(nil, smp(0, gib/2, "", 0))
	result = step(&result.state, smp(30_000, gibs(2.5), "", 0))
	if !result.capture {
		t.Fatal("fast growth did not capture")
	}
	var captures []float64
	for minute := int64(1); minute <= 30*60; minute++ {
		g := 2.5 + (0.25*float64(minute))/60
		result = step(&result.state, smp(30_000+minute*60_000, int64(g*float64(gib)), "", 0))
		if result.capture {
			captures = append(captures, roundHundredths(g))
		}
	}
	if !result.state.Armed {
		t.Fatal("not re-armed by window drift")
	}
	if want := []float64{6.5}; !reflect.DeepEqual(captures, want) {
		t.Fatalf("captures = %v, want %v", captures, want)
	}
}

func roundHundredths(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

func TestDisarmedLegacyStateStepsFromFirstSampleAfterUpgrade(t *testing.T) {
	legacy := &watchState{
		Identity:     "123:456",
		Armed:        false,
		Observations: []observation{{Time: 0, RSSBytes: gib}},
	}
	result := step(legacy, smp(30_000, gibs(4.5), "", 0))
	if result.capture {
		t.Fatal("captured on the upgrade sample")
	}
	if result.state.LastCaptureBytes == nil || *result.state.LastCaptureBytes != gibs(4.5) {
		t.Fatalf("lastCaptureBytes = %v, want 4.5 GiB", result.state.LastCaptureBytes)
	}
	result = step(&result.state, smp(60_000, 8*gib, "", 0))
	if result.capture {
		t.Fatal("captured at 8 GiB")
	}
	result = step(&result.state, smp(90_000, gibs(8.6), "", 0))
	if !result.capture {
		t.Fatal("no escalation capture at 8.6 GiB")
	}
}

func TestPidStarttimeIdentityChangeResetsBaselineAndRearms(t *testing.T) {
	previous := step(nil, smp(0, gib, "", 0))
	captured := step(&previous.state, smp(30_000, gibs(2.6), "", 0))
	if captured.state.Armed {
		t.Fatal("armed after capture")
	}
	replacement := step(&captured.state, smp(60_000, 7*gib, "124:457", 0))
	if replacement.capture {
		t.Fatal("captured on identity change")
	}
	if replacement.minimumBytes != 7*gib {
		t.Fatalf("minimum = %d, want 7 GiB", replacement.minimumBytes)
	}
	if !replacement.state.Armed {
		t.Fatal("not re-armed on identity change")
	}
	if replacement.state.LastCaptureBytes != nil {
		t.Fatalf("lastCaptureBytes = %d, want unset", *replacement.state.LastCaptureBytes)
	}
	if len(replacement.state.Observations) != 1 {
		t.Fatalf("observations = %d, want 1", len(replacement.state.Observations))
	}
}

func TestDropsObservationsOlderThanRollingWindow(t *testing.T) {
	result := step(nil, smp(0, gib, "", 0))
	result = step(&result.state, smp(30_000, gibs(1.2), "", 0))
	result = step(&result.state, smp(3_630_001, 4*gib, "", 0))
	if result.minimumBytes != 4*gib {
		t.Fatalf("minimum = %d, want 4 GiB", result.minimumBytes)
	}
	if len(result.state.Observations) != 1 {
		t.Fatalf("observations = %d, want 1", len(result.state.Observations))
	}
}

func TestSwapCountsTowardThreshold(t *testing.T) {
	first := step(nil, smp(0, 9*gib, "123:456", 2*gib))
	if first.state.OverSince == nil || *first.state.OverSince != 0 {
		t.Fatalf("overSince = %v, want 0", first.state.OverSince)
	}
	due := step(&first.state, smp(rssRestartDelayMS, 9*gib, "123:456", 2*gib))
	if !due.shouldRequest {
		t.Fatal("swap did not count toward the restart threshold")
	}
}

func TestNewServerIdentityDropsPreviousRequestTime(t *testing.T) {
	high := step(nil, smp(0, 11*gib, "", 0))
	requested := high.state
	zero := int64(0)
	requested.LastRequestAt = &zero
	other := step(&requested, smp(sampleIntervalMS, 11*gib, "999:1", 0))
	if other.state.LastRequestAt != nil {
		t.Fatalf("lastRequestAt = %d, want unset", *other.state.LastRequestAt)
	}
	due := step(&other.state, smp(sampleIntervalMS+rssRestartDelayMS, 11*gib, "999:1", 0))
	if !due.shouldRequest {
		t.Fatal("new identity not due after the restart delay")
	}
}

func TestParseWatchStateRejectsInvalidShapes(t *testing.T) {
	valid := `{"identity":"123:456","armed":true,"observations":[{"time":1,"rssBytes":2}]}`
	if _, ok := parseWatchState([]byte(valid)); !ok {
		t.Fatal("valid state rejected")
	}
	cases := map[string]string{
		"missing identity":     `{"armed":true,"observations":[]}`,
		"non-bool armed":       `{"identity":"1:2","armed":"yes","observations":[]}`,
		"missing observations": `{"identity":"1:2","armed":true}`,
		"null overSince":       `{"identity":"1:2","armed":true,"observations":[],"overSince":null}`,
		"null lastRequestAt":   `{"identity":"1:2","armed":true,"observations":[],"lastRequestAt":null}`,
		"null observation":     `{"identity":"1:2","armed":true,"observations":[{"time":1,"rssBytes":null}]}`,
		"string overSince":     `{"identity":"1:2","armed":true,"observations":[],"overSince":"1"}`,
	}
	for name, text := range cases {
		if _, ok := parseWatchState([]byte(text)); ok {
			t.Errorf("%s: accepted", name)
		}
	}
}

// gibs converts a fractional GiB count to bytes, exact for the binary fractions used here.
func gibs(v float64) int64 {
	return int64(v * float64(gib))
}
