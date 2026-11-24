package issueowner

import (
	"strings"
	"testing"
)

func TestIsStateType(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "backlog", value: "backlog", want: true},
		{name: "unstarted", value: "unstarted", want: true},
		{name: "started", value: "started", want: true},
		{name: "completed", value: "completed", want: true},
		{name: "canceled", value: "canceled", want: true},
		{name: "triage", value: "triage"},
		{name: "duplicate", value: "duplicate"},
		{name: "capitalized", value: "Backlog"},
		{name: "empty", value: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := IsStateType(test.value); got != test.want {
				t.Fatalf("IsStateType(%q) = %t, want %t", test.value, got, test.want)
			}
		})
	}
}

func TestReadOwnerFromBody(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
		ok   bool
	}{
		{name: "canonical at start", body: "Owner: ci-build (service owner)\n\nBody.", want: "ci-build", ok: true},
		{name: "mid sentence example", body: "Follow-up to EX-1128 (PR #450). Owner: ci-build.", want: "ci-build", ok: true},
		{name: "canonical beyond legacy window", body: strings.Repeat("Design + recommendation only. ", 54) + "\n\nOwner: nix-infra lane.", want: "nix-infra", ok: true},
		{name: "slash suffix", body: "Owner: agents/service-owner\n\nBody.", want: "agents", ok: true},
		{name: "slash form", body: "Owner: skills/service-owner", want: "skills", ok: true},
		{name: "bold marker", body: "**Owner:** org\n\nBody.", want: "org", ok: true},
		{name: "bold punctuation", body: "**Owner: business-stack.** Task.", want: "business-stack", ok: true},
		{name: "bold close", body: "**Owner: business-stack**\n\nAssignee: Mintaka.", want: "business-stack", ok: true},
		{name: "semicolon", body: "Owner: krypton-forge;", want: "krypton-forge", ok: true},
		{name: "legacy bold", body: "Follow-up to X. **Owner: frontend** done.", want: "frontend", ok: true},
		{name: "legacy slash", body: "Follow-up. Owner: agents/service-owner.", want: "agents", ok: true},
		{name: "legacy semicolon", body: "Filed by hand. Owner: krypton-forge; see table.", want: "krypton-forge", ok: true},
		{name: "asterisk bounds token", body: "Owner: ci*build", want: "ci", ok: true},
		{name: "canonical beats legacy", body: "Owner: nix-infra\n\nFollow-up to X. Owner: ci-build.", want: "nix-infra", ok: true},
		{name: "canonical after heading", body: "## Context\n\nOwner: ci-build\n\nBody.", want: "ci-build", ok: true},
		{name: "unowned", body: "Just a normal issue body."},
		{name: "empty", body: ""},
		{name: "parenthetical ignored", body: "Owner: ci-build (service owner)", want: "ci-build", ok: true},
		{name: "inside legacy window", body: strings.Repeat("x", LegacyWindow-60) + ". Owner: ci-build.", want: "ci-build", ok: true},
		{name: "past legacy window", body: strings.Repeat("x", LegacyWindow+50) + ". Owner: ci-build."},
		{name: "canonical ignores legacy bound", body: strings.Repeat("x", LegacyWindow+50) + "\n\nOwner: ci-build", want: "ci-build", ok: true},
		{name: "offset bound does not truncate token", body: strings.Repeat("x", LegacyWindow-14) + ". Owner: ci-build-service.", want: "ci-build-service", ok: true},
		{name: "underscore does not truncate canonical token", body: "Owner: ci-build_v2\n\nBody."},
		{name: "underscore does not truncate legacy token", body: "Follow-up to X. Owner: ci-build_v2."},
		{name: "Unicode whitespace terminator", body: "Owner: ci-build\u00a0(service)", want: "ci-build", ok: true},
		{name: "UTF-16 window includes supplementary rune", body: strings.Repeat("😀", 249) + ". Owner: ci-build.", want: "ci-build", ok: true},
		{name: "UTF-16 window excludes offset at 500", body: strings.Repeat("😀", 250) + ". Owner: ci-build."},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := ReadOwnerFromBody(test.body)
			if got != test.want || ok != test.ok {
				t.Fatalf("ReadOwnerFromBody() = (%q, %t), want (%q, %t)", got, ok, test.want, test.ok)
			}
		})
	}
}

