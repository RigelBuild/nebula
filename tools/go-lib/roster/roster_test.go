package roster

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPath(t *testing.T) {
	tests := []struct {
		name     string
		values   map[string]string
		expected string
	}{
		{
			name:     "explicit roster path",
			values:   map[string]string{"ROSTER_PATH": "/tmp/custom.json", "HOME": "/home/test"},
			expected: "/tmp/custom.json",
		},
		{
			name:     "home fallback",
			values:   map[string]string{"HOME": "/home/test"},
			expected: filepath.Join("/home/test", "agents", "state", "roster.json"),
		},
		{
			name:     "unset home fallback",
			values:   map[string]string{},
			expected: filepath.Join("~", "agents", "state", "roster.json"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Path(func(key string) string { return tt.values[key] })
			if got != tt.expected {
				t.Fatalf("Path() = %q, want %q", got, tt.expected)
			}
		})
	}
	if got := Path(nil); got != filepath.Join("~", "agents", "state", "roster.json") {
		t.Fatalf("Path(nil) = %q, want home fallback", got)
	}
}

func TestDigest(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		content  string
		expected string
	}{
		{
			name:     "UTF-16 key order",
			filename: "utf16.json",
			content:  `{"𐀀":1,"":2}`,
			expected: "4045c21a23c8ae8f8d9add81f54bd506bee65885099876fb4afb378b1f2c3516",
		},
		{
			name:     "JavaScript number formatting",
			filename: "numbers.json",
			content:  `{"numbers":[0.1,1e21,-0,5e-7],"nested":{"z":0.000001,"a":1.2345678901234567}}`,
			expected: "7a7d668a987c66464c91f06edb6380028e8758913ab92ebc8932a34e4cd75f2f",
		},
		{
			name:     "string escaping",
			filename: "strings.json",
			content:  `{"text":"<>&\u2028\u0001\b\n\r\t\u001f"}`,
			expected: "cb01822e71c7b9573a95ad9ab88a43bb093f921d53685b190dc529cf62a41a06",
		},
		{
			name:     "nested objects",
			filename: "nested.json",
			content:  `{"z":{"gamma":[true,null,{"y":"nested","a":3}],"beta":2},"a":[{"q":1,"b":2},"x"]}`,
			expected: "75b48e7c6e9aba8dcea07d67341eb6dc15c7cbf0013d26cb06b8f30a6f0f80c9",
		},
		{
			name:     "lone surrogate escape",
			filename: "surrogate.json",
			content:  `{"text":"\ud800"}`,
			expected: "7d38e2388498cec03881027e7753b07826c5af2d61dd589b4c1caaab14ec2cc4",
		},
		{
			name:     "lone low surrogate escape",
			filename: "low-surrogate.json",
			content:  `{"text":"\udc00"}`,
			expected: "84ceb359a7fdecb7d3538ec2e4a17a62d58078f8fbf828e69a286c5d1fc76e10",
		},
		{
			name:     "UTF-16 supplementary before BMP",
			filename: "utf16-mixed.json",
			content:  `{"Ａ":2,"𐀀":1}`,
			expected: "ca0baa72e33604df3708d8e77c17a80ff20e62f55e9a27ef4e2cf8fad252df0d",
		},
	}

	dir := t.TempDir()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(dir, tt.filename)
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			value, err := readJSON(path)
			if err != nil {
				t.Fatal(err)
			}
			got, err := Digest(value)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.expected {
				t.Fatalf("Digest() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestDigestSupplementaryKeyOrdering(t *testing.T) {
	value, err := decodeJSON([]byte(`{"\ud800\udc01":1,"\ud800\udc00":2}`))
	if err != nil {
		t.Fatal(err)
	}

	const expected = "2a8ead92a43d7b8c161119bbd58bb73aab5113f69e23173b1e5c6feda88e81cc"
	for i := range 50 {
		got, err := Digest(value)
		if err != nil {
			t.Fatal(err)
		}
		if got != expected {
			t.Fatalf("Digest() call %d = %q, want %q", i+1, got, expected)
		}
	}
}
func TestDigestRosterFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/roster.json")
	if err != nil {
		t.Fatal(err)
	}
	value, err := decodeJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Digest(value)
	if err != nil {
		t.Fatal(err)
	}
	const expected = "b5a40b51d41cbc063f105979efd2a76dff8a9f53d8aed5e92b56043ae02576db"
	if got != expected {
		t.Fatalf("Digest(fixture) = %q, want %q", got, expected)
	}
}

func TestDigestRejectsUnsupportedValues(t *testing.T) {
	if _, err := Digest(make(chan int)); err == nil {
		t.Fatal("Digest(channel) succeeded")
	}
}

