package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/exec" //nolint:depguard // Native macOS statistics commands run directly with argv, without a shell.
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	webhookURL               = "http://127.0.0.1:8644/webhooks/alerts"
	webhookTimeout           = 10 * time.Second
	alertThreshold           = 5
	retention                = 7 * 24 * time.Hour
	processLimit             = 15
	alertSummaryProcessLimit = 3
	alertName                = "MemoryPressure"
	stateFileName            = "mem-sampler-state.json"
	secretFileEnv            = "MEMSAMPLER_WEBHOOK_SECRET_FILE" //nolint:gosec // This is an environment variable name, not a credential.
	samplerDirEnv            = "MEMSAMPLER_DIR"
	hostLabelEnv             = "MEMSAMPLER_HOST_LABEL"
	areaLabelEnv             = "MEMSAMPLER_AREA_LABEL"
	alertURLEnv              = "MEMSAMPLER_ALERT_URL"
	ownerHintEnv             = "MEMSAMPLER_OWNER_HINT"
)

var (
	swapPattern = regexp.MustCompile(`total\s*=\s*([0-9.]+)([KMGTP]?)\s+used\s*=\s*([0-9.]+)([KMGTP]?)`)
	freePattern = regexp.MustCompile(`System-wide memory free percentage:\s*([0-9]+)%`)
	psPattern   = regexp.MustCompile(`^\s*(\d+)\s+(\d+)\s+(.+?)\s*$`)
)

type process struct {
	PID   int    `json:"pid"`
	RSSKB int64  `json:"rss_kb"`
	Comm  string `json:"comm"`
}

type swapUsage struct {
	UsedMB  float64
	TotalMB float64
}

type sample struct {
	Timestamp     time.Time `json:"ts"`
	PressureLevel int       `json:"pressure_level"`
	FreePct       int       `json:"free_pct"`
	SwapUsedMB    float64   `json:"swap_used_mb"`
	SwapTotalMB   float64   `json:"swap_total_mb"`
	Top           []process `json:"top"`
}

type alertState struct {
	HighSamples      int   `json:"high_samples"`
	Firing           bool  `json:"firing"`
	EpisodeStartUnix int64 `json:"episode_start_unix,omitempty"`
}

type alertSender struct {
	endpoint string
	client   *http.Client
	host     string
	area     string
	owner    string
	alertURL string
}

type Envelope struct {
	Source     string            `json:"source"`
	Status     string            `json:"status"`
	Severity   string            `json:"severity"`
	Page       bool              `json:"page"`
	Title      string            `json:"title"`
	Summary    string            `json:"summary"`
	URL        string            `json:"url"`
	Labels     map[string]string `json:"labels"`
	OwnerHint  string            `json:"owner_hint"`
	DedupeKey  string            `json:"dedupe_key"`
	ReceivedAt string            `json:"received_at"`
}

func parseSwapUsage(output string) (swapUsage, error) {
	match := swapPattern.FindStringSubmatch(output)
	if len(match) != 5 {
		return swapUsage{}, fmt.Errorf("parse vm.swapusage output %q: expected total and used values", output)
	}
	total, err := parseMemoryMB(match[1], match[2])
	if err != nil {
		return swapUsage{}, fmt.Errorf("parse swap total: %w", err)
	}
	used, err := parseMemoryMB(match[3], match[4])
	if err != nil {
		return swapUsage{}, fmt.Errorf("parse swap used: %w", err)
	}
	return swapUsage{UsedMB: used, TotalMB: total}, nil
}

func parseMemoryMB(value, unit string) (float64, error) {
	amount, err := strconv.ParseFloat(value, 64)
	if err != nil || amount < 0 || math.IsNaN(amount) || math.IsInf(amount, 0) {
		return 0, fmt.Errorf("invalid memory value %q", value)
	}
	power := 0
	if unit != "" {
		power = strings.Index("KMGTPE", unit) + 1
		if power == 0 {
			return 0, fmt.Errorf("unsupported memory unit %q", unit)
		}
	}
	return amount * math.Pow(1024, float64(power-2)), nil
}