func TestReadOwnerDiagnosticFromBody(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantLine   string
		wantDetail string
		want       bool
	}{
		{name: "malformed uppercase", body: "Owner: gateway-TS", wantLine: "Owner: gateway-TS", wantDetail: "gateway-ts", want: true},
		{name: "underscore token", body: "Owner: ci-build_v2\n\nBody.", wantLine: "Owner: ci-build_v2", wantDetail: "ci-build_v2", want: true},
		{name: "uppercase owner", body: "Owner: CI-build\n\nBody.", wantLine: "Owner: CI-build", wantDetail: "lowercase", want: true},
		{name: "well formed canonical", body: "Owner: ci-build\n\nBody."},
		{name: "well formed legacy", body: "Follow-up. Owner: ci-build."},
		{name: "no ownership claim", body: "A normal body."},
		{name: "empty token", body: "Owner:\n\nBody.", wantLine: "Owner:", wantDetail: "no token", want: true},
		{name: "miscased key", body: "owner: ci-build\n\nBody.", wantLine: "owner: ci-build", wantDetail: "canonical form", want: true},
		{name: "digit start", body: "Owner: 2nd-team\n\nBody.", wantLine: "Owner: 2nd-team", wantDetail: "[a-z][a-z0-9-]*", want: true},
		{name: "parsed line suppresses malformed other line", body: "Owner: ci-build\n\nSee Owner: CI-build"},
		{name: "canonical padding is reported", body: "Owner : ci-build", wantLine: "Owner : ci-build", wantDetail: "canonical form", want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := ReadOwnerDiagnosticFromBody(test.body)
			if ok != test.want {
				t.Fatalf("ReadOwnerDiagnosticFromBody() present = %t, want %t", ok, test.want)
			}
			if !ok {
				return
			}
			if got.Kind != OwnerDiagnosticMalformed || got.Line != test.wantLine || !strings.Contains(got.Detail, test.wantDetail) {
				t.Fatalf("diagnostic = %#v, want malformed line %q containing %q", got, test.wantLine, test.wantDetail)
			}
		})
	}
}

func TestWriteOwnerInBody(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "unowned body", body: "Some body text.", want: "Owner: ci-build\n\nSome body text."},
		{name: "replace owner", body: "Owner: nix-infra\n\nSome body text.", want: "Owner: ci-build\n\nSome body text."},
		{name: "legacy remains prose", body: "Follow-up to X. Owner: ci-build.", want: "Owner: ci-build\n\nFollow-up to X. Owner: ci-build."},
		{name: "empty", body: "", want: "Owner: ci-build"},
		{name: "same owner no op", body: "Owner: ci-build\n\nSome body text.", want: "Owner: ci-build\n\nSome body text."},
		{name: "only owner", body: "Owner: ci-build", want: "Owner: ci-build"},
		{name: "preserve whitespace", body: "Line one.\nLine two.  \n\n- a bullet\n- another\n\n\nTrailing.\n", want: "Owner: ci-build\n\nLine one.\nLine two.  \n\n- a bullet\n- another\n\n\nTrailing.\n"},
		{name: "bold owner in leading block", body: "**Owner:** org\n\nBody.", want: "Owner: ci-build\n\nBody."},
		{name: "design record prose", body: "# Agent issue ownership\n\nStatus: proposed. Owner: mercator (design), pending ruling.\n\nRest.", want: "Owner: ci-build\n\n# Agent issue ownership\n\nStatus: proposed. Owner: mercator (design), pending ruling.\n\nRest."},
		{name: "fenced example below leading block", body: "Intro.\n\n```text\nOwner: ci-build\n```\n\nNote.", want: "Owner: ci-build\n\nIntro.\n\n```text\nOwner: ci-build\n```\n\nNote."},
		{name: "leading owner after other text", body: "## Context\nOwner: nix-infra\nMore leading text.\n\nTail.", want: "Owner: ci-build\n\n## Context\nMore leading text.\n\nTail."},
		{name: "whitespace separator protects tail", body: "Intro.\n \nOwner: nix-infra\n", want: "Owner: ci-build\n\nIntro.\n \nOwner: nix-infra\n"},
		{name: "one leading block", body: "Owner: nix-infra\nStill the same block.", want: "Owner: ci-build\n\nStill the same block."},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := WriteOwnerInBody(test.body, "ci-build"); got != test.want {
				t.Fatalf("WriteOwnerInBody() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestWriteOwnerInBodyIsIdempotent(t *testing.T) {
	bodies := []string{
		"Some body text.",
		"Owner: nix-infra\n\nSome body text.",
		"Follow-up to X. Owner: ci-build.",
		"",
		"Owner: ci-build",
		"## Context\n\nOwner: ci-build\n\nMore.",
		"Owner: ci-build\n\nBody.",
	}
	for _, body := range bodies {
		t.Run(body, func(t *testing.T) {
			first := WriteOwnerInBody(body, "ci-build")
			if got := WriteOwnerInBody(first, "ci-build"); got != first {
				t.Fatalf("second pass changed body: got %q, want %q", got, first)
			}
		})
	}
	body := "Owner: ci-build\n\nBody."
	for range 4 {
		body = WriteOwnerInBody(body, "ci-build")
	}
	if body != "Owner: ci-build\n\nBody." {
		t.Fatalf("repeated write accumulated separators: %q", body)
	}
}

func TestQuoteJS(t *testing.T) {
	got := quoteJS("<tag>&\u2028line\u2029")
	want := "\"<tag>&\u2028line\u2029\""
	if got != want {
		t.Fatalf("quoteJS() = %q, want %q", got, want)
	}
}
