package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RigelBuild/nebula/tools/go-lib/prgates"
	"github.com/spf13/cobra"
)

func TestMetadataFlagBody(t *testing.T) {
	file := filepath.Join(t.TempDir(), "section")
	if err := os.WriteFile(file, []byte("from file"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := "## What does this do\n\nold behavior\n\n## Why do we need it\n\nold problem\n\n## Summary\n\nold summary\n\n## Verification\n\nold verification\n\n## Risks\n\nold risks\n\n## Spec impact\n\nold impact\n"
	for _, tc := range []struct {
		name               string
		args               []string
		heading, old, want string
		wantErr            bool
		absent             []string
		spec               bool
	}{
		{name: "what does this do", args: []string{"--section", "What does this do=plain behavior"}, heading: "What does this do", old: "old behavior", want: "plain behavior"},
		{name: "why do we need it", args: []string{"--section", "Why do we need it=clear problem"}, heading: "Why do we need it", old: "old problem", want: "clear problem"},
		{name: "summary", args: []string{"--summary", "new summary"}, heading: "Summary", old: "old summary", want: "new summary"},
		{name: "verification", args: []string{"--verification", "@" + file}, heading: "Verification", old: "old verification", want: "from file"},
		{name: "risks", args: []string{"--risks", "new risks"}, heading: "Risks", old: "old risks", want: "new risks"},
		{name: "spec impact", args: []string{"--spec-impact", "none — reason"}, heading: "Spec impact", old: "old impact", want: "Spec-impact: none — reason", spec: true},
		{name: "existing spec impact", args: []string{"--spec-impact", "\n spec-impact: none"}, heading: "Spec impact", old: "old impact", want: "spec-impact: none", absent: []string{"Spec-impact: spec-impact:"}, spec: true},
		{name: "section replacement", args: []string{"--section", "Summary=custom"}, heading: "Summary", old: "old summary", want: "custom"},
		{name: "section append", args: []string{"--section", "Extra=@" + file}, heading: "Extra", want: "from file"},
		{name: "precedence", args: []string{"--summary", "named", "--section", "Summary=first", "--section", "Summary=last"}, heading: "Summary", old: "old summary", want: "named", absent: []string{"first", "last"}},
		{name: "last wins", args: []string{"--section", "Summary=first", "--section", "Summary=last"}, heading: "Summary", old: "old summary", want: "last", absent: []string{"first"}},
		{name: "missing file", args: []string{"--summary", "@" + file + "-missing"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var flags metadataFlags
			cmd := &cobra.Command{Use: "meta"}
			flags.bind(cmd)
			if err := cmd.ParseFlags(tc.args); err != nil {
				t.Fatal(err)
			}
			resolved, err := flags.resolve()
			if (err != nil) != tc.wantErr {
				t.Fatalf("resolve error = %v", err)
			}
			if tc.wantErr {
				return
			}
			got, err := metadataBody(base, "", resolved)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(got, tc.want) || (tc.old != "" && strings.Contains(got, tc.old)) || strings.Count(got, "## "+tc.heading+"\n") != 1 {
				t.Fatalf("body = %q", got)
			}
			for _, absent := range tc.absent {
				if strings.Contains(got, absent) {
					t.Fatalf("body = %q", got)
				}
			}
			if tc.spec && !prgates.SpecImpact(prgates.SpecImpactFacts{Body: got, SpecDirs: []string{"docs/"}}).Pass {
				t.Fatalf("spec gate refused %q", got)
			}
		})
	}
}

func TestReplaceSectionCommentsAndDuplicates(t *testing.T) {
	const appendIfMissing = false
	got, err := replaceSection("<!--\n## Summary\nignored\n-->\n## Summary\nold\n", "Summary", "new", appendIfMissing)
	if err != nil || strings.Count(got, "## Summary") != 1 || strings.Contains(got, "old") {
		t.Fatalf("body = %q, err = %v", got, err)
	}
	_, err = replaceSection("## Summary\none\n## Summary\ntwo\n", "Summary", "new", appendIfMissing)
	if err == nil {
		t.Fatal("duplicate heading accepted")
	}
}

func TestMetadataFlagTitle(t *testing.T) {
	for _, tc := range []struct {
		name, title, typ, scope, want string
		breaking, wantErr             bool
	}{
		{name: "type", title: "add feature", typ: "feat", want: "feat: add feature"},
		{name: "scope and breaking", title: "change API", typ: "fix", scope: "api", breaking: true, want: "fix(api)!: change API"},
		{name: "unknown", title: "subject", typ: "wip", wantErr: true},
		{name: "conflict", title: "feat: subject", typ: "fix", wantErr: true},
		{name: "other type conflict", title: "ci: subject", typ: "fix", wantErr: true},
		{name: "trimmed conflict", title: "  feat: subject", typ: "fix", wantErr: true},
		{name: "subject colon", title: "Note: subject", typ: "feat", want: "feat: Note: subject"},
		{name: "case conflict", title: "Feat:subject", typ: "fix", wantErr: true},
		{name: "empty", typ: "feat", wantErr: true},
		{name: "invalid scope", title: "subject", typ: "fix", scope: "a)b", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := (&metadataFlags{typ: tc.typ, scope: tc.scope, breaking: tc.breaking}).composeTitle(tc.title)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("title = %q, error = %v", got, err)
			}
		})
	}
	for _, typ := range conventionalTypes {
		t.Run("accept "+typ, func(t *testing.T) {
			got, err := (&metadataFlags{typ: typ}).composeTitle("add feature")
			if err != nil || got != typ+": add feature" {
				t.Fatalf("title = %q, error = %v", got, err)
			}
		})
		t.Run("prefixed title "+typ, func(t *testing.T) {
			if _, err := (&metadataFlags{typ: "feat"}).composeTitle(typ + ":subject"); err == nil {
				t.Fatalf("accepted title with existing %q prefix", typ)
			}
		})
	}
}

