package main

import (
	"reflect"
	"testing"
)

func TestAggregateNullIssueGroupsAsUntagged(t *testing.T) {
	input := int64(100)
	rows := []Row{{V: 1, Kind: "message", RowKey: "r1", Issue: nil, Input: &input, Model: new("model-a")}}
	groups := Aggregate(rows, PriceTable{}, []Key{"issue"})
	if len(groups) != 1 {
		t.Fatalf("Aggregate() returned %d groups, want 1", len(groups))
	}
	if got := groups[0].Key[Key("issue")]; got != "untagged" {
		t.Fatalf("issue key = %q, want untagged", got)
	}
	if groups[0].Calls != 1 || groups[0].Input != input {
		t.Fatalf("group = %+v, want one call and %d input tokens", groups[0], input)
	}
}

func TestParseReposValidatesOverrides(t *testing.T) {
	repos, err := parseRepos("owner/one,Team-1/repo_name")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"owner/one", "Team-1/repo_name"}; !reflect.DeepEqual(repos, want) {
		t.Fatalf("parseRepos() = %v, want %v", repos, want)
	}
	for _, value := range []string{"", "   ", "owner/one,bad repo", "owner/one,owner/one", "/repo", "owner/"} {
		if _, err := parseRepos(value); err == nil {
			t.Errorf("parseRepos(%q) error = nil, want validation failure", value)
		}
	}
}

func TestParseIssuePrefixRequiresUpperCaseKey(t *testing.T) {
	prefix, err := parseIssuePrefix("EX")
	if err != nil {
		t.Fatal(err)
	}
	if prefix != "EX" {
		t.Fatalf("parseIssuePrefix() = %q, want EX", prefix)
	}
	for _, value := range []string{"", "ex", "E-X", "1EX"} {
		if _, err := parseIssuePrefix(value); err == nil {
			t.Errorf("parseIssuePrefix(%q) error = nil, want validation failure", value)
		}
	}
}

func TestParseDateRequiresCanonicalDate(t *testing.T) {
	date, err := parseDate("2026-11-09")
	if err != nil {
		t.Fatal(err)
	}
	if got := date.Format("2006-01-02"); got != "2026-11-09" {
		t.Fatalf("parseDate() = %q, want 2026-11-09", got)
	}
	for _, value := range []string{"", "2026-2-9", "2026-02-30"} {
		if _, err := parseDate(value); err == nil {
			t.Errorf("parseDate(%q) error = nil, want invalid date", value)
		}
	}
}