func TestDigestFileSerializesAgentFields(t *testing.T) {
	file := File{
		SchemaVersion:  "roster-v1",
		SourceRevision: "rev",
		GeneratedAt:    "2026-11-03T12:00:00Z",
		Provenance:     "test",
		Agents: []Agent{{
			Name: "alpha",
			Raw:  map[string]any{"name": "alpha", "unknown": 1.0},
		}},
	}
	if _, err := Digest(file); err != nil {
		t.Fatalf("Digest(File) error = %v", err)
	}
}

func TestDigestDropsOnlyTopLevelDigest(t *testing.T) {
	base := map[string]any{"value": "same"}
	withDigest := map[string]any{"digest": "ignored", "value": "same"}
	baseDigest, err := Digest(base)
	if err != nil {
		t.Fatal(err)
	}
	withDigestValue, err := Digest(withDigest)
	if err != nil {
		t.Fatal(err)
	}
	if baseDigest != withDigestValue {
		t.Fatalf("top-level digest changed result: %q != %q", baseDigest, withDigestValue)
	}

	nestedDigest, err := Digest(map[string]any{"child": map[string]any{"digest": "included"}})
	if err != nil {
		t.Fatal(err)
	}
	withoutNested, err := Digest(map[string]any{"child": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if nestedDigest == withoutNested {
		t.Fatal("nested digest was omitted")
	}
}

func TestDigestRejectsNonFiniteNumbers(t *testing.T) {
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := Digest(value); err == nil || !strings.Contains(err.Error(), "non-finite number") {
			t.Errorf("Digest(%v) error = %v, want non-finite number", value, err)
		}
	}
}

func TestLoad(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		content   string
		wantError string
	}{
		{
			name:    "valid roster and unknown agent fields",
			content: validRosterJSON(now.Format(time.RFC3339Nano), `,"extra":{"enabled":true}`),
		},
		{
			name:      "invalid top-level schema",
			content:   `{"agents":[]}`,
			wantError: "invalid roster schema",
		},
		{
			name:      "invalid agent type",
			content:   `{"schemaVersion":"roster-v1","sourceRevision":"rev","generatedAt":"` + now.Format(time.RFC3339Nano) + `","provenance":"test","agents":[null]}`,
			wantError: "invalid roster agent",
		},
		{
			name:      "invalid agent name",
			content:   `{"schemaVersion":"roster-v1","sourceRevision":"rev","generatedAt":"` + now.Format(time.RFC3339Nano) + `","provenance":"test","agents":[{"name":"Bad"}]}`,
			wantError: "invalid roster agent",
		},
		{
			name:      "invalid JSON",
			content:   `{`,
			wantError: "parse roster JSON",
		},
		{
			name:      "unparseable generated time",
			content:   validRosterJSON("not-a-date", ""),
			wantError: "stale roster",
		},
		{
			name:      "digest mismatch",
			content:   validRosterWithDigest(now.Format(time.RFC3339Nano), "0000000000000000000000000000000000000000000000000000000000000000"),
			wantError: "roster digest mismatch",
		},
		{
			name:      "older than maximum age",
			content:   validRosterJSON(now.Add(-MaxAge-time.Millisecond).Format(time.RFC3339Nano), ""),
			wantError: "stale roster",
		},
		{
			name:      "too far in future",
			content:   validRosterJSON(now.Add(30*time.Second+time.Millisecond).Format(time.RFC3339Nano), ""),
			wantError: "stale roster",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeRoster(t, tt.content)
			snapshot, err := Load(t.Context(), path, now)
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("Load() error = %v, want %q", err, tt.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.Path != path || snapshot.Roster.SchemaVersion != "roster-v1" {
				t.Fatalf("Load() snapshot has wrong path or schema version: %+v", snapshot)
			}
			if len(snapshot.Roster.Agents) != 1 || snapshot.Roster.Agents[0].Name != "alpha-1" {
				t.Fatalf("Load() agents = %+v, want alpha-1", snapshot.Roster.Agents)
			}
			if _, ok := snapshot.Roster.Agents[0].Raw["extra"]; !ok {
				t.Fatalf("unknown agent field was lost: %+v", snapshot.Roster.Agents[0].Raw)
			}
			if got, want := snapshot.Names(), []string{"alpha-1"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("Names() = %v, want %v", got, want)
			}
		})
	}
}

func TestLoadAcceptsValidDigest(t *testing.T) {
	data, err := os.ReadFile("testdata/roster.json")
	if err != nil {
		t.Fatal(err)
	}
	generatedAt := time.Date(2026, 9, 23, 18, 40, 24, 875_000_000, time.UTC)
	content := strings.TrimSuffix(strings.TrimSpace(string(data)), "}") + `,"digest":"b5a40b51d41cbc063f105979efd2a76dff8a9f53d8aed5e92b56043ae02576db"}`
	path := writeRoster(t, content)
	snapshot, err := Load(t.Context(), path, generatedAt)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if snapshot.Roster.Digest != snapshot.Digest || snapshot.Digest != "b5a40b51d41cbc063f105979efd2a76dff8a9f53d8aed5e92b56043ae02576db" {
		t.Fatalf("Load() digest = %q, roster digest = %q", snapshot.Digest, snapshot.Roster.Digest)
	}
}

