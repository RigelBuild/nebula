package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseSwapUsage(t *testing.T) {
	got, err := parseSwapUsage("total = 2048.00M  used = 493.44M  free = 1554.56M  (encrypted)")
	if err != nil {
		t.Fatal(err)
	}
	if got.TotalMB != 2048 || got.UsedMB != 493.44 {
		t.Fatalf("swap usage = %+v, want total 2048 MB and used 493.44 MB", got)
	}
}

func TestParseFreePct(t *testing.T) {
	tests := []struct {
		name string
		text string
		want int
	}{
		{name: "sample output", text: "System-wide memory free percentage: 24%\n", want: 24},
		{name: "zero", text: "System-wide memory free percentage: 0%", want: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseFreePct(test.text)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("free percent = %d, want %d", got, test.want)
			}
		})
	}
}

func TestParseProcesses(t *testing.T) {
	input := []process{
		{PID: 10, RSSKB: 1000, Comm: "/Applications/Big App"},
		{PID: 11, RSSKB: 4000, Comm: "/usr/bin/worker"},
		{PID: 12, RSSKB: 2000, Comm: "/usr/bin/other"},
	}
	got := topProcesses(input, 2)
	want := []process{
		{PID: 11, RSSKB: 4000, Comm: "/usr/bin/worker"},
		{PID: 12, RSSKB: 2000, Comm: "/usr/bin/other"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("top processes = %#v, want %#v", got, want)
	}
	if input[0].PID != 10 {
		t.Fatalf("topProcesses mutated its input: %#v", input)
	}
	parsed, err := parseProcesses("  10  1000 /Applications/Big App\n  11  4000 /usr/bin/worker\n  12  2000 /usr/bin/other\n", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed, want) {
		t.Fatalf("parsed processes = %#v, want %#v", parsed, want)
	}
}

func TestMemoryPressureHigh(t *testing.T) {
	tests := []struct {
		name     string
		sample   sample
		wantHigh bool
	}{
		{
			name:   "healthy live-like sample",
			sample: sample{PressureLevel: 1, SwapUsedMB: 493.44, SwapTotalMB: 2048},
		},
		{
			name:     "warning memory pressure",
			sample:   sample{PressureLevel: 2, SwapUsedMB: 493.44, SwapTotalMB: 2048},
			wantHigh: true,
		},
		{
			name:   "high swap with normal pressure",
			sample: sample{PressureLevel: 1, SwapUsedMB: 1800, SwapTotalMB: 2000},
		},
		{
			name:   "swap total unavailable",
			sample: sample{PressureLevel: 1, SwapUsedMB: 1201},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := memoryPressureHigh(test.sample); got != test.wantHigh {
				t.Fatalf("memoryPressureHigh(%+v) = %t, want %t", test.sample, got, test.wantHigh)
			}
		})
	}
}

func TestAdvanceState(t *testing.T) {
	t.Run("five high samples fire once and low resolves", func(t *testing.T) {
		started := time.Unix(1_800_000_000, 0).UTC()
		state := alertState{}
		for index := range 4 {
			var event string
			state, event = advanceState(state, true, started.Add(time.Duration(index)*time.Minute))
			if event != "" {
				t.Fatalf("sample %d event = %q, want none", index+1, event)
			}
		}
		var event string
		state, event = advanceState(state, true, started.Add(4*time.Minute))
		if event != "firing" || !state.Firing || state.EpisodeStartUnix != started.Unix() {
			t.Fatalf("fifth high sample = state %#v, event %q", state, event)
		}
		state, event = advanceState(state, true, started.Add(5*time.Minute))
		if event != "" || !state.Firing || state.EpisodeStartUnix != started.Unix() {
			t.Fatalf("sixth high sample = state %#v, event %q; want same episode", state, event)
		}
		state, event = advanceState(state, false, started.Add(6*time.Minute))
		if event != "resolved" || state.Firing || state.HighSamples != 0 || state.EpisodeStartUnix != started.Unix() {
			t.Fatalf("low sample = state %#v, event %q; want resolved", state, event)
		}
		state, event = advanceState(state, true, started.Add(7*time.Minute))
		if event != "" || state.EpisodeStartUnix != started.Add(7*time.Minute).Unix() {
			t.Fatalf("new high episode = state %#v, event %q; want new start", state, event)
		}
	})
}

