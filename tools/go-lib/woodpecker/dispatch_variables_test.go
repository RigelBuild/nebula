package woodpecker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// tsDispatchConstant matches the TS constant that dispatchCdTargets uses:
// `export const CD_DISPATCH_VARIABLES = [ ... ] as const;`.
var tsDispatchConstant = regexp.MustCompile(`(?s)export const CD_DISPATCH_VARIABLES = \[(.*?)\] as const;`)

// tsDispatchElement matches one array element: a double-quoted, non-empty name.
var tsDispatchElement = regexp.MustCompile(`^"[^"]+"$`)

// parseTSDispatchVariables extracts the names from the TS CD_DISPATCH_VARIABLES
// constant in src. It fails when the constant is missing or any array element
// is not a double-quoted, non-empty name, so no element is dropped silently.
func parseTSDispatchVariables(src []byte) ([]string, error) {
	block := tsDispatchConstant.FindSubmatch(src)
	if block == nil {
		return nil, errors.New("CD_DISPATCH_VARIABLES constant not found")
	}
	var names []string
	for _, element := range strings.Split(string(block[1]), ",") {
		element = strings.TrimSpace(element)
		if element == "" {
			continue
		}
		if !tsDispatchElement.MatchString(element) {
			return nil, fmt.Errorf("CD_DISPATCH_VARIABLES element %q is not a quoted name", element)
		}
		names = append(names, element[1:len(element)-1])
	}
	return names, nil
}

// readTSDispatchVariables reads CD_DISPATCH_VARIABLES from the TS sender
// fixture in testdata, so the Go list cannot drift from the constant it exports.
func readTSDispatchVariables(t *testing.T) []string {
	t.Helper()
	source, err := os.ReadFile("testdata/woodpecker-api.ts")
	if err != nil {
		t.Fatalf("read woodpecker-api.ts: %v", err)
	}
	names, err := parseTSDispatchVariables(source)
	if err != nil {
		t.Fatalf("woodpecker-api.ts: %v", err)
	}
	return names
}

// The embedded list, the TS constant, and the keys dispatchCDTargets sends
// must name the same variables in the same order, so the deployments.yaml
// allowlist check stays honest.
func TestCDDispatchVariablesMatchTSAndSentKeys(t *testing.T) {
	names, err := CDDispatchVariables()
	if err != nil {
		t.Fatalf("CDDispatchVariables: %v", err)
	}

	tsNames := readTSDispatchVariables(t)
	if !slices.Equal(names, tsNames) {
		t.Fatalf("CDDispatchVariables = %v, TS CD_DISPATCH_VARIABLES = %v", names, tsNames)
	}

	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	if _, err := DispatchCDTargets(context.Background(),
		Context{BaseURL: server.URL, Token: "token", RepoID: 1},
		[]string{"first"}, testDispatchID, "a1b2c3d4", 0); err != nil {
		t.Fatalf("DispatchCDTargets: %v", err)
	}
	variables, _ := body["variables"].(map[string]any)
	sent := make([]string, 0, len(variables))
	for key := range variables {
		sent = append(sent, key)
	}
	slices.Sort(sent)
	want := slices.Clone(names)
	slices.Sort(want)
	if !slices.Equal(sent, want) {
		t.Fatalf("dispatch sends %v, CDDispatchVariables = %v", sent, want)
	}
	if got := variables["CD_CHECK_BASE"]; got != "a1b2c3d4" {
		t.Fatalf("CD_CHECK_BASE = %v, want the base passed to DispatchCDTargets", got)
	}
}

// checkDispatchKeys must reject a variable set that drifts from the list, so
// a dispatch with a wrong key fails before any pipeline starts.
func TestCheckDispatchKeysRejectsDrift(t *testing.T) {
	names, err := CDDispatchVariables()
	if err != nil {
		t.Fatalf("CDDispatchVariables: %v", err)
	}
	matching := make(map[string]string, len(names))
	for _, name := range names {
		matching[name] = "x"
	}
	if err := checkDispatchKeys(matching); err != nil {
		t.Fatalf("matching keys rejected: %v", err)
	}
	extra := map[string]string{"UNLISTED_VARIABLE": "x"}
	for _, name := range names {
		extra[name] = "x"
	}
	if err := checkDispatchKeys(extra); err == nil {
		t.Fatal("extra key accepted, want error")
	}
	if err := checkDispatchKeys(map[string]string{names[0]: "x"}); err == nil {
		t.Fatal("missing key accepted, want error")
	}
}

// parseTSDispatchVariables must fail on every malformed source, so a bad
// constant cannot shrink or reshape the list the Go side checks against.
func TestParseTSDispatchVariablesRejectsBadSources(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{"constant missing", `export const OTHER = ["PETREL_CD_TARGETS"] as const;`},
		{"unquoted element", `export const CD_DISPATCH_VARIABLES = [PETREL_CD_TARGETS] as const;`},
		{"single-quoted element", `export const CD_DISPATCH_VARIABLES = ['PETREL_CD_TARGETS'] as const;`},
		{"empty quoted element", `export const CD_DISPATCH_VARIABLES = ["PETREL_CD_TARGETS", ""] as const;`},
		{"two names in one element", `export const CD_DISPATCH_VARIABLES = ["A" "B"] as const;`},
		{"trailing junk after name", `export const CD_DISPATCH_VARIABLES = ["PETREL_CD_TARGETS"x] as const;`},
		{"line comment in array", "export const CD_DISPATCH_VARIABLES = [\n\t\"PETREL_CD_TARGETS\", // not a name\n\t\"PETREL_CD_DISPATCH_ID\",\n] as const;"},
		{"inline block comment in array", `export const CD_DISPATCH_VARIABLES = ["PETREL_CD_TARGETS", /* x */ "PETREL_CD_DISPATCH_ID"] as const;`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if names, err := parseTSDispatchVariables([]byte(c.src)); err == nil {
				t.Fatalf("parsed %v, want error", names)
			}
		})
	}
}

// A well-formed constant parses to its names in order.
func TestParseTSDispatchVariablesAcceptsWellFormed(t *testing.T) {
	src := []byte("export const CD_DISPATCH_VARIABLES = [\n\t\"PETREL_CD_TARGETS\",\n\t\"PETREL_CD_DISPATCH_ID\",\n] as const;")
	names, err := parseTSDispatchVariables(src)
	if err != nil {
		t.Fatalf("parseTSDispatchVariables: %v", err)
	}
	if want := []string{"PETREL_CD_TARGETS", "PETREL_CD_DISPATCH_ID"}; !slices.Equal(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
}