func TestMetadataFlagsCLI(t *testing.T) {
	for _, command := range []string{"submit", "meta"} {
		t.Run(command, func(t *testing.T) {
			args := []string{command}
			if command == "meta" {
				args = append(args, "bookmark")
			}
			args = append(args, "--title", "subject", "--type", "wip")
			var output bytes.Buffer
			if code := run(t.Context(), args, &output, &output); code != 2 {
				t.Fatalf("exit = %d, output = %s", code, &output)
			}
		})
	}
	var output bytes.Buffer
	if code := run(t.Context(), []string{"submit", "--help"}, &output, &output); code != 0 {
		t.Fatalf("help exit = %d, output = %s", code, &output)
	}
	for _, flag := range []string{"--summary", "--verification", "--risks", "--spec-impact", "--section", "--type", "--scope", "--breaking", "--review"} {
		if !strings.Contains(output.String(), flag) {
			t.Errorf("help missing %s", flag)
		}
	}
}

func TestMetadataFlagsCLISave(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): add change", "")
	f.commit(t, "feature.go")
	var out bytes.Buffer
	args := []string{"meta", bookmark, "--title", "test metadata flag saving", "--body-file", f.bodyFile, "--summary", "X", "--section", "Notes=Y", "--type", "feat", "--verification", strings.Repeat("verified behavior ", 20), "--risks", "none"}
	if code := run(f.ctx, args, &out, &out); code != 0 {
		t.Fatalf("exit=%d output=%s", code, &out)
	}
	candidate, err := f.repo.loadCandidate(bookmark)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.title != "feat: test metadata flag saving (EX-1)" || !strings.Contains(candidate.body, "## Summary\n\nX") || !strings.Contains(candidate.body, "## Notes\n\nY") {
		t.Fatalf("candidate = %+v", candidate)
	}
}