func TestEnvelopeDedupeKeys(t *testing.T) {
	started := time.Unix(1_800_000_000, 0).UTC()
	first := createEnvelope("firing", sample{Timestamp: started}, started.Unix(), testSender(alertSender{}))
	resolved := createEnvelope("resolved", sample{Timestamp: started}, started.Unix(), testSender(alertSender{}))
	second := createEnvelope("firing", sample{Timestamp: started.Add(time.Hour)}, started.Add(time.Hour).Unix(), testSender(alertSender{}))
	retry := createEnvelope("firing", sample{Timestamp: started.Add(time.Minute)}, started.Unix(), testSender(alertSender{}))
	if first.DedupeKey == resolved.DedupeKey || first.DedupeKey == second.DedupeKey {
		t.Fatalf("transition dedupe keys must differ: firing=%q resolved=%q next firing=%q", first.DedupeKey, resolved.DedupeKey, second.DedupeKey)
	}
	if first.DedupeKey != retry.DedupeKey {
		t.Fatalf("retry key = %q, want original firing key %q", retry.DedupeKey, first.DedupeKey)
	}
	for _, char := range first.DedupeKey {
		if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._:-", char) {
			t.Fatalf("dedupe key %q contains unsupported character %q", first.DedupeKey, char)
		}
	}
}

func TestStatePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), stateFileName)
	want := alertState{HighSamples: 4, Firing: true, EpisodeStartUnix: 1_800_000_000}
	if err := writeState(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := readState(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("state = %#v, want %#v", got, want)
	}
}

func TestSignHeaders(t *testing.T) {
	secret := "whsec_" + base64.StdEncoding.EncodeToString([]byte("example-secret"))
	body := `{"status":"firing"}`
	now := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	got, err := signHeaders(secret, "alert-id", body, now)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte("example-secret"))
	message := []byte(fmt.Sprintf("alert-id.%d.%s", now.Unix(), body))
	if _, err := mac.Write(message); err != nil {
		t.Fatalf("write signature input: %v", err)
	}
	want := "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if got["webhook-signature"] != want {
		t.Fatalf("signature = %q, want %q", got["webhook-signature"], want)
	}
	if got["webhook-id"] != "alert-id" || got["webhook-timestamp"] != strconv.FormatInt(now.Unix(), 10) {
		t.Fatalf("signature headers = %#v", got)
	}
}

func TestPruneLogs(t *testing.T) {
	directory := t.TempDir()
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	old := filepath.Join(directory, "mem-old.jsonl")
	cutoff := filepath.Join(directory, "mem-cutoff.jsonl")
	recent := filepath.Join(directory, "mem-recent.jsonl")
	unrelated := filepath.Join(directory, "other-old.jsonl")
	for _, path := range []string{old, cutoff, recent, unrelated} {
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for path, modified := range map[string]time.Time{
		old:       now.Add(-retention - time.Second),
		cutoff:    now.Add(-retention),
		recent:    now.Add(-time.Hour),
		unrelated: now.Add(-retention - time.Hour),
	} {
		if err := os.Chtimes(path, modified, modified); err != nil {
			t.Fatal(err)
		}
	}
	if err := pruneLogs(directory, now); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		want bool
	}{
		{name: filepath.Base(old), want: false},
		{name: filepath.Base(cutoff), want: true},
		{name: filepath.Base(recent), want: true},
		{name: filepath.Base(unrelated), want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := os.Stat(filepath.Join(directory, test.name))
			got := err == nil
			if got != test.want {
				t.Fatalf("file exists = %t, want %t (stat err %v)", got, test.want, err)
			}
		})
	}
}

