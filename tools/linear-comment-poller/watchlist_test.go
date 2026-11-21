package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// legacy7 uses the DEMO alias, which normalises to the canonical EX-7.
const legacy7 = "demo-7"

func examplePrefixes(t *testing.T) issuePrefixes {
	t.Helper()
	prefixes, err := parseIssuePrefixes([]string{"EX", "demo"})
	if err != nil {
		t.Fatal(err)
	}
	return prefixes
}

func TestParseIssuePrefixes(t *testing.T) {
	for _, test := range []struct {
		name      string
		values    []string
		wantKeys  string
		wantError string
	}{
		{name: "missing", values: nil, wantError: "at least one team key"},
		{name: "empty value", values: []string{" , "}, wantError: "at least one team key"},
		{name: "invalid key", values: []string{"EX-1"}, wantError: "must be a letter"},
		{name: "leading digit", values: []string{"1EX"}, wantError: "must be a letter"},
		{name: "repeated and comma list", values: []string{"ex", "DEMO,Q2"}, wantKeys: "EX,DEMO,Q2"},
	} {
		t.Run(test.name, func(t *testing.T) {
			prefixes, err := parseIssuePrefixes(test.values)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("parseIssuePrefixes error = %v, want %q", err, test.wantError)
				}
				return
			}
			if err != nil || strings.Join(prefixes.keys, ",") != test.wantKeys {
				t.Fatalf("parseIssuePrefixes = %v, %v", prefixes.keys, err)
			}
		})
	}
	if query := commentsQuery(examplePrefixes(t)); !strings.Contains(query, `key: { eq: "EX" }`) {
		t.Fatalf("query team filter = %s", query)
	}
}

func TestNormaliseIssueID(t *testing.T) {
	for _, test := range []struct {
		name, input, expected string
		valid                 bool
	}{
		{name: "lowercase", input: "ex-42", expected: "EX-42", valid: true},
		{name: "alias", input: legacy7, expected: "EX-7", valid: true},
		{name: "unsupported team", input: "TEAM-7"},
		{name: "zero", input: "EX-0"},
		{name: "unsafe integer", input: "EX-9007199254740992"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, ok := examplePrefixes(t).normaliseIssueID(test.input)
			if ok != test.valid || got != test.expected {
				t.Fatalf("normaliseIssueID(%q) = %q, %v", test.input, got, ok)
			}
		})
	}
}

func TestParseWatchlist(t *testing.T) {
	for _, test := range []struct{ name, raw, wantError string }{
		{name: "normalises legacy", raw: `{"schemaVersion":"watchlist-v1","issues":[{"id":"` + legacy7 + `","watermark":null}]}`},
		{name: "invalid schema", raw: `{"schemaVersion":"bad","issues":[]}`, wantError: "invalid schemaVersion"},
		{name: "invalid id", raw: `{"schemaVersion":"watchlist-v1","issues":[{"id":"bad","watermark":null}]}`, wantError: "invalid id"},
		{name: "invalid watermark", raw: `{"schemaVersion":"watchlist-v1","issues":[{"id":"EX-7","watermark":"yesterday"}]}`, wantError: "watermark"},
		{name: "non array issues", raw: `{"schemaVersion":"watchlist-v1","issues":{}}`, wantError: "issues must be an array"},
	} {
		t.Run(test.name, func(t *testing.T) {
			watchlist, err := parseWatchlist(examplePrefixes(t), json.RawMessage(test.raw))
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("parseWatchlist error = %v, want %q", err, test.wantError)
				}
				return
			}
			if err != nil || watchlist.Issues[0].ID != "EX-7" {
				t.Fatalf("parseWatchlist = %#v, %v", watchlist, err)
			}
		})
	}
}

func TestReadWatchlists(t *testing.T) {
	root := t.TempDir()
	for _, codename := range []string{"agent-a", "agent-b", "agent-c", "empty"} {
		if err := os.Mkdir(filepath.Join(root, codename), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"agent-a": `{"schemaVersion":"watchlist-v1","issues":[{"id":"` + legacy7 + `","watermark":"2026-10-03T22:25:53.156Z"},{"id":"EX-9","watermark":null}]}`,
		"agent-b": `{"schemaVersion":"watchlist-v1","issues":[{"id":"EX-7","watermark":null}]}`,
		"agent-c": "not json",
	}
	for codename, contents := range files {
		if err := os.WriteFile(filepath.Join(root, codename, "watchlist.json"), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	result, err := readWatchlists(examplePrefixes(t), root)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(result.IDs, ",") != "EX-7,EX-9" || strings.Join(result.Skipped, ",") != "agent-c" || strings.Join(result.Present, ",") != "agent-a,agent-b,agent-c,empty" {
		t.Fatalf("readWatchlists = %#v", result)
	}
	if len(result.Agents) != 2 || result.Agents[0].Watchlist.Issues[0].Watermark == nil || len(result.Warnings) != 1 {
		t.Fatalf("readWatchlists agent results = %#v", result)
	}
	if got, want := result.Warnings[0], `agent-c/watchlist.json: JSON Parse error: Unexpected identifier "not"`; got != want {
		t.Fatalf("watchlist warning = %q, want %q", got, want)
	}
}