func parseFreePct(output string) (int, error) {
	match := freePattern.FindStringSubmatch(output)
	if len(match) != 2 {
		return 0, fmt.Errorf("parse memory_pressure output %q: expected free percentage", output)
	}
	value, err := strconv.Atoi(match[1])
	if err != nil || value < 0 || value > 100 {
		return 0, fmt.Errorf("invalid free percentage %q", match[1])
	}
	return value, nil
}

func parseProcesses(output string, limit int) ([]process, error) {
	processes := make([]process, 0, processLimit)
	for line := range strings.SplitSeq(output, "\n") {
		match := psPattern.FindStringSubmatch(line)
		if len(match) == 0 {
			if strings.TrimSpace(line) == "" {
				continue
			}
			return nil, fmt.Errorf("parse ps row %q", line)
		}
		pid, err := strconv.Atoi(match[1])
		if err != nil || pid < 0 {
			return nil, fmt.Errorf("invalid process pid %q", match[1])
		}
		rss, err := strconv.ParseInt(match[2], 10, 64)
		if err != nil || rss < 0 {
			return nil, fmt.Errorf("invalid process rss %q", match[2])
		}
		processes = append(processes, process{PID: pid, RSSKB: rss, Comm: match[3]})
	}
	return topProcesses(processes, limit), nil
}

func topProcesses(processes []process, limit int) []process {
	if limit < 0 {
		limit = 0
	}
	sorted := slices.Clone(processes)
	slices.SortStableFunc(sorted, func(left, right process) int {
		switch {
		case left.RSSKB > right.RSSKB:
			return -1
		case left.RSSKB < right.RSSKB:
			return 1
		default:
			return 0
		}
	})
	if len(sorted) > limit {
		sorted = sorted[:limit]
	}
	if sorted == nil {
		return []process{}
	}
	return sorted
}

func advanceState(state alertState, isHigh bool, now time.Time) (alertState, string) {
	if isHigh {
		if state.HighSamples == 0 && !state.Firing {
			state.EpisodeStartUnix = now.Unix()
		}
		if state.HighSamples < alertThreshold {
			state.HighSamples++
		}
		if state.HighSamples >= alertThreshold && !state.Firing {
			state.Firing = true
			return state, "firing"
		}
		return state, ""
	}
	state.HighSamples = 0
	if state.Firing {
		state.Firing = false
		return state, "resolved"
	}
	state.EpisodeStartUnix = 0
	return state, ""
}

func transitionDedupeKey(status string, episodeStartUnix int64) string {
	return fmt.Sprintf("memory-pressure:%s:%d", status, episodeStartUnix)
}

func createEnvelope(status string, current sample, episodeStartUnix int64, sender alertSender) Envelope {
	processes := current.Top
	if len(processes) > alertSummaryProcessLimit {
		processes = processes[:alertSummaryProcessLimit]
	}
	parts := make([]string, 0, len(processes))
	for _, item := range processes {
		parts = append(parts, fmt.Sprintf("%s (%d MB)", item.Comm, item.RSSKB/1024))
	}
	if len(parts) == 0 {
		parts = append(parts, "no process data")
	}
	return Envelope{
		Source:     "grafana",
		Status:     status,
		Severity:   "warning",
		Page:       false,
		Title:      alertName,
		Summary:    "memory pressure: top processes by RSS: " + strings.Join(parts, ", "),
		URL:        sender.alertURL,
		Labels:     map[string]string{"alertname": alertName, "host": sender.host, "area": sender.area, "severity": "warning"},
		OwnerHint:  sender.owner,
		DedupeKey:  transitionDedupeKey(status, episodeStartUnix),
		ReceivedAt: current.Timestamp.UTC().Format(time.RFC3339Nano),
	}
}

func signHeaders(secret, id, body string, now time.Time) (map[string]string, error) {
	keyText := strings.TrimPrefix(secret, "whsec_")
	key, err := base64.StdEncoding.DecodeString(keyText)
	if err != nil {
		return nil, fmt.Errorf("decode webhook secret: %w", err)
	}
	timestamp := strconv.FormatInt(now.Unix(), 10)
	mac := hmac.New(sha256.New, key)
	if _, err := io.WriteString(mac, id+"."+timestamp+"."+body); err != nil {
		return nil, fmt.Errorf("sign webhook body: %w", err)
	}
	return map[string]string{
		"content-type":      "application/json",
		"webhook-id":        id,
		"webhook-timestamp": timestamp,
		"webhook-signature": "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil)),
	}, nil
}