// recordingServer appends each alert's dedupe key to keys and replies with status.
func recordingServer(t *testing.T, keys *[]string, status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got Envelope
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode alert: %v", err)
			return
		}
		*keys = append(*keys, got.DedupeKey)
		w.WriteHeader(status)
	}))
}

func writeTestSecret(t *testing.T) string {
	t.Helper()
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("whsec_"+base64.StdEncoding.EncodeToString([]byte("test-secret"))), 0o600); err != nil {
		t.Fatal(err)
	}
	return secret
}

func TestProcessSamplePressureTrigger(t *testing.T) {
	for _, test := range []struct {
		name       string
		current    sample
		wantAlerts int
	}{
		{name: "high swap with normal pressure", current: sample{PressureLevel: 1, SwapUsedMB: 1800, SwapTotalMB: 2000}},
		{name: "warning pressure", current: sample{PressureLevel: 2}, wantAlerts: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			secret := writeTestSecret(t)
			var keys []string
			server := recordingServer(t, &keys, http.StatusNoContent)
			defer server.Close()
			sender := testSender(alertSender{endpoint: server.URL, client: &http.Client{Timeout: time.Second}})
			started := time.Unix(1_800_000_000, 0).UTC()
			for index := range 5 {
				current := test.current
				current.Timestamp = started.Add(time.Duration(index) * time.Minute)
				if err := processSample(directory, current, secret, sender); err != nil {
					t.Fatal(err)
				}
				if index < 4 && len(keys) != 0 {
					t.Fatalf("sample %d sent alert before threshold", index+1)
				}
			}
			if len(keys) != test.wantAlerts {
				t.Fatalf("alerts = %d, want %d", len(keys), test.wantAlerts)
			}
		})
	}
}

func TestProcessSampleSendFailureDoesNotAdvanceState(t *testing.T) {
	for _, test := range []struct {
		name       string
		closeFirst bool
	}{
		{name: "http 500"},
		{name: "closed server", closeFirst: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			checkSendFailureThenRetry(t, test.closeFirst)
		})
	}
}

func checkSendFailureThenRetry(t *testing.T, closeFirst bool) {
	t.Helper()
	directory := t.TempDir()
	prior := alertState{HighSamples: alertThreshold - 1, EpisodeStartUnix: 1_800_000_000}
	statePath := filepath.Join(directory, stateFileName)
	if err := writeState(statePath, prior); err != nil {
		t.Fatal(err)
	}
	var keys []string
	failingServer := recordingServer(t, &keys, http.StatusInternalServerError)
	endpoint := failingServer.URL
	if closeFirst {
		failingServer.Close()
	}
	defer failingServer.Close()
	secret := writeTestSecret(t)
	current := sample{Timestamp: time.Unix(1_800_000_600, 0).UTC(), PressureLevel: 2}
	sender := testSender(alertSender{endpoint: endpoint, client: &http.Client{Timeout: time.Second}})
	if err := processSample(directory, current, secret, sender); err == nil {
		t.Fatal("processSample error = nil, want send failure")
	}
	gotState, err := readState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if gotState != prior {
		t.Fatalf("state after failed send = %#v, want prior %#v", gotState, prior)
	}

	retryServer := recordingServer(t, &keys, http.StatusNoContent)
	defer retryServer.Close()
	retrySample := current
	retrySample.Timestamp = current.Timestamp.Add(time.Minute)
	sender.endpoint = retryServer.URL
	if err := processSample(directory, retrySample, secret, sender); err != nil {
		t.Fatal(err)
	}

	wantKey := fmt.Sprintf("memory-pressure:firing:%d", prior.EpisodeStartUnix)
	wantKeys := []string{wantKey, wantKey}
	if closeFirst {
		wantKeys = []string{wantKey}
	}
	if !slices.Equal(keys, wantKeys) {
		t.Fatalf("retry dedupe keys = %v, want %v", keys, wantKeys)
	}
	advanced, err := readState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !advanced.Firing || advanced.HighSamples != alertThreshold {
		t.Fatalf("state after successful retry = %#v, want firing", advanced)
	}
}

