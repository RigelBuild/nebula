package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/issueowner/linear"
)

type reportJSON struct {
	From    string  `json:"from"`
	To      string  `json:"to"`
	Skipped int     `json:"skipped"`
	Groups  []Group `json:"groups"`
}

func main() {
	// main is the process root context. Every other function takes its caller's ctx.
	if err := runWithClock(context.Background(), os.Args[1:], os.Stdout, os.Stderr, os.Getenv, time.Now); err != nil {
		slog.New(slog.NewTextHandler(os.Stderr, nil)).Error("wave-ledger failed", "error", err)
		os.Exit(1)
	}
}

type environment func(string) string

func runWithClock(ctx context.Context, args []string, stdout, stderr io.Writer, getenv environment, now func() time.Time) error {
	if len(args) == 0 {
		return errors.New("command required: report, outcomes or drivers")
	}
	switch args[0] {
	case "report":
		return runReport(args[1:], stdout, stderr)
	case "drivers":
		return runDrivers(args[1:], stdout, stderr)
	case "outcomes":
		return runOutcomesWithClock(ctx, args[1:], stderr, getenv, now)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runReport(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("report", flag.ContinueOnError)
	flags.SetOutput(stderr)
	fromText := flags.String("from", "", "start UTC date (YYYY-MM-DD)")
	toText := flags.String("to", "", "end UTC date (YYYY-MM-DD)")
	byText := flags.String("by", "", "comma-separated grouping keys")
	dir := flags.String("dir", "", "ledger directory (required)")
	pricesPath := flags.String("prices", "", "price table path")
	jsonOutput := flags.Bool("json", false, "write JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *dir == "" {
		return errors.New("--dir is required")
	}
	from, err := parseDate(*fromText)
	if err != nil {
		return fmt.Errorf("invalid --from: %w", err)
	}
	to, err := parseDate(*toText)
	if err != nil {
		return fmt.Errorf("invalid --to: %w", err)
	}
	if to.Before(from) {
		return errors.New("--to must be on or after --from")
	}
	keys := make([]Key, 0)
	for name := range strings.SplitSeq(*byText, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			return errors.New("--by requires one or more grouping keys")
		}
		key := Key(name)
		if _, ok := groupKeys[key]; !ok {
			return fmt.Errorf("unknown grouping key %q", name)
		}
		keys = append(keys, key)
	}
	root := *dir
	rows, skipped, err := ReadRows(root, from, to)
	if err != nil {
		return err
	}
	table, err := LoadPrices(*pricesPath)
	if err != nil {
		return err
	}
	groups := Aggregate(rows, table, keys)
	if *jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(reportJSON{From: from.Format("2006-01-02"), To: to.Format("2006-01-02"), Skipped: skipped, Groups: groups}); err != nil {
			return fmt.Errorf("writing report JSON: %w", err)
		}
		return nil
	}
	if _, err := fmt.Fprintf(stdout, "from: %s\nto: %s\nskipped: %d\n", from.Format("2006-01-02"), to.Format("2006-01-02"), skipped); err != nil {
		return fmt.Errorf("writing report header: %w", err)
	}
	for _, group := range groups {
		if _, err := fmt.Fprintf(stdout, "%v calls=%d errors=%d compactions=%d cost_usd=%.6f unpriced_tokens=%d\n", group.Key, group.Calls, group.ErrorCalls, group.Compactions, group.CostUSD, group.UnpricedTokens); err != nil {
			return fmt.Errorf("writing report group: %w", err)
		}
	}
	return nil
}

func runOutcomesWithClock(ctx context.Context, args []string, stderr io.Writer, getenv environment, now func() time.Time) error {
	flags := flag.NewFlagSet("outcomes", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dateText := flags.String("date", "", "outcome date (YYYY-MM-DD; defaults to today in UTC)")
	dir := flags.String("dir", "", "ledger directory (required)")
	pricesPath := flags.String("prices", "", "price table path")
	reposText := flags.String("repos", "", "comma-separated GitHub repositories (required)")
	issuePrefixText := flags.String("issue-prefix", "", "issue key prefix, such as PRJ (required)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *dir == "" {
		return errors.New("--dir is required")
	}
	date, err := outcomeDate(*dateText, now)
	if err != nil {
		return err
	}
	root := *dir
	repos, err := parseRepos(*reposText)
	if err != nil {
		return err
	}
	issuePrefix, err := parseIssuePrefix(*issuePrefixText)
	if err != nil {
		return err
	}
	start, end := outcomeWindow(date)
	rows, _, err := ReadRows(root, start, end)
	if err != nil {
		return err
	}
	table, err := LoadPrices(*pricesPath)
	if err != nil {
		return err
	}
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	accessToken := linear.ReadAPIKey(getenv)
	if accessToken == "" {
		return errors.New("linear access token is empty: set LINEAR_API_KEY_FILE")
	}
	api := linear.NewIssueAPIWithToken(http.DefaultClient, "", accessToken)
	source := NewGHSource(execGHRunner{}, repos, issuePrefix, api)
	return WriteOutcomes(ctx, root, date, rows, table, source, issuePrefix)
}

func outcomeDate(value string, now func() time.Time) (time.Time, error) {
	if value != "" {
		date, err := parseDate(value)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid --date: %w", err)
		}
		return date, nil
	}
	if now == nil {
		return time.Time{}, errors.New("clock is nil")
	}
	return utcDay(now()), nil
}

func outcomeWindow(date time.Time) (time.Time, time.Time) {
	end := utcDay(date)
	return end.AddDate(0, 0, -29), end
}

func parseRepos(value string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return nil, errors.New("--repos is required")
	}
	repos := make([]string, 0)
	seen := make(map[string]struct{})
	for repo := range strings.SplitSeq(value, ",") {
		repo = strings.TrimSpace(repo)
		if !repoPattern.MatchString(repo) {
			return nil, fmt.Errorf("invalid repository %q", repo)
		}
		if _, exists := seen[repo]; exists {
			return nil, fmt.Errorf("duplicate repository %q", repo)
		}
		seen[repo] = struct{}{}
		repos = append(repos, repo)
	}
	return repos, nil
}

func parseIssuePrefix(value string) (string, error) {
	if value == "" {
		return "", errors.New("--issue-prefix is required")
	}
	if !prefixPattern.MatchString(value) {
		return "", fmt.Errorf("invalid --issue-prefix %q", value)
	}
	return value, nil
}

func parseDate(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, errors.New("date is required")
	}
	date, err := time.Parse("2006-01-02", value)
	if err != nil || date.Format("2006-01-02") != value {
		return time.Time{}, errors.New("expected YYYY-MM-DD")
	}
	return date.UTC(), nil
}