func appendSample(directory string, current sample) error {
	if err := os.MkdirAll(directory, 0o700); err != nil { //nolint:gosec // The operator configures the sampler's private data directory.
		return fmt.Errorf("create sample directory: %w", err)
	}
	path := filepath.Join(directory, "mem-"+current.Timestamp.Format("2006-01-02")+".jsonl")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // The file is a dated log inside the configured sampler directory.
	if err != nil {
		return fmt.Errorf("open sample log: %w", err)
	}
	encodeErr := json.NewEncoder(file).Encode(current)
	closeErr := file.Close()
	if encodeErr != nil {
		return fmt.Errorf("append sample: %w", encodeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close sample log: %w", closeErr)
	}
	return nil
}

func pruneLogs(directory string, now time.Time) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read sample directory: %w", err)
	}
	cutoff := now.Add(-retention)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "mem-") || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("stat sample log %q: %w", path, err)
		}
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(path); err != nil { //nolint:gosec // The file is a dated log found inside the configured sampler directory.
				return fmt.Errorf("remove expired sample log %q: %w", path, err)
			}
		}
	}
	return nil
}

func readState(path string) (alertState, error) {
	data, err := os.ReadFile(path) //nolint:gosec // The caller constructs this fixed state filename under the configured sampler directory.
	if errors.Is(err, os.ErrNotExist) {
		return alertState{}, nil
	}
	if err != nil {
		return alertState{}, fmt.Errorf("read sampler state: %w", err)
	}
	var state alertState
	if err := json.Unmarshal(data, &state); err != nil {
		return alertState{}, fmt.Errorf("decode sampler state: %w", err)
	}
	return state, nil
}

func writeState(path string, state alertState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode sampler state: %w", err)
	}
	temp := path + ".tmp"
	if err := os.WriteFile(temp, data, 0o600); err != nil { //nolint:gosec // The temp file is the fixed state filename with a suffix under the configured sampler directory.
		return fmt.Errorf("write sampler state: %w", err)
	}
	if err := os.Rename(temp, path); err != nil { //nolint:gosec // Both paths are the fixed sampler state filename and its temp sibling in the configured directory.
		return fmt.Errorf("replace sampler state: %w", err)
	}
	return nil
}

func capture(now time.Time) (sample, error) {
	pressure, err := commandOutput("sysctl", "-n", "kern.memorystatus_vm_pressure_level")
	if err != nil {
		return sample{}, fmt.Errorf("read memory pressure level: %w", err)
	}
	pressureLevel, err := strconv.Atoi(strings.TrimSpace(pressure))
	if err != nil {
		return sample{}, fmt.Errorf("parse memory pressure level: %w", err)
	}
	swapOutput, err := commandOutput("sysctl", "-n", "vm.swapusage")
	if err != nil {
		return sample{}, fmt.Errorf("read swap usage: %w", err)
	}
	swap, err := parseSwapUsage(swapOutput)
	if err != nil {
		return sample{}, err
	}
	freeOutput, err := commandOutput("memory_pressure", "-Q")
	if err != nil {
		return sample{}, fmt.Errorf("read free memory percentage: %w", err)
	}
	freePct, err := parseFreePct(freeOutput)
	if err != nil {
		return sample{}, err
	}
	psOutput, err := commandOutput("ps", "-axm", "-o", "pid=,rss=,comm=")
	if err != nil {
		return sample{}, fmt.Errorf("read process list: %w", err)
	}
	processes, err := parseProcesses(psOutput, processLimit)
	if err != nil {
		return sample{}, err
	}
	return sample{
		Timestamp:     now.UTC(),
		PressureLevel: pressureLevel,
		FreePct:       freePct,
		SwapUsedMB:    swap.UsedMB,
		SwapTotalMB:   swap.TotalMB,
		Top:           processes,
	}, nil
}