func TestProcessSampleRecoveryStartsNewEpisode(t *testing.T) {
	directory := t.TempDir()
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("whsec_"+base64.StdEncoding.EncodeToString([]byte("test-secret"))), 0o600); err != nil {
		t.Fatal(err)
	}
	var alerts []Envelope
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got Envelope
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode alert: %v", err)
			return
		}
		alerts = append(alerts, got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	sender := testSender(alertSender{endpoint: server.URL, client: &http.Client{Timeout: time.Second}})
	started := time.Unix(1_800_000_000, 0).UTC()

	for index := range alertThreshold - 1 {
		current := sample{Timestamp: started.Add(time.Duration(index) * time.Minute), PressureLevel: 2}
		if err := processSample(directory, current, secret, sender); err != nil {
			t.Fatal(err)
		}
	}
	recoveredAt := started.Add(time.Duration(alertThreshold-1) * time.Minute)
	if err := processSample(directory, sample{Timestamp: recoveredAt, PressureLevel: 1}, secret, sender); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(directory, stateFileName)
	state, err := readState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if state.HighSamples != 0 || state.Firing || state.EpisodeStartUnix != 0 {
		t.Fatalf("state after recovery = %#v, want reset episode", state)
	}
	if len(alerts) != 0 {
		t.Fatalf("alerts before firing = %#v, want no request including no resolved status", alerts)
	}

	newStarted := recoveredAt.Add(time.Minute)
	for index := range alertThreshold {
		current := sample{Timestamp: newStarted.Add(time.Duration(index) * time.Minute), PressureLevel: 2}
		if err := processSample(directory, current, secret, sender); err != nil {
			t.Fatal(err)
		}
	}
	if len(alerts) != 1 || alerts[0].Status != "firing" {
		t.Fatalf("alerts after second episode = %#v, want one firing alert", alerts)
	}
	wantKey := fmt.Sprintf("memory-pressure:firing:%d", newStarted.Unix())
	if alerts[0].DedupeKey != wantKey || newStarted.Unix() == started.Unix() {
		t.Fatalf("second episode key = %q, want %q", alerts[0].DedupeKey, wantKey)
	}
	state, err = readState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Firing || state.EpisodeStartUnix != newStarted.Unix() {
		t.Fatalf("state after second episode = %#v, want firing from %d", state, newStarted.Unix())
	}
}

func TestProcessSampleTimeoutDoesNotAdvanceState(t *testing.T) {
	directory := t.TempDir()
	prior := alertState{HighSamples: alertThreshold - 1, EpisodeStartUnix: 1_800_000_000}
	statePath := filepath.Join(directory, stateFileName)
	if err := writeState(statePath, prior); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("whsec_"+base64.StdEncoding.EncodeToString([]byte("test-secret"))), 0o600); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release
	}))
	defer server.Close()
	current := sample{Timestamp: time.Unix(1_800_000_600, 0).UTC(), PressureLevel: 2}
	sender := testSender(alertSender{endpoint: server.URL, client: &http.Client{Timeout: 10 * time.Millisecond}})
	if err := processSample(directory, current, secret, sender); err == nil {
		t.Fatal("processSample error = nil, want timeout")
	}
	<-entered
	close(release)
	got, err := readState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if got != prior {
		t.Fatalf("state after timeout = %#v, want prior %#v", got, prior)
	}
}

