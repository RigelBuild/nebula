package main

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSubmitPreflightRefusalsLeaveHistory(t *testing.T) {
	for _, kind := range []string{"nonlinear", "foreign-published", "gate-state", "ci-gate", "changed-template", "adoption-ownership", "adoption-foreign-published", "adoption-ci-gate", "adoption-gate-state", "unreadable", "missing section", "duplicate heading", "compose body", "bounds", "missing What section", "missing Why section", "empty What section", "empty Why section"} {
		t.Run(kind, func(t *testing.T) {
			f := fixtureRepo(t)
			if !slices.Contains([]string{"nonlinear", "foreign-published", "gate-state", "ci-gate", "changed-template", "missing What section", "missing Why section", "empty What section", "empty Why section"}, kind) {
				testAdoptionRefusalLeavesHistory(t, f, kind)
				return
			}
			title := "feat(rjc): exercise preflight refusals"
			bookmark := f.new(t, "EX-1", title, "")
			f.commit(t, "first.txt")
			jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
			switch kind {
			case "nonlinear":
				first := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
				jjTest(t, f.repo, "new", mainOrigin)
				f.commit(t, "other.txt")
				second := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
				jjTest(t, f.repo, "new", first, second)
				f.commit(t, "merge.txt")
				f.commit(t, "after-merge.txt")
				jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
			case "foreign-published":
				interior := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
				f.commit(t, "tip.txt")
				jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
				jjTest(t, f.repo, "new", interior)
				f.commit(t, "sibling.txt")
				jjTest(t, f.repo, "bookmark", "create", "other-lane/ex-2-sibling", "-r", "@-")
				jjTest(t, f.repo, "git", "push", "--bookmark", "other-lane/ex-2-sibling")
				jjTest(t, f.repo, "bookmark", "forget", "other-lane/ex-2-sibling")
				jjTest(t, f.repo, "new", quoteRev(bookmark))
				f.commit(t, "unbookmarked.txt")
			case "gate-state":
				if err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: title, bodyFile: f.bodyFile}, f, io.Discard); err != nil {
					t.Fatal(err)
				}
				md := f.metadata[f.prs[bookmark]]
				md.HeadSHA = strings.Repeat("a", 40)
				f.metadata[f.prs[bookmark]] = md
				f.commit(t, "unbookmarked.txt")
			case "ci-gate":
				body, err := os.ReadFile(f.bodyFile)
				if err != nil {
					t.Fatal(err)
				}
				writeTestFile(t, f.bodyFile, strings.Replace(string(body), "Spec-impact: none", "", 1))
				f.commit(t, "unbookmarked.txt")
			case "changed-template":
				clone := filepath.Join(t.TempDir(), "clone")
				remote := filepath.Join(os.Getenv("HOME"), "remote.git")
				if _, err := f.repo.command(f.ctx, nil, nil, "git", "clone", remote, clone); err != nil {
					t.Fatal(err)
				}
				writeTestFile(t, filepath.Join(clone, ".github/PULL_REQUEST_TEMPLATE.md"), "## What does this do\n\n## Why do we need it\n\n## Summary\n\n## Verification\n\n## Risks\n\n## Additional required section\n")
				if _, err := f.repo.command(f.ctx, nil, nil, "git", "-C", clone, "add", ".github/PULL_REQUEST_TEMPLATE.md"); err != nil {
					t.Fatal(err)
				}
				if _, err := f.repo.command(f.ctx, nil, nil, "git", "-C", clone, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "docs: update template"); err != nil {
					t.Fatal(err)
				}
				if _, err := f.repo.command(f.ctx, nil, nil, "git", "-C", clone, "push", "origin", "main"); err != nil {
					t.Fatal(err)
				}
				f.commit(t, "unbookmarked.txt")
			case "missing What section", "missing Why section", "empty What section", "empty Why section":
				body, err := os.ReadFile(f.bodyFile)
				if err != nil {
					t.Fatal(err)
				}
				replacements := map[string][2]string{
					"missing What section": {"## What does this do\n\nThis fixture submits a feature through the real jj state checks.\n\n## Why do we need it", "## Why do we need it"},
					"missing Why section":  {"## Why do we need it\n\nThe fake remote keeps integration tests offline while exercising submit behavior.\n\n## Summary", "## Summary"},
					"empty What section":   {"## What does this do\n\nThis fixture submits a feature through the real jj state checks.\n\n", "## What does this do\n\n"},
					"empty Why section":    {"## Why do we need it\n\nThe fake remote keeps integration tests offline while exercising submit behavior.\n\n", "## Why do we need it\n\n"},
				}
				replacement := replacements[kind]
				writeTestFile(t, f.bodyFile, strings.Replace(string(body), replacement[0], replacement[1], 1))
				f.commit(t, "unbookmarked.txt")
			}
			before := jjTest(t, f.repo, "log", "--no-graph", "-r", "::@ | bookmarks(exact:"+quoteRev(bookmark)+")", "-T", `commit_id ++ change_id ++ local_bookmarks.filter(|b| b.name() != "main") ++ description`)
			err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: title, bodyFile: f.bodyFile}, f, io.Discard)
			gates := map[string]string{"nonlinear": "nonlinear", "foreign-published": "foreign-published", "gate-state": "ownership", "ci-gate": specImpactGate, "changed-template": "body-template", "missing What section": "body-template", "missing Why section": "body-template", "empty What section": "body-template", "empty Why section": "body-template"}
			if gate, ok := gates[kind]; ok {
				assertRefusalGate(t, err, gate)
			}
			if kind == "nonlinear" && !strings.Contains(err.Error(), "merge") && !strings.Contains(err.Error(), "one root") {
				t.Fatalf("expected merge or one-root refusal: %v", err)
			}
			after := jjTest(t, f.repo, "log", "--no-graph", "-r", "::@ | bookmarks(exact:"+quoteRev(bookmark)+")", "-T", `commit_id ++ change_id ++ local_bookmarks.filter(|b| b.name() != "main") ++ description`)
			if before != after {
				t.Fatalf("%s refusal changed history: %v\nbefore: %s\nafter: %s", kind, err, before, after)
			}
		})
	}
}

