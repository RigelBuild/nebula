package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSkipAdmissionHistory(t *testing.T) {
	f := fixtureRepo(t)
	stubIssueState(t, f)
	f.repo.profile.HookReports = true
	bookmark := f.new(t, "EX-1", "feat(rjc): add skip test", "")
	f.commit(t, "skip.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	tip := jjTest(t, f.repo, "log", "--no-graph", "-r", bookmark, "-T", "commit_id")
	line := []Change{{Bookmark: bookmark, Tip: tip, Base: "main@origin", Owned: true}}
	store := localPrePush{root: f.repo.stateDir}
	id, err := f.repo.hookIdentity(f.ctx, line[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := f.repo.checkSkip(f.ctx, line, "main-red:#7", store); err == nil || !strings.Contains(err.Error(), "skip-evidence") {
		t.Fatalf("missing evidence: %v", err)
	}
	id.Result = hookFail
	id.FailedTargets = []string{"a:test"}
	id.FailedGates = []string{"gate-a"}
	id.Run = "failed-run"
	if err := store.Save(id); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.checkSkip(f.ctx, line, "main-red:#7", store); err != nil {
		t.Fatal(err)
	}
	rec, found, err := store.Read(f.ctx, id.Repo, 0, id.Head)
	if err != nil || !found || rec.Result != "skipped" || rec.Skip != "main-red:#7" || rec.FailedRun != id.Run || !slices.Equal(rec.FailedTargets, []string{"a:test"}) || !slices.Equal(rec.FailedGates, []string{"gate-a"}) {
		t.Fatalf("skipped evidence: %+v %v", rec, err)
	}
	a := SkippedTarget{Target: "a:test", Reason: "EX-7"}
	if err := f.repo.checkSkipTargets(f.ctx, line, []SkippedTarget{a}, store); err != nil {
		t.Fatal(err)
	}
	b := SkippedTarget{Target: "a:other", Reason: "EX-7"}
	if err := f.repo.checkSkipTargets(f.ctx, line, []SkippedTarget{b}, store); err == nil || !strings.Contains(err.Error(), "skip-target-not-failed") {
		t.Fatalf("unproven target: %v", err)
	}
	t.Setenv("RJC_TEST_HOOK_FAIL", "1")
	t.Setenv("RJC_TEST_FAILED_TARGET", "a:other")
	if err := f.repo.runHooks(f.ctx, line, []SkippedTarget{a}, localHookRunner{repo: f.repo}, store); err == nil || !strings.Contains(err.Error(), "hooks-failed") {
		t.Fatalf("selective failure: %v", err)
	}
	if err := f.repo.checkSkipTargets(f.ctx, line, []SkippedTarget{a, b}, store); err != nil {
		t.Fatal(err)
	}
}

func TestP0SkipRequiresUrgentIssue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		priority  int
		state     string
		found     bool
		lookupErr error
		accept    bool
	}{
		{name: "urgent without failed run", priority: 1, state: "started", found: true, accept: true},
		{name: "high priority", priority: 2, state: "started", found: true},
		{name: "completed issue", priority: 1, state: "completed", found: true},
		{name: "canceled issue", priority: 1, state: "canceled", found: true},
		{name: "unknown issue", priority: 0},
		{name: "lookup error", found: true, lookupErr: errors.New("linear unavailable")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := parallelFixtureRepo(t)
			bookmark := f.new(t, "EX-1", "feat(rjc): p0 skip test", "")
			f.commit(t, "skip.txt")
			jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
			tip := jjTest(t, f.repo, "log", "--no-graph", "-r", bookmark, "-T", "commit_id")
			line := []Change{{Bookmark: bookmark, Tip: tip, Base: "main@origin", Owned: true}}
			f.repo.lookupLinearPriority = func(_ context.Context, issue string) (int, string, bool, error) {
				if issue != "EX-7" {
					t.Fatalf("looked up %q, want EX-7", issue)
				}
				return tt.priority, tt.state, tt.found, tt.lookupErr
			}
			store := localPrePush{root: f.repo.stateDir}
			err := f.repo.checkSkip(f.ctx, line, "p0:EX-7", store)
			if tt.lookupErr != nil && !errors.Is(err, tt.lookupErr) {
				t.Fatalf("lookup error cause was not retained: %v", err)
			}
			if tt.accept {
				if err != nil {
					t.Fatal(err)
				}
				rec, found, readErr := store.Read(f.ctx, f.repo.profile.Name, 0, tip)
				if readErr != nil || !found || rec.Result != hookSkipped || rec.Skip != "p0:EX-7" || rec.FailedRun != "" || len(rec.FailedTargets) != 0 || len(rec.FailedGates) != 0 {
					t.Fatalf("P0 skip record: %+v found=%v err=%v", rec, found, readErr)
				}
				if err := f.repo.checkReadyHooks(f.ctx, store, rec, false); err != nil {
					t.Fatalf("open Urgent P0 skip failed ready recheck: %v", err)
				}
				return
			}
			var refusal *failure
			if !errors.As(err, &refusal) || refusal.stage != "hooks" || refusal.gate != "skip-reason" {
				t.Fatalf("P0 skip refusal stage/gate = %v, want hooks/skip-reason", err)
			}
			if err == nil || !strings.Contains(err.Error(), "skip-reason") {
				t.Fatalf("P0 skip was not refused at skip-reason: %v", err)
			}
			if _, found, readErr := store.Read(f.ctx, f.repo.profile.Name, 0, tip); readErr != nil || found {
				t.Fatalf("refused P0 skip saved a record: found=%v err=%v", found, readErr)
			}
		})
	}
}