func commandOutput(name string, args ...string) (string, error) {
	output, err := exec.Command(name, args...).Output() //nolint:gosec // All command names and arguments are fixed at the capture call sites; no shell is used.
	if err != nil {
		return "", fmt.Errorf("run %s: %w", name, err)
	}
	return string(output), nil
}

func postAlert(envelope Envelope, secret string, now time.Time, sender alertSender) error {
	body, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("encode alert envelope: %w", err)
	}
	id := fmt.Sprintf("%s-%d", alertName, now.UnixNano())
	headers, err := signHeaders(secret, id, string(body), now)
	if err != nil {
		return err
	}
	request, err := http.NewRequest(http.MethodPost, sender.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create webhook request: %w", err)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := sender.client.Do(request)
	if err != nil {
		return fmt.Errorf("send webhook alert: %w", err)
	}
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		closeErr := response.Body.Close()
		return errors.Join(fmt.Errorf("read webhook response: %w", err), closeErr)
	}
	if err := response.Body.Close(); err != nil {
		return fmt.Errorf("close webhook response: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("webhook returned HTTP %d", response.StatusCode)
	}
	return nil
}

// macOS keeps swap allocated when memory is not tight, so swap is recorded but never triggers.
func memoryPressureHigh(current sample) bool {
	return current.PressureLevel >= 2
}

func processSample(directory string, current sample, secretPath string, sender alertSender) error {
	if err := appendSample(directory, current); err != nil {
		return err
	}
	if err := pruneLogs(directory, current.Timestamp); err != nil {
		return err
	}
	statePath := filepath.Join(directory, stateFileName)
	state, err := readState(statePath)
	if err != nil {
		return err
	}

	next, transition := advanceState(state, memoryPressureHigh(current), current.Timestamp)
	if transition == "" {
		return writeState(statePath, next)
	}
	if secretPath == "" {
		slog.Warn("memory alert transition not sent: webhook secret file path is not configured")
		return writeState(statePath, state)
	}
	secret, err := os.ReadFile(secretPath) //nolint:gosec // The operator configures the secret file path.
	if err != nil {
		slog.Error("memory alert transition not sent: webhook secret file unavailable", "err", err)
		return writeState(statePath, state)
	}
	episodeStartUnix := next.EpisodeStartUnix
	if transition == "resolved" {
		episodeStartUnix = state.EpisodeStartUnix
	}
	envelope := createEnvelope(transition, current, episodeStartUnix, sender)
	if err := postAlert(envelope, strings.TrimSpace(string(secret)), current.Timestamp, sender); err != nil {
		return err
	}
	return writeState(statePath, next)
}

func run() error {
	directory := os.Getenv(samplerDirEnv)
	if directory == "" {
		directory = filepath.Join(os.Getenv("HOME"), "Library", "Logs", "mem-sampler")
	}
	current, err := capture(time.Now().UTC())
	if err != nil {
		return err
	}
	sender, err := senderFromEnv(os.Getenv)
	if err != nil {
		return err
	}
	return processSample(directory, current, os.Getenv(secretFileEnv), sender)
}

func main() {
	if err := run(); err != nil {
		slog.Error("memory sample failed", "err", err)
		os.Exit(1)
	}
}

// senderFromEnv builds the webhook sender from required settings. Host, area and
// URL have no default: they name the operator's fleet, not this tool.
func senderFromEnv(getenv func(string) string) (alertSender, error) {
	host := getenv(hostLabelEnv)
	area := getenv(areaLabelEnv)
	owner := getenv(ownerHintEnv)
	alertURL := getenv(alertURLEnv)
	if host == "" || area == "" || owner == "" || alertURL == "" {
		return alertSender{}, fmt.Errorf("%s, %s, %s and %s are required", hostLabelEnv, areaLabelEnv, ownerHintEnv, alertURLEnv)
	}
	return alertSender{
		endpoint: webhookURL,
		client:   &http.Client{Timeout: webhookTimeout},
		host:     host,
		area:     area,
		owner:    owner,
		alertURL: alertURL,
	}, nil
}
