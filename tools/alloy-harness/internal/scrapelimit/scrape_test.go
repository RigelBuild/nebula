package scrapelimit

import (
	"strings"
	"testing"
)

func TestBodyHasExactSampleCount(t *testing.T) {
	for _, n := range []int{0, 1, 64, 65} {
		body := Body(n)
		got := strings.Count(body, "fixture_sample{")
		if got != n {
			t.Errorf("Body(%d) has %d samples, want %d", n, got, n)
		}
	}
}

func TestConfigCarriesLimitAndPort(t *testing.T) {
	cfg := Config(9123, 64)
	for _, want := range []string{`127.0.0.1:9123`, `sample_limit    = 64`} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config is missing %q:\n%s", want, cfg)
		}
	}
}

func TestExceededTotalSumsSeries(t *testing.T) {
	page := "# HELP x\n" +
		Exceeded + " 2\n" +
		Exceeded + "{job=\"a\"} 3\n" +
		"other_metric 9\n"
	if got := ExceededTotal(page); got != 5 {
		t.Errorf("ExceededTotal = %v, want 5", got)
	}
}

func TestExceededTotalIgnoresUnparseable(t *testing.T) {
	if got := ExceededTotal(Exceeded + " notanumber\n"); got != 0 {
		t.Errorf("ExceededTotal = %v, want 0 for an unparseable value", got)
	}
}

// TestSettledRefusesOverLimitWithoutRefusal pins the rule that an over-limit target
// with no counted refusal does not settle, however many scrapes it answered.
func TestSettledRefusesOverLimitWithoutRefusal(t *testing.T) {
	if Settled(65, 64, 10, 0) {
		t.Fatal("an over-limit target with no refusal settled")
	}
	if !Settled(65, 64, 10, 1) {
		t.Fatal("an over-limit target with a refusal did not settle")
	}
}

func TestSettledNeedsMinHits(t *testing.T) {
	if Settled(64, 64, MinHits-1, 0) {
		t.Fatal("a target that answered too few scrapes settled")
	}
	if !Settled(64, 64, MinHits, 0) {
		t.Fatal("an at-limit target with enough scrapes did not settle")
	}
}

func TestScrapeLinesKeepsOnlyScrapeSeries(t *testing.T) {
	page := "# scrape help\nprometheus_target_scrapes_x 1\nunrelated 2\n"
	got := ScrapeLines(page)
	if strings.Contains(got, "unrelated") || strings.Contains(got, "# scrape") {
		t.Errorf("ScrapeLines kept the wrong lines: %q", got)
	}
	if !strings.Contains(got, "prometheus_target_scrapes_x 1") {
		t.Errorf("ScrapeLines dropped a scrape series: %q", got)
	}
}

// TestSettledUsesLiteralMinHits pins the threshold to 3 by value. Asserting MinHits
// alone would pass if someone changed the constant, so this test keeps the number.
func TestSettledUsesLiteralMinHits(t *testing.T) {
	if MinHits != 3 {
		t.Fatalf("MinHits = %d, want 3", MinHits)
	}
	if Settled(64, 64, 2, 0) {
		t.Fatal("two scrapes settled an at-limit target")
	}
	if !Settled(64, 64, 3, 0) {
		t.Fatal("three scrapes did not settle an at-limit target")
	}
}

// TestChecksAtLimitCleanBoundary pins the rule that an at-limit target passes the
// clean check only with zero refusals.
func TestChecksAtLimitCleanBoundary(t *testing.T) {
	over := Result{Exceeded: 1, Hits: 3}
	for _, tc := range []struct {
		name     string
		exceeded float64
		want     bool
	}{
		{"zero refusals pass", 0, true},
		{"one refusal fails", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Checks(over, Result{Exceeded: tc.exceeded, Hits: 3})
			if got[2].OK != tc.want {
				t.Fatalf("at-limit clean check OK = %v, want %v", got[2].OK, tc.want)
			}
		})
	}
}

// TestChecksOverLimitBoundary pins the rule that an over-limit target passes its check
// only when Alloy counted at least one refusal.
func TestChecksOverLimitBoundary(t *testing.T) {
	at := Result{Exceeded: 0, Hits: 3}
	for _, tc := range []struct {
		name     string
		exceeded float64
		want     bool
	}{
		{"no refusal fails", 0, false},
		{"one refusal passes", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Checks(Result{Exceeded: tc.exceeded, Hits: 3}, at)
			if got[0].OK != tc.want {
				t.Fatalf("over-limit check OK = %v, want %v", got[0].OK, tc.want)
			}
		})
	}
}

// TestChecksMinHitsBoundary pins the rule that the answered-scrapes check needs
// MinHits scrapes: two fail, three pass.
func TestChecksMinHitsBoundary(t *testing.T) {
	over := Result{Exceeded: 1, Hits: 3}
	if Checks(over, Result{Hits: 2})[1].OK {
		t.Fatal("two scrapes passed the answered-scrapes check")
	}
	if !Checks(over, Result{Hits: 3})[1].OK {
		t.Fatal("three scrapes failed the answered-scrapes check")
	}
}

// TestChecksOrderAndNames pins the three check names and their order, which is the
// order the harness prints them.
func TestChecksOrderAndNames(t *testing.T) {
	got := Checks(Result{Exceeded: 1, Hits: 3}, Result{Hits: 3})
	if len(got) != 3 {
		t.Fatalf("Checks returned %d checks, want 3", len(got))
	}
	if !strings.HasPrefix(got[0].Name, "65 samples with sample_limit 64 raise ") {
		t.Errorf("check 0 name = %q", got[0].Name)
	}
	if !strings.HasPrefix(got[1].Name, "64-sample target answered ") {
		t.Errorf("check 1 name = %q", got[1].Name)
	}
	if !strings.HasPrefix(got[2].Name, "64 samples with sample_limit 64 scrape cleanly") {
		t.Errorf("check 2 name = %q", got[2].Name)
	}
}