func validRosterJSON(generatedAt, agentFields string) string {
	return `{"schemaVersion":"roster-v1","sourceRevision":"rev","generatedAt":"` + generatedAt + `","provenance":"test","agents":[{"name":"alpha-1"` + agentFields + `}]}`
}

func TestLoadRejectsNanosecondBeyondAge(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 1, time.UTC)
	generatedAt := now.Add(-MaxAge - time.Nanosecond).Format(time.RFC3339Nano)
	path := writeRoster(t, validRosterJSON(generatedAt, ""))
	if _, err := Load(t.Context(), path, now); err == nil || err.Error() != "stale roster" {
		t.Fatalf("Load() error = %v, want stale roster", err)
	}
}

func validRosterWithDigest(generatedAt, digest string) string {
	return `{"schemaVersion":"roster-v1","sourceRevision":"rev","generatedAt":"` + generatedAt + `","provenance":"test","agents":[{"name":"alpha-1"}],"digest":"` + digest + `"}`
}

func TestLoadFreshnessBoundaries(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name      string
		generated time.Time
	}{
		{name: "maximum age boundary", generated: now.Add(-MaxAge)},
		{name: "future boundary", generated: now.Add(30 * time.Second)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := writeRoster(t, validRosterJSON(tt.generated.Format(time.RFC3339Nano), ""))
			if _, err := Load(t.Context(), path, now); err != nil {
				t.Fatalf("Load() error = %v", err)
			}
		})
	}
}

//nolint:gosmopolitan // This test sets time.Local to verify the zoneless parsing location.
func TestLoadGeneratedAtFormats(t *testing.T) {
	local := time.FixedZone("test", 2*60*60)
	previousLocal := time.Local
	time.Local = local
	t.Cleanup(func() { time.Local = previousLocal })

	tests := []struct {
		name        string
		generatedAt string
		wantTime    time.Time
		wantError   bool
	}{
		{name: "date only", generatedAt: "2026-01-01", wantTime: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		{name: "year and month", generatedAt: "2026-01", wantTime: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		{name: "year only", generatedAt: "2026", wantTime: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		{name: "zoneless minute", generatedAt: "2026-01-01T00:00", wantTime: time.Date(2026, 1, 1, 0, 0, 0, 0, local)},
		{name: "zoneless seconds", generatedAt: "2026-01-01T00:00:00", wantTime: time.Date(2026, 1, 1, 0, 0, 0, 0, local)},
		{name: "zoneless milliseconds", generatedAt: "2026-01-01T00:00:00.123", wantTime: time.Date(2026, 1, 1, 0, 0, 0, 123_000_000, local)},
		{name: "zoned Z", generatedAt: "2026-01-01T00:00:00Z", wantTime: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		{name: "zoned offset", generatedAt: "2026-01-01T02:00:00+02:00", wantTime: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		{name: "garbage", generatedAt: "not-a-date", wantError: true},
		{name: "invalid date", generatedAt: "2026-02-30", wantError: true},
		{name: "fraction too precise", generatedAt: "2026-01-01T00:00:00.0001Z", wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseGeneratedAt(tt.generatedAt)
			if tt.wantError {
				if err == nil {
					t.Fatalf("parseGeneratedAt(%q) unexpectedly succeeded", tt.generatedAt)
				}
				path := writeRoster(t, validRosterJSON(tt.generatedAt, ""))
				if _, err := Load(t.Context(), path, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)); err == nil {
					t.Fatalf("Load() accepted generatedAt %q", tt.generatedAt)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseGeneratedAt(%q) error = %v", tt.generatedAt, err)
			}
			if !got.Equal(tt.wantTime) {
				t.Fatalf("parseGeneratedAt(%q) = %v, want %v", tt.generatedAt, got, tt.wantTime)
			}
			path := writeRoster(t, validRosterJSON(tt.generatedAt, ""))
			if _, err := Load(t.Context(), path, got); err != nil {
				t.Fatalf("Load() rejected generatedAt %q: %v", tt.generatedAt, err)
			}
		})
	}
}

func TestLoadAcceptsFractionalOffsetTime(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	path := writeRoster(t, validRosterJSON("2026-11-03T06:00:00.125-06:00", ""))
	if _, err := Load(t.Context(), path, now); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
}

func writeRoster(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "roster.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadContextAndReadErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Load(ctx, filepath.Join(t.TempDir(), "missing.json"), time.Now()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Load(canceled context) error = %v, want context canceled", err)
	}
	if _, err := Load(t.Context(), filepath.Join(t.TempDir(), "missing.json"), time.Now()); err == nil {
		t.Fatal("Load(missing file) succeeded")
	}
}

func readJSON(path string) (any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return decodeJSON(data)
}