func TestP0SkipReadyRechecksUrgentIssue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		priority  int
		state     string
		found     bool
		lookupErr error
		want      string
	}{
		{name: "issue closed", priority: 1, state: "completed", found: true, want: "P0 issue is closed"},
		{name: "issue non-Urgent", priority: 2, state: "started", found: true, want: "P0 skip requires an Urgent issue"},
		{name: "lookup error", found: true, lookupErr: errors.New("linear unavailable"), want: "cannot verify issue priority"},
		{name: "unknown issue", priority: 0, found: false, want: "P0 issue does not exist or has no priority/state"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := parallelFixtureRepo(t)
			bookmark := f.new(t, "EX-1", "feat(rjc): p0 ready recheck", "")
			f.commit(t, "skip.txt")
			jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
			tip := jjTest(t, f.repo, "log", "--no-graph", "-r", bookmark, "-T", "commit_id")
			line := []Change{{Bookmark: bookmark, Tip: tip, Base: "main@origin", Owned: true}}
			lookups := 0
			f.repo.lookupLinearPriority = func(_ context.Context, issue string) (int, string, bool, error) {
				if issue != "EX-7" {
					t.Fatalf("looked up %q, want EX-7", issue)
				}
				lookups++
				if lookups == 1 {
					return 1, "started", true, nil
				}
				return tt.priority, tt.state, tt.found, tt.lookupErr
			}
			store := localPrePush{root: f.repo.stateDir}
			if err := f.repo.checkSkip(f.ctx, line, "p0:EX-7", store); err != nil {
				t.Fatalf("initial open Urgent P0 skip: %v", err)
			}
			rec, found, err := store.Read(f.ctx, f.repo.profile.Name, 0, tip)
			if err != nil || !found {
				t.Fatalf("P0 skip record: found=%v err=%v", found, err)
			}
			err = f.repo.checkReadyHooks(f.ctx, store, rec, false)
			var refusal *failure
			if !errors.As(err, &refusal) || refusal.stage != "ready" || refusal.gate != "hooks-skipped" || !strings.Contains(refusal.reason, tt.want) {
				t.Fatalf("ready refusal = %v, want ready/hooks-skipped containing %q", err, tt.want)
			}
			if tt.lookupErr != nil && !errors.Is(err, tt.lookupErr) {
				t.Fatalf("ready lookup error cause was not retained: %v", err)
			}
			if lookups != 2 {
				t.Fatalf("Linear lookups = %d, want 2", lookups)
			}
		})
	}
}