//nolint:tparallel // Subtests share the parent fixture.
func TestMetadataFlagsCLIRefusals(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): add change", "")
	f.commit(t, "feature.go")
	f.commit(t, "unbookmarked.go")
	before := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	duplicate := filepath.Join(t.TempDir(), "duplicate.md")
	writeTestFile(t, duplicate, "## Summary\none\n## Summary\ntwo\n")
	short := filepath.Join(t.TempDir(), "short.md")
	writeTestFile(t, short, "## What does this do\nBrief.\n## Why do we need it\nA test need.\n## Summary\nBrief.\n## Verification\nTested.\n## Risks\nNone.\n")
	baseBody, err := os.ReadFile(f.bodyFile)
	if err != nil {
		t.Fatal(err)
	}
	missingSection := filepath.Join(t.TempDir(), "missing-section.md")
	missingSectionBody := strings.Replace(string(baseBody), "## Risks\n\nNone: this test has no live remote.\n\n", "", 1)
	writeTestFile(t, missingSection, missingSectionBody)
	for _, tc := range []struct {
		name, body string
	}{
		{name: "missing-what", body: strings.Replace(string(baseBody), "## What does this do\n\nThis fixture submits a feature through the real jj state checks.\n\n", "", 1)},
		{name: "missing-why", body: strings.Replace(string(baseBody), "## Why do we need it\n\nThe fake remote keeps integration tests offline while exercising submit behavior.\n\n", "", 1)},
		{name: "empty-what", body: strings.Replace(string(baseBody), "## What does this do\n\nThis fixture submits a feature through the real jj state checks.", "## What does this do", 1)},
		{name: "empty-why", body: strings.Replace(string(baseBody), "## Why do we need it\n\nThe fake remote keeps integration tests offline while exercising submit behavior.", "## Why do we need it", 1)},
	} {
		writeTestFile(t, filepath.Join(filepath.Dir(f.bodyFile), tc.name+".md"), tc.body)
	}
	for _, tc := range []struct {
		name, gate string
		args       []string
		code       int
	}{
		{name: "bad syntax", args: []string{"submit", bookmark, "--title", "feat: subject", "--section", "broken"}, code: 2},
		{name: "unreadable file", args: []string{"submit", bookmark, "--title", "feat: subject", "--summary", "@" + filepath.Join(t.TempDir(), "missing")}, code: 1},
		{name: "unreadable body", args: []string{"submit", bookmark, "--title", "feat: subject", "--body-file", filepath.Join(t.TempDir(), "missing")}, code: 1},
		{name: "missing section", gate: "body-template", args: []string{"submit", bookmark, "--title", "feat: subject for valid metadata", "--body-file", missingSection}, code: 1},
		{name: "duplicate heading", args: []string{"submit", bookmark, "--title", "feat: subject for valid metadata", "--body-file", duplicate, "--summary", "replacement"}, code: 1},
		{name: "bad title", args: []string{"submit", bookmark, "--title", "add a thing", "--body-file", f.bodyFile}, code: 1},
		{name: "short body", args: []string{"submit", bookmark, "--title", "feat: a valid longer title", "--body-file", short}, code: 1},
		{name: "missing what", gate: "body-template", args: []string{"submit", bookmark, "--title", "feat: a valid longer title", "--body-file", filepath.Join(filepath.Dir(f.bodyFile), "missing-what.md")}, code: 1},
		{name: "missing why", gate: "body-template", args: []string{"submit", bookmark, "--title", "feat: a valid longer title", "--body-file", filepath.Join(filepath.Dir(f.bodyFile), "missing-why.md")}, code: 1},
		{name: "empty what", gate: "body-template", args: []string{"submit", bookmark, "--title", "feat: a valid longer title", "--body-file", filepath.Join(filepath.Dir(f.bodyFile), "empty-what.md")}, code: 1},
		{name: "empty why", gate: "body-template", args: []string{"submit", bookmark, "--title", "feat: a valid longer title", "--body-file", filepath.Join(filepath.Dir(f.bodyFile), "empty-why.md")}, code: 1},
		{name: "empty title", args: []string{"submit", bookmark, "--title", "", "--type", "feat"}, code: 2},
		{name: "invalid scope", args: []string{"submit", bookmark, "--title", "subject", "--type", "feat", "--scope", "a)b"}, code: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if code := run(f.ctx, tc.args, &out, &out); code != tc.code {
				t.Fatalf("exit=%d output=%s", code, &out)
			}
			if tc.gate != "" && !strings.Contains(out.String(), tc.gate) {
				t.Fatalf("output = %s, want gate %q", &out, tc.gate)
			}
			after := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
			if after != before {
				t.Fatalf("bookmark moved: %s -> %s", before, after)
			}
		})
	}
}
