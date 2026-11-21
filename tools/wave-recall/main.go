// Package main is wave-recall, the dreamer's Hindsight recall client.
//
// It runs one recall query against one memory bank and prints one line per
// result: the memory id, a tab, and the memory text with newlines escaped.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

// usageError marks a bad command line. main exits 2 for it and 1 for other failures.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

// lineEscaper keeps each result on one output line. A backslash is escaped first, so a literal
// backslash-n in memory text is never read back as an escaped newline.
var lineEscaper = strings.NewReplacer(`\`, `\\`, "\r", `\r`, "\n", `\n`)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Getenv, os.Stdout)
	stop()
	if err == nil {
		return
	}
	code := 1
	if _, ok := errors.AsType[usageError](err); ok {
		code = 2
	}
	// Stderr has no caller to report a failed write to; the exit code still signals the failure.
	_, _ = fmt.Fprintf(os.Stderr, "wave-recall: %v\n", err)
	os.Exit(code)
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout io.Writer) error {
	flags := flag.NewFlagSet("wave-recall", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	bank := flags.String("bank", "", "Hindsight bank id")
	query := flags.String("query", "", "recall query text")
	maxTokens := flags.Int("max-tokens", 0, "token budget for the recall response")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(stdout)
			flags.PrintDefaults()
			return nil
		}
		return usageError{msg: err.Error()}
	}
	if *bank == "" || *query == "" || *maxTokens <= 0 {
		return usageError{msg: "--bank, --query and a positive --max-tokens are required"}
	}
	baseURL, err := hindsightURL(getenv)
	if err != nil {
		return err
	}
	results, err := recall(ctx, baseURL, getenv("HINDSIGHT_API_KEY"), *bank, *query, *maxTokens)
	if err != nil {
		return err
	}
	return writeResults(stdout, results)
}

// hindsightURL reads the API base URL and rejects anything that is not an absolute http or https URL.
func hindsightURL(getenv func(string) string) (string, error) {
	raw := strings.TrimRight(getenv("HINDSIGHT_URL"), "/")
	if raw == "" {
		return "", errors.New("HINDSIGHT_URL is not set")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("HINDSIGHT_URL: %w", err)
	}
	// The endpoint path is appended to this string, so a query or fragment would end up after it.
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("HINDSIGHT_URL must be an absolute http or https URL")
	}
	return raw, nil
}

// writeResults prints one id<TAB>text line per result. recall has already checked that each id is a safe token and that text is set.
func writeResults(w io.Writer, results []recallResult) error {
	for _, r := range results {
		if _, err := fmt.Fprintf(w, "%s\t%s\n", *r.ID, lineEscaper.Replace(*r.Text)); err != nil {
			return fmt.Errorf("write results: %w", err)
		}
	}
	return nil
}