func testAdoptionRefusalLeavesHistory(t *testing.T, f *fixture, kind string) {
	t.Helper()
	f.commit(t, "feature.go")
	opt := submitOptions{issue: "EX-1", title: "feat: valid title for adoption", bodyFile: f.bodyFile}
	gate := ""
	switch kind {
	case "adoption-ownership", "adoption-foreign-published":
		interior := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
		f.commit(t, "tip.txt")
		tip := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
		jjTest(t, f.repo, "new", interior)
		f.commit(t, "sibling.txt")
		jjTest(t, f.repo, "bookmark", "create", "other-lane/ex-2-sibling", "-r", "@-")
		if kind == "adoption-foreign-published" {
			jjTest(t, f.repo, "git", "push", "--bookmark", "other-lane/ex-2-sibling")
			jjTest(t, f.repo, "bookmark", "forget", "other-lane/ex-2-sibling")
			gate = "remote-parent"
		} else {
			gate = "foreign-descendant"
		}
		jjTest(t, f.repo, "new", tip)
	case "adoption-ci-gate":
		body, err := os.ReadFile(f.bodyFile)
		if err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, f.bodyFile, strings.Replace(string(body), "Spec-impact: none", "", 1))
		gate = specImpactGate
	case "adoption-gate-state":
		bookmark, err := f.repo.keys.BookmarkName(f.repo.codename, 1, "feat: valid title for adoption (EX-1)")
		if err != nil {
			t.Fatal(err)
		}
		f.prs[bookmark] = 1
		f.metadata[1] = Metadata{HeadRef: bookmark, HeadSHA: strings.Repeat("a", 40), BaseRef: "main"}
		gate = "issue-on-existing-pr"
	case "unreadable":
		opt.bodyFile = filepath.Join(t.TempDir(), "missing")
	case "missing section":
		opt.sections = []namedSection{{title: "Spec-impact", value: "none", replaceOnly: true}}
	case "duplicate heading":
		opt.bodyFile = filepath.Join(t.TempDir(), "duplicate")
		writeTestFile(t, opt.bodyFile, "## Summary\none\n## Summary\ntwo\n")
		opt.sections = []namedSection{{title: "Summary", value: "replacement", replaceOnly: true}}
	case "compose body":
		opt.bodyFile = filepath.Join(t.TempDir(), "incomplete")
		writeTestFile(t, opt.bodyFile, "## What does this do\nSome content.\n## Why do we need it\nA reason.\n## Summary\nSome content.\n")
	case "bounds":
		opt.bodyFile = filepath.Join(t.TempDir(), "short")
		writeTestFile(t, opt.bodyFile, "## What does this do\nBrief.\n## Why do we need it\nA test need.\n## Summary\nBrief.\n## Verification\nTested.\n## Risks\nNone.\n")
	}
	before := jjTest(t, f.repo, "log", "--no-graph", "-r", "all()", "-T", "commit_id ++ change_id ++ bookmarks ++ description")
	err := f.repo.submit(f.ctx, opt, f, io.Discard)
	if err == nil {
		t.Fatal("expected refusal")
	}
	if gate != "" {
		assertRefusalGate(t, err, gate)
	}
	after := jjTest(t, f.repo, "log", "--no-graph", "-r", "all()", "-T", "commit_id ++ change_id ++ bookmarks ++ description")
	if before != after {
		t.Fatalf("refusal changed history: %v", err)
	}
}

func TestSubmitPreflightSelectsTargetWithForeignAuthor(t *testing.T) {
	f := fixtureRepo(t)
	title := "feat(rjc): retain target selection"
	bookmark := f.new(t, "EX-1", title, "")
	jjTest(t, f.repo, "config", "set", "--repo", "user.email", "other@example.com")
	f.commit(t, "feature.txt")
	jjTest(t, f.repo, "config", "set", "--repo", "user.email", "agent@example.com")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	f.commit(t, "tip.txt")
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: title, bodyFile: f.bodyFile, dryRun: true}, f, io.Discard); err != nil {
		t.Fatal(err)
	}
}