func TestLinearIssuePriorityCLI(t *testing.T) {
	tests := []struct {
		name        string
		response    string
		stderr      bool
		stdoutError bool
		accept      bool
	}{
		{name: "urgent open issue", response: `{"data":{"issue":{"priority":1,"state":{"type":"started"}}}}`, accept: true},
		{name: "no priority", response: `{"data":{"issue":{"priority":0,"state":{"type":"started"}}}}`},
		{name: "high priority", response: `{"data":{"issue":{"priority":2,"state":{"type":"started"}}}}`},
		{name: "Linear CLI failure", response: "issue unavailable", stderr: true},
		{name: "Linear GraphQL error on stdout", response: `{"errors":[{"message":"Entity not found: Issue"}]}`, stdoutError: true},
		{name: "null issue", response: `{"data":{"issue":null}}`},
		{name: "missing state", response: `{"data":{"issue":{"priority":1}}}`},
		{name: "null priority", response: `{"data":{"issue":{"priority":null,"state":{"type":"started"}}}}`},
		{name: "non-JSON response", response: `not json`},
		{name: "completed issue", response: `{"data":{"issue":{"priority":1,"state":{"type":"completed"}}}}`},
		{name: "canceled issue", response: `{"data":{"issue":{"priority":1,"state":{"type":"canceled"}}}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := fixtureRepo(t)
			bin := t.TempDir()
			linear := "#!/bin/sh\n[ \"$1\" = api ] || exit 90\ncase \"$2\" in *'issue(id:\"EX-7\")'*) ;; *) exit 91 ;; esac\n"
			printf := "printf '%s\\n' '" + tt.response + "'"
			switch {
			case tt.stderr:
				linear += printf + " >&2\nexit 1\n"
			case tt.stdoutError:
				linear += printf + "\nexit 1\n"
			default:
				linear += printf + "\n"
			}
			path := filepath.Join(bin, "linear")
			if err := os.WriteFile(path, []byte(linear), 0o700); err != nil {
				t.Fatal(err)
			}
			f.repo.env = []string{"PATH=" + bin}
			err := f.repo.checkP0Priority(f.ctx, "EX-7", "hooks", "skip-reason")
			if tt.accept && err != nil {
				t.Fatalf("Urgent issue refused: %v", err)
			}
			if !tt.accept && (err == nil || !strings.Contains(err.Error(), "skip-reason")) {
				t.Fatalf("invalid P0 issue accepted or misclassified: %v", err)
			}
			if tt.stderr && !strings.Contains(err.Error(), "issue unavailable") {
				t.Fatalf("Linear CLI error was hidden: %v", err)
			}
			if tt.stdoutError && !strings.Contains(err.Error(), "Entity not found: Issue") {
				t.Fatalf("Linear GraphQL error from stdout was hidden: %v", err)
			}
		})
	}
}

func TestP0SkipMissingLinearCLIRefuses(t *testing.T) {
	f := fixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): p0 skip test", "")
	f.commit(t, "skip.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	tip := jjTest(t, f.repo, "log", "--no-graph", "-r", bookmark, "-T", "commit_id")
	line := []Change{{Bookmark: bookmark, Tip: tip, Base: "main@origin", Owned: true}}
	path := t.TempDir()
	f.repo.env = []string{"PATH=" + path}
	err := f.repo.checkSkip(f.ctx, line, "p0:EX-7", localPrePush{root: f.repo.stateDir})
	var failure *failure
	if !errors.As(err, &failure) || failure.gate != "skip-reason" || !strings.Contains(failure.fix, "linear") {
		t.Fatalf("missing Linear CLI was not refused with a fix hint: %v", err)
	}
}

func TestP0SkipOptionValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		skip    string
		wantErr bool
	}{
		{name: "valid issue", skip: "p0:EX-1"},
		{name: "zero issue", skip: "p0:EX-0", wantErr: true},
		{name: "malformed issue", skip: "p0:foo", wantErr: true},
		{name: "trailing argument", skip: "p0:EX-1 x", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSubmitOptions(submitOptions{skip: tc.skip})
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateSubmitOptions(%q) error = %v, wantErr %v", tc.skip, err, tc.wantErr)
			}
		})
	}
}
func stubIssueState(t *testing.T, f *fixture) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if strings.Contains(request.URL.Path, "/issues/") {
			respondJSON(t, w, map[string]string{"state": "open"})
			return
		}
		f.serve(t, w, request)
	}))
	t.Cleanup(server.Close)
	s, err := service(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.forge = &forgeClient{client: server.Client(), baseURL: server.URL}
}

func TestSkipReasonGrammar(t *testing.T) {
	for _, tc := range []struct {
		name    string
		skip    string
		wantErr bool
	}{
		{name: "empty", skip: "", wantErr: true},
		{name: "blank", skip: "   ", wantErr: true},
		{name: "multiline", skip: "flaky\nrunner", wantErr: true},
		{name: "tab", skip: "flaky\trunner", wantErr: true},
		{name: "overlong", skip: strings.Repeat("x", 201), wantErr: true},
		{name: "longest", skip: strings.Repeat("x", 200)},
		{name: "free text", skip: "flaky runner"},
		{name: "padded free text", skip: "  flaky runner  "},
		{name: "artifact form is free text", skip: "artifact:EX-1"},
		{name: "main-red", skip: "main-red:#1"},
		{name: "main-red without ref", skip: "main-red:", wantErr: true},
		{name: "p0 issue", skip: "p0:EX-1"},
		{name: "p0 with trailing text", skip: "p0: urgent", wantErr: true},
		{name: "bidirectional override", skip: "flaky\u202Erunner", wantErr: true},
		{name: "Unicode line separator", skip: "flaky\u2028runner", wantErr: true},
		{name: "stack start marker", skip: "x " + stackStart, wantErr: true},
		{name: "stack end marker", skip: stackEnd, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSubmitOptions(submitOptions{skip: tc.skip, skipGiven: true})
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateSubmitOptions(%q) error = %v, wantErr %v", tc.skip, err, tc.wantErr)
			}
		})
	}
}

func TestFreeTextSkipNeedsNoFailedRun(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{"flaky runner", "artifact:EX-1"} {
		t.Run(reason, func(t *testing.T) {
			f := parallelFixtureRepo(t)
			bookmark := f.new(t, "EX-1", "feat(rjc): free text skip", "")
			f.commit(t, "skip.txt")
			jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
			tip := jjTest(t, f.repo, "log", "--no-graph", "-r", bookmark, "-T", "commit_id")
			line := []Change{{Bookmark: bookmark, Tip: tip, Base: "main@origin", Owned: true}}
			store := localPrePush{root: f.repo.stateDir}
			if err := f.repo.checkSkip(f.ctx, line, reason, store); err != nil {
				t.Fatal(err)
			}
			rec, found, err := store.Read(f.ctx, f.repo.profile.Name, 0, tip)
			if err != nil || !found || rec.Result != hookSkipped || rec.Skip != reason || rec.FailedRun != "" {
				t.Fatalf("free-text skip record: %+v found=%v err=%v", rec, found, err)
			}
			if err := f.repo.checkReadyHooks(f.ctx, store, rec, false); err != nil {
				t.Fatalf("free-text skip refused plain ready: %v", err)
			}
			err = f.repo.checkReadyHooks(f.ctx, store, rec, true)
			if err == nil || !strings.Contains(err.Error(), "hooks-skipped") {
				t.Fatalf("free-text skip reached main-red recheck: %v", err)
			}
		})
	}
}

func TestSkipOnReadyPRBypassRules(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		skip    string
		targets []SkippedTarget
		refused bool
	}{
		{name: "free-text keeps PR ready", skip: "flaky runner"},
		{name: "p0 keeps PR ready", skip: "p0:EX-7"},
		{name: "main-red refused", skip: "main-red:#1", refused: true},
		{name: "skip-target refused", targets: []SkippedTarget{{Target: "a:test", Reason: "EX-1"}}, refused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := parallelFixtureRepo(t)
			bookmark := f.new(t, "EX-1", "feat(rjc): ready PR bypass", "")
			f.commit(t, "feature.txt")
			opt := submitOptions{bookmark: bookmark, title: "feat(rjc): ready PR bypass", bodyFile: f.bodyFile}
			if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
				t.Fatal(err)
			}
			number := f.prs[bookmark]
			meta := f.metadata[number]
			meta.Draft = false
			f.metadata[number] = meta
			f.repo.lookupLinearPriority = func(context.Context, string) (int, string, bool, error) {
				return 1, "started", true, nil
			}
			f.repo.rootChecks = func(context.Context, string, []string) (rootChecksResult, error) { return rootChecksResult{}, nil }
			opt.skip, opt.skippedTargets = tc.skip, tc.targets
			err := f.repo.submit(f.ctx, opt, f, io.Discard)
			if tc.refused {
				if err == nil || !strings.Contains(err.Error(), "skip-on-ready-pr") {
					t.Fatalf("ready PR bypass not refused: %v", err)
				}
				var detail *failure
				if !errors.As(err, &detail) || !strings.Contains(detail.fix, "gh pr ready --undo 1 -R example/app") || !strings.Contains(detail.fix, "draft: true via GitHub MCP") {
					t.Fatalf("missing draft command: %#v", detail)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if f.metadata[number].Draft {
				t.Fatal("bypass re-drafted a ready PR")
			}
		})
	}
}

func TestSkipCoversUpperHeadsAboveFailedLower(t *testing.T) {
	f := fixtureRepo(t)
	stubIssueState(t, f)
	gateModuleFixture(t, f)
	lower := f.new(t, "EX-1", "feat(rjc): lower failed skip", "")
	f.commit(t, "lower.txt")
	jjTest(t, f.repo, "bookmark", "move", lower, "--to", "@-")
	f.commit(t, "upper.txt")
	upper := "test-lane/upper-skip"
	jjTest(t, f.repo, "bookmark", "create", upper, "-r", "@-")
	lowerTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(lower), "-T", "commit_id")
	upperTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(upper), "-T", "commit_id")
	lowerChange := Change{Bookmark: lower, Tip: lowerTip, Base: mainOrigin, Owned: true}
	upperChange := Change{Bookmark: upper, Tip: upperTip, Base: lower, Owned: true}
	store := localPrePush{root: f.repo.stateDir}
	id, err := f.repo.hookIdentity(f.ctx, lowerChange)
	if err != nil {
		t.Fatal(err)
	}
	id.Result = hookFail
	id.Run = "failed-run"
	id.FailedTargets = []string{"a:test"}
	id.FailedGates = []string{"gate-a"}
	if err := store.Save(id); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.checkSkip(f.ctx, []Change{lowerChange, upperChange}, "main-red:#7", store); err != nil {
		t.Fatalf("skip refused above failed lower head: %v", err)
	}
	rec, found, err := store.Read(f.ctx, f.repo.profile.Name, 0, upperTip)
	if err != nil || !found || rec.Result != hookSkipped || rec.Skip != "main-red:#7" || rec.FailedRun != "failed-run" || !slices.Equal(rec.FailedGates, []string{"gate-a"}) {
		t.Fatalf("upper skip record: %+v found=%v err=%v", rec, found, err)
	}
	history, err := store.History(f.repo.profile.Name, upperTip)
	if err != nil || len(history) != 0 {
		t.Fatalf("upper head got a failed run it never had: %+v err=%v", history, err)
	}
}

func TestSkipRefusesUpperHeadWhenLowerMovedPastFailure(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	stubIssueState(t, f)
	lower := f.new(t, "EX-1", "feat(rjc): moved lower failure", "")
	f.commit(t, "lower.txt")
	jjTest(t, f.repo, "bookmark", "move", lower, "--to", "@-")
	failedTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(lower), "-T", "commit_id")
	store := localPrePush{root: f.repo.stateDir}
	id, err := f.repo.hookIdentity(f.ctx, Change{Bookmark: lower, Tip: failedTip, Base: mainOrigin, Owned: true})
	if err != nil {
		t.Fatal(err)
	}
	id.Result = hookFail
	id.Run = "failed-run"
	if err := store.Save(id); err != nil {
		t.Fatal(err)
	}
	f.commit(t, "lower-next.txt")
	jjTest(t, f.repo, "bookmark", "move", lower, "--to", "@-")
	f.commit(t, "upper.txt")
	upper := "test-lane/upper-moved"
	jjTest(t, f.repo, "bookmark", "create", upper, "-r", "@-")
	upperTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(upper), "-T", "commit_id")
	// The lower bookmark moved past the failed head, so the failure no longer names its current head.
	lowerChange := Change{Bookmark: lower, Tip: failedTip, Base: mainOrigin, Owned: true}
	upperChange := Change{Bookmark: upper, Tip: upperTip, Base: lower, Owned: true}
	err = f.repo.checkSkip(f.ctx, []Change{lowerChange, upperChange}, "main-red:#7", store)
	if err == nil || !strings.Contains(err.Error(), "skip-evidence") {
		t.Fatalf("upper head covered by a failure on a moved lower bookmark: %v", err)
	}
}

func TestSkipRefusesUpperHeadNotDescendingFromFailedTip(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	stubIssueState(t, f)
	lower := f.new(t, "EX-1", "feat(rjc): failed lower tip", "")
	f.commit(t, "lower.txt")
	jjTest(t, f.repo, "bookmark", "move", lower, "--to", "@-")
	lowerTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(lower), "-T", "commit_id")
	// A sibling of the lower head: same parent, not a descendant of lowerTip.
	jjTest(t, f.repo, "new", mainOrigin)
	f.commit(t, "sibling.txt")
	upper := "test-lane/sibling-upper"
	jjTest(t, f.repo, "bookmark", "create", upper, "-r", "@-")
	upperTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(upper), "-T", "commit_id")
	store := localPrePush{root: f.repo.stateDir}
	lowerChange := Change{Bookmark: lower, Tip: lowerTip, Base: mainOrigin, Owned: true}
	upperChange := Change{Bookmark: upper, Tip: upperTip, Base: mainOrigin, Owned: true}
	id, err := f.repo.hookIdentity(f.ctx, lowerChange)
	if err != nil {
		t.Fatal(err)
	}
	id.Result, id.Run = hookFail, "failed-run"
	id.FailedTargets = []string{"a:test"}
	if err := store.Save(id); err != nil {
		t.Fatal(err)
	}
	err = f.repo.checkSkip(f.ctx, []Change{lowerChange, upperChange}, "main-red:#7", store)
	if err == nil || !strings.Contains(err.Error(), "skip-evidence") {
		t.Fatalf("upper head that does not descend from the failed tip was covered: %v", err)
	}
	if _, found, readErr := store.Read(f.ctx, f.repo.profile.Name, 0, upperTip); readErr != nil || found {
		t.Fatalf("refused upper head saved a record: found=%v err=%v", found, readErr)
	}
}

func TestSkipCoverUsesReviewFixParentTip(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	stubIssueState(t, f)
	parent := f.new(t, "EX-1", "feat(rjc): review fix parent", "")
	f.commit(t, "parent.txt")
	jjTest(t, f.repo, "bookmark", "move", parent, "--to", "@-")
	failedTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(parent), "-T", "commit_id")
	store := localPrePush{root: f.repo.stateDir}
	id, err := f.repo.hookIdentity(f.ctx, Change{Bookmark: parent, Tip: failedTip, Base: mainOrigin, Owned: true})
	if err != nil {
		t.Fatal(err)
	}
	id.Result, id.Run = hookFail, "failed-run"
	id.FailedTargets = []string{"a:test"}
	if err := store.Save(id); err != nil {
		t.Fatal(err)
	}
	// The review fix is a new commit on the parent. The child sits above the fix, not above the failed tip.
	f.commit(t, "fix.txt")
	fix := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	f.commit(t, "child.txt")
	child := "test-lane/review-child"
	jjTest(t, f.repo, "bookmark", "create", child, "-r", "@-")
	childTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(child), "-T", "commit_id")
	plan := &reviewFixPlan{parent: parent, fix: fix, line: []Change{
		{Bookmark: parent, Tip: failedTip, Base: mainOrigin, Owned: true},
		{Bookmark: child, Tip: childTip, Base: parent, Owned: true},
	}}
	// submitHookLine swaps the fix in as the parent's Tip. The parent is then a new head with no failure,
	// so the stale failure at failedTip must not cover the child.
	line, err := f.repo.submitHookLine(f.ctx, child, "", "", plan)
	if err != nil {
		t.Fatal(err)
	}
	if line[0].Tip != fix {
		t.Fatalf("submitHookLine did not swap the parent to the fix tip: %s", line[0].Tip)
	}
	err = f.repo.checkSkip(f.ctx, line, "main-red:#7", store)
	if err == nil || !strings.Contains(err.Error(), "skip-evidence") {
		t.Fatalf("parent at the fix tip was covered by the stale failure: %v", err)
	}
	if _, found, readErr := store.Read(f.ctx, f.repo.profile.Name, 0, fix); readErr != nil || found {
		t.Fatalf("refused fix head saved a record: found=%v err=%v", found, readErr)
	}
}

func TestSkipRefusesUpperHeadWithDifferentHookConfig(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	stubIssueState(t, f)
	lower := f.new(t, "EX-1", "feat(rjc): hook config mismatch", "")
	f.commit(t, "lower.txt")
	jjTest(t, f.repo, "bookmark", "move", lower, "--to", "@-")
	lowerTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(lower), "-T", "commit_id")
	f.commit(t, "upper.txt")
	writeTestFile(t, filepath.Join(f.repo.root, hkConfigName), "amends \"hk\"\n")
	jjTest(t, f.repo, "commit", "-m", "fix: upper adds hook config")
	jjTest(t, f.repo, "bookmark", "create", "test-lane/upper-config", "-r", "@-")
	upperTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev("test-lane/upper-config"), "-T", "commit_id")
	store := localPrePush{root: f.repo.stateDir}
	lowerChange := Change{Bookmark: lower, Tip: lowerTip, Base: mainOrigin, Owned: true}
	upperChange := Change{Bookmark: "test-lane/upper-config", Tip: upperTip, Base: lower, Owned: true}
	id, err := f.repo.hookIdentity(f.ctx, lowerChange)
	if err != nil {
		t.Fatal(err)
	}
	id.Result, id.Run = hookFail, "failed-run"
	id.FailedTargets = []string{"a:test"}
	if err := store.Save(id); err != nil {
		t.Fatal(err)
	}
	upperID, err := f.repo.hookIdentity(f.ctx, upperChange)
	if err != nil {
		t.Fatal(err)
	}
	if upperID.HookConfigBlob == "" || upperID.HookConfigBlob == id.HookConfigBlob {
		t.Fatalf("test setup: upper blob %q must differ from lower blob %q", upperID.HookConfigBlob, id.HookConfigBlob)
	}
	err = f.repo.checkSkip(f.ctx, []Change{lowerChange, upperChange}, "main-red:#7", store)
	if err == nil || !strings.Contains(err.Error(), "skip-evidence") || !strings.Contains(err.Error(), "hook config") {
		t.Fatalf("hook-config mismatch was not refused: %v", err)
	}
}

func TestSkipKeepsUpperHeadOwnFailedRun(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	stubIssueState(t, f)
	lower := f.new(t, "EX-1", "feat(rjc): upper own run kept", "")
	f.commit(t, "lower.txt")
	jjTest(t, f.repo, "bookmark", "move", lower, "--to", "@-")
	lowerTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(lower), "-T", "commit_id")
	f.commit(t, "upper.txt")
	upper := "test-lane/upper-own-run"
	jjTest(t, f.repo, "bookmark", "create", upper, "-r", "@-")
	upperTip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(upper), "-T", "commit_id")
	store := localPrePush{root: f.repo.stateDir}
	lowerChange := Change{Bookmark: lower, Tip: lowerTip, Base: mainOrigin, Owned: true}
	upperChange := Change{Bookmark: upper, Tip: upperTip, Base: lower, Owned: true}
	lowerID, err := f.repo.hookIdentity(f.ctx, lowerChange)
	if err != nil {
		t.Fatal(err)
	}
	lowerID.Result, lowerID.Run = hookFail, "lower-run"
	lowerID.FailedTargets = []string{"a:test"}
	if err := store.Save(lowerID); err != nil {
		t.Fatal(err)
	}
	// The upper head ran on its own and failed differently. Its own result must be used, not the lower's.
	upperID, err := f.repo.hookIdentity(f.ctx, upperChange)
	if err != nil {
		t.Fatal(err)
	}
	upperID.Result, upperID.Run = hookFail, "upper-run"
	upperID.FailedTargets = []string{"c:real"}
	if err := store.Save(upperID); err != nil {
		t.Fatal(err)
	}
	err = f.repo.checkSkip(f.ctx, []Change{lowerChange, upperChange}, "main-red:#7", store)
	if err != nil {
		t.Fatalf("skip refused for head with own failed run: %v", err)
	}
	rec, found, err := store.Read(f.ctx, f.repo.profile.Name, 0, upperTip)
	if err != nil || !found {
		t.Fatalf("upper skip record: found=%v err=%v", found, err)
	}
	if rec.FailedRun != "upper-run" || !slices.Equal(rec.FailedTargets, []string{"c:real"}) || rec.CoveredBy != "" {
		t.Fatalf("upper head did not keep its own failed run: %+v", rec)
	}
}