func TestProcessSampleSuccessfulSendAdvancesState(t *testing.T) {
	directory := t.TempDir()
	prior := alertState{HighSamples: alertThreshold - 1, EpisodeStartUnix: 1_800_000_000}
	statePath := filepath.Join(directory, stateFileName)
	if err := writeState(statePath, prior); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("whsec_"+base64.StdEncoding.EncodeToString([]byte("test-secret"))), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got Envelope
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode alert: %v", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	current := sample{Timestamp: time.Unix(1_800_000_600, 0).UTC(), PressureLevel: 2}
	sender := testSender(alertSender{endpoint: server.URL, client: &http.Client{Timeout: time.Second}})
	if err := processSample(directory, current, secret, sender); err != nil {
		t.Fatal(err)
	}
	got, err := readState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	want := alertState{HighSamples: alertThreshold, Firing: true, EpisodeStartUnix: prior.EpisodeStartUnix}
	if got != want {
		t.Fatalf("state after successful send = %#v, want %#v", got, want)
	}
}

func TestCreateEnvelope(t *testing.T) {
	current := sample{
		Timestamp: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC),
		Top: []process{
			{Comm: "largest", RSSKB: 4096},
			{Comm: "second", RSSKB: 2048},
			{Comm: "third", RSSKB: 1024},
			{Comm: "fourth", RSSKB: 512},
		},
	}
	got := createEnvelope("firing", current, 1_800_000_000, testSender(alertSender{}))
	if got.Source != "grafana" || got.Status != "firing" || got.Severity != "warning" {
		t.Fatalf("envelope fields = %#v", got)
	}
	if got.Labels["alertname"] != alertName || got.Labels["host"] != testHost || got.Labels["area"] != testArea {
		t.Fatalf("envelope labels = %#v", got.Labels)
	}
	if !strings.Contains(got.Summary, "third") || strings.Contains(got.Summary, "fourth") {
		t.Fatalf("summary = %q, want top three only", got.Summary)
	}
	if got.URL != testAlertURL || got.DedupeKey != "memory-pressure:firing:1800000000" {
		t.Fatalf("envelope route = %#v", got)
	}
}

func TestAppendSampleKeepsAllProcesses(t *testing.T) {
	directory := t.TempDir()
	current := sample{Timestamp: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC), Top: make([]process, processLimit)}
	for index := range current.Top {
		current.Top[index] = process{PID: index + 1, Comm: fmt.Sprintf("process-%d", index+1)}
	}
	if err := appendSample(directory, current); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "mem-2026-11-05.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got sample
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Top) != processLimit {
		t.Fatalf("JSONL top processes = %d, want %d", len(got.Top), processLimit)
	}
}

// Example values only: the sender's host, area, owner and URL are set by the operator.
const (
	testHost     = "example-host"
	testArea     = "example-area"
	testOwner    = "example-owner"
	testAlertURL = "https://alerts.example.com/"
)

func testSender(base alertSender) alertSender {
	base.host = testHost
	base.area = testArea
	base.owner = testOwner
	base.alertURL = testAlertURL
	return base
}

func TestSenderFromEnvRequiresEveryOperatorValue(t *testing.T) {
	t.Parallel()
	full := map[string]string{
		hostLabelEnv: testHost,
		areaLabelEnv: testArea,
		ownerHintEnv: testOwner,
		alertURLEnv:  testAlertURL,
	}
	got, err := senderFromEnv(func(name string) string { return full[name] })
	if err != nil || got.host != testHost || got.area != testArea || got.owner != testOwner || got.alertURL != testAlertURL {
		t.Fatalf("senderFromEnv(full) = %#v, %v", got, err)
	}
	for _, missing := range []string{hostLabelEnv, areaLabelEnv, ownerHintEnv, alertURLEnv} {
		t.Run(missing, func(t *testing.T) {
			t.Parallel()
			env := map[string]string{}
			maps.Copy(env, full)
			delete(env, missing)
			if _, err := senderFromEnv(func(name string) string { return env[name] }); err == nil || !strings.Contains(err.Error(), missing) {
				t.Fatalf("senderFromEnv without %s: err = %v, want it named", missing, err)
			}
		})
	}
}
