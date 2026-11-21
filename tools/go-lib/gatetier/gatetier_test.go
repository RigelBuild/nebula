package gatetier

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestPlannerTargets(t *testing.T) {
	query := []byte(`{"tasks":{"z":{"format":{"target":"z:format","options":{"runInCI":true}}},"a":{"check":{"target":"a:check","deps":[{"target":"a:lint"}],"options":{"runInCI":true}},"lint":{"target":"a:lint","options":{"runInCI":true}},"verify":{"target":"a:verify","deps":[{"target":"a:middle"}],"options":{"runInCI":true}},"middle":{"target":"a:middle","deps":[{"target":"a:build"}],"options":{"runInCI":true}},"build":{"target":"a:build","type":"build","options":{"runInCI":true}},"release":{"target":"a:release","tags":["cd-only"],"options":{"runInCI":true}},"local":{"target":"a:local","options":{"runInCI":false}}}}}`)
	for _, test := range []struct {
		name string
		tier Tier
		args []string
		want []string
	}{
		{
			name: "fast filters build dependencies tags and local tasks",
			tier: Fast,
			args: []string{"query", "tasks", "--affected", "--downstream", "none"},
			want: []string{"a:check", "a:lint", "z:format"},
		},
		{
			name: "full filters build dependencies tags and local tasks like fast",
			tier: Full,
			args: []string{"query", "tasks", "--affected", "--downstream", "direct"},
			want: []string{"a:check", "a:lint", "z:format"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := func(_ context.Context, env []string, args ...string) ([]byte, error) {
				if reflect.DeepEqual(args, []string{"query", "tasks"}) {
					return query, nil
				}
				if !reflect.DeepEqual(env, []string{"MOON_BASE=base", "MOON_HEAD=head"}) {
					t.Fatalf("env = %v", env)
				}
				if !reflect.DeepEqual(args, test.args) {
					t.Fatalf("args = %v, want %v", args, test.args)
				}
				return query, nil
			}
			got, err := (Planner{Runner: runner}).Targets(t.Context(), test.tier, "base", "head")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("targets = %v, want %v", got, test.want)
			}
		})
	}
}

func TestFastExcludesUnaffectedTransitiveBuild(t *testing.T) {
	affected := []byte(`{"tasks":{"a":{"check":{"target":"a:check","deps":[{"target":"a:middle"}]}}}}`)
	graph := []byte(`{"tasks":{"a":{"check":{"target":"a:check","deps":[{"target":"a:middle"}]},"middle":{"target":"a:middle","deps":[{"target":"a:build"}]},"build":{"target":"a:build","type":"build"}}}}`)
	planner := Planner{Runner: func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		if len(args) == 2 {
			return graph, nil
		}
		return affected, nil
	}}
	targets, err := planner.Targets(t.Context(), Fast, "base", "head")
	if err != nil || len(targets) != 0 {
		t.Fatalf("targets = %v, err = %v; want no targets", targets, err)
	}
}

func TestFastDependencyExclusions(t *testing.T) {
	for _, test := range []struct {
		name  string
		graph string
		want  []string
	}{
		{name: "cd-only dependency", graph: `{"tasks":{"a":{"root":{"target":"a:root","deps":[{"target":"a:middle"}]},"middle":{"target":"a:middle","deps":[{"target":"a:slow"}]},"slow":{"target":"a:slow","tags":["cd-only"]}}}}`, want: []string{}},
		{name: "cycle without build", graph: `{"tasks":{"a":{"root":{"target":"a:root","deps":[{"target":"a:other"}]},"other":{"target":"a:other","deps":[{"target":"a:root"}]}}}}`, want: []string{"a:root"}},
		{name: "cycle with build", graph: `{"tasks":{"a":{"root":{"target":"a:root","deps":[{"target":"a:other"}]},"other":{"target":"a:other","deps":[{"target":"a:root"},{"target":"a:build"}]},"build":{"target":"a:build","type":"build"}}}}`, want: []string{}},
		{name: "dependency not in CI", graph: `{"tasks":{"a":{"root":{"target":"a:root","deps":[{"target":"a:local"}]},"local":{"target":"a:local","options":{"runInCI":false}}}}}`, want: []string{"a:root"}},
		{name: "non-CI dependency through unaffected intermediate", graph: `{"tasks":{"a":{"root":{"target":"a:root","deps":[{"target":"a:middle"}]},"middle":{"target":"a:middle","deps":[{"target":"a:local"}]},"local":{"target":"a:local","options":{"runInCI":false}}}}}`, want: []string{"a:root"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			affected := []byte(`{"tasks":{"a":{"root":{"target":"a:root"}}}}`)
			planner := Planner{Runner: func(_ context.Context, _ []string, args ...string) ([]byte, error) {
				if len(args) == 2 {
					return []byte(test.graph), nil
				}
				return affected, nil
			}}
			got, err := planner.Targets(t.Context(), Fast, "base", "head")
			if err != nil || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("targets = %v, err = %v; want %v", got, err, test.want)
			}
		})
	}
}

// Full tier applies the same cd-only exclusion as fast tier. A cd-only image
// build, and every task that depends on one, is CD work, so a push-to-main gate
// never runs image builds or deploys.
func TestFullExcludesCDOnlyTasks(t *testing.T) {
	query := []byte(`{"tasks":{"a":{"check":{"target":"a:check","deps":[{"target":"a:image"}]},"image":{"target":"a:image","tags":["cd-only"]},"lint":{"target":"a:lint"},"release":{"target":"a:release","tags":["cd-only"]}}}}`)
	planner := Planner{Runner: func(context.Context, []string, ...string) ([]byte, error) {
		return query, nil
	}}
	got, err := planner.Targets(t.Context(), Full, "base", "head")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a:lint"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("targets = %v, want %v", got, want)
	}
}

func TestPlannerTargetsRejectsInvalidTierAndQuery(t *testing.T) {
	for _, test := range []struct {
		name   string
		tier   Tier
		output []byte
		err    error
	}{
		{name: "invalid tier", tier: Tier("other"), err: errors.New("invalid gate tier")},
		{name: "invalid json", tier: Fast, output: []byte("{"), err: errors.New("decode affected Moon tasks")},
		{name: "missing tasks", tier: Fast, output: []byte(`{}`), err: errors.New("has no tasks")},
	} {
		t.Run(test.name, func(t *testing.T) {
			planner := Planner{Runner: func(context.Context, []string, ...string) ([]byte, error) {
				return test.output, nil
			}}
			_, err := planner.Targets(t.Context(), test.tier, "base", "head")
			if err == nil || !strings.Contains(err.Error(), test.err.Error()) {
				t.Fatalf("err = %v, want substring %q", err, test.err)
			}
		})
	}
}

func TestPlannerTargetsWrapsRunnerError(t *testing.T) {
	wantErr := errors.New("moon unavailable")
	planner := Planner{Runner: func(context.Context, []string, ...string) ([]byte, error) {
		return nil, wantErr
	}}
	_, err := planner.Targets(t.Context(), Fast, "base", "head")
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want wrapped runner error", err)
	}
}

// imagePlanner plans one task per project over a fixed graph. The projects cover
// every full-tier image, and svc declares none, so it runs on ci-base.
func imagePlanner() Planner {
	projects := []byte(`{"projects":[` +
		`{"id":"k8s","config":{"project":{"metadata":{"metadata":{"pipeline":{"image":"ci-k8s"}}}}}},` +
		`{"id":"gosvc","config":{"project":{"metadata":{"metadata":{"pipeline":{"image":"ci-go"}}}}}},` +
		`{"id":"rust","config":{"project":{"metadata":{"metadata":{"pipeline":{"image":"ci-rust"}}}}}},` +
		`{"id":"runner-ami-smoke","config":{"project":{"metadata":{"metadata":{"pipeline":{"image":"ci-pulumi"}}}}}},` +
		`{"id":"ci","config":{"project":{"metadata":{"metadata":{"pipeline":{"image":"ci-publish"}}}}}},` +
		`{"id":"svc","config":{"project":{"metadata":{}}}}]}`)
	tasks := []byte(`{"tasks":{"k8s":{"cue":{"target":"k8s:cue"}},"gosvc":{"vuln":{"target":"gosvc:vuln"}},"rust":{"build":{"target":"rust:build"}},"runner-ami-smoke":{"test":{"target":"runner-ami-smoke:test"}},"ci":{"test":{"target":"ci:test"}},"svc":{"test":{"target":"svc:test"}}}}`)
	return Planner{Runner: func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		if reflect.DeepEqual(args, []string{"query", "projects"}) {
			return projects, nil
		}
		return tasks, nil
	}}
}

var allImages = []string{"ci-base", "ci-go", "ci-k8s", "ci-pulumi", "ci-publish", "ci-rust"}

// The image filter selects only tasks whose project declares that image. A
// ci-pulumi or ci-publish task is planned for its own job, never dropped.
func TestPlanFiltersByImage(t *testing.T) {
	planner := imagePlanner()
	for _, test := range []struct {
		image string
		want  []string
	}{
		{image: "ci-base", want: []string{"svc:test"}},
		{image: "ci-k8s", want: []string{"k8s:cue"}},
		{image: "ci-go", want: []string{"gosvc:vuln"}},
		{image: "ci-rust", want: []string{"rust:build"}},
		{image: "ci-pulumi", want: []string{"runner-ami-smoke:test"}},
		{image: "ci-publish", want: []string{"ci:test"}},
	} {
		t.Run(test.image, func(t *testing.T) {
			all, err := planner.PlanAll(t.Context(), Full, "base", "head")
			if err != nil {
				t.Fatal(err)
			}
			if got := targetsOf(OnImage(all, test.image)); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("targets = %v, want %v", got, test.want)
			}
		})
	}
}

// PlanTargets refuses a full-tier plan with a task on an image that has no job,
// with *UncoveredError, and never returns the partial plan. This drives the real
// planTargetsWith path through a fixed moon graph.
func TestPlanTargetsRefusesUncoveredImage(t *testing.T) {
	planner := imagePlanner()
	_, err := planTargetsWith(t.Context(), planner, Full, "ci-base", "base", "head", []string{"ci-base", "ci-go", "ci-k8s", "ci-rust"})
	var uncovered *UncoveredError
	if !errors.As(err, &uncovered) {
		t.Fatalf("err = %v, want *UncoveredError", err)
	}
	if !slices.Contains(uncovered.Targets, "ci:test") || uncovered.Image != "ci-publish" {
		t.Fatalf("UncoveredError = %+v, want ci:test on ci-publish", uncovered)
	}
}

// Every planned task lands in exactly one full-tier job, so the jobs together
// cover the whole plan.
func TestJobsCoverThePlanExactlyOnce(t *testing.T) {
	all, err := imagePlanner().PlanAll(t.Context(), Full, "base", "head")
	if err != nil {
		t.Fatal(err)
	}
	if missing := Uncovered(all, allImages); len(missing) != 0 {
		t.Fatalf("tasks with no job: %v", targetsOf(missing))
	}
	union := make([]string, 0, len(all))
	for _, image := range allImages {
		union = append(union, targetsOf(OnImage(all, image))...)
	}
	want := targetsOf(all)
	slices.Sort(union)
	slices.Sort(want)
	if !reflect.DeepEqual(union, want) {
		t.Fatalf("jobs run %v, plan is %v", union, want)
	}
}

// A task whose image has no job is reported, never dropped.
func TestUncoveredReportsTaskWithoutJob(t *testing.T) {
	all, err := imagePlanner().PlanAll(t.Context(), Full, "base", "head")
	if err != nil {
		t.Fatal(err)
	}
	got := targetsOf(Uncovered(all, []string{"ci-base", "ci-go", "ci-k8s", "ci-rust"}))
	want := []string{"ci:test", "runner-ami-smoke:test"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("uncovered = %v, want %v", got, want)
	}
}

// cdOnlyGraph is the cd-only part of ci/moon.yml. The amd64 build-image leg runs
// in CI. The arm64 leg and the stitch do not. A cd-only deploy depends on a plain check.
const cdOnlyGraph = `{"tasks":{` +
	`"ci":{` +
	`"amd64":{"target":"ci:build-image-ci-publish-amd64","tags":["cd-only"],"deps":[{"target":"publish-ci-image:build"}],"options":{"runInCI":true}},` +
	`"arm64":{"target":"ci:build-image-ci-publish-arm64","tags":["cd-only"],"options":{"runInCI":false}},` +
	`"stitch":{"target":"ci:build-image-ci-publish","tags":["cd-only"],"deps":[{"target":"ci:build-image-ci-publish-amd64"},{"target":"ci:build-image-ci-publish-arm64"}],"options":{"runInCI":false}}},` +
	`"k8s":{` +
	`"cue":{"target":"k8s:cue","options":{"runInCI":true}},` +
	`"deploy":{"target":"k8s:deploy","tags":["cd-only"],"deps":[{"target":"k8s:cue"}],"options":{"runInCI":true}}},` +
	`"rust":{"deploy":{"target":"rust:deploy","tags":["cd-only"],"options":{"runInCI":true}}}}}`

// cdOnlyAffected leaves rust:deploy unaffected.
const cdOnlyAffected = `{"tasks":{` +
	`"ci":{"amd64":{"target":"ci:build-image-ci-publish-amd64"},"arm64":{"target":"ci:build-image-ci-publish-arm64"},"stitch":{"target":"ci:build-image-ci-publish"}},` +
	`"k8s":{"cue":{"target":"k8s:cue"},"deploy":{"target":"k8s:deploy"}}}}`

const cdOnlyProjects = `{"projects":[` +
	`{"id":"ci","config":{"project":{"metadata":{"metadata":{"pipeline":{"image":"ci-publish"}}}}}},` +
	`{"id":"k8s","config":{"project":{"metadata":{"metadata":{"pipeline":{"image":"ci-k8s"}}}}}},` +
	`{"id":"rust","config":{"project":{"metadata":{"metadata":{"pipeline":{"image":"ci-rust"}}}}}}]}`

// cdOnlyRunner answers the moon queries the cd-only tier makes. It fails the test
// on any other call. The affected query is the deep walk, the same one the push
// plan uses, so a cd-only task affected only through a changed dependency is found.
func cdOnlyRunner(t *testing.T) Runner {
	t.Helper()
	return func(_ context.Context, env []string, args ...string) ([]byte, error) {
		switch {
		case reflect.DeepEqual(args, []string{"query", "projects"}):
			return []byte(cdOnlyProjects), nil
		case reflect.DeepEqual(args, []string{"query", "tasks"}):
			return []byte(cdOnlyGraph), nil
		case reflect.DeepEqual(args, []string{"query", "tasks", "--affected", "--downstream", "deep"}):
			if !reflect.DeepEqual(env, []string{"MOON_BASE=base", "MOON_HEAD=head"}) {
				t.Fatalf("env = %v", env)
			}
			return []byte(cdOnlyAffected), nil
		}
		t.Fatalf("unexpected moon call %v", args)
		return nil, nil
	}
}

// The cd-only tier runs the affected cd-only targets that run in CI, with their
// transitive dependencies. Unlike Fast and Full, it does not drop cd-only tasks.
func TestCDOnlyPlansAffectedCIRunTargets(t *testing.T) {
	got, err := (Planner{Runner: cdOnlyRunner(t)}).Plan(t.Context(), CDOnly, "base", "head")
	if err != nil {
		t.Fatal(err)
	}
	want := []Task{
		{Target: "ci:build-image-ci-publish-amd64", Deps: []string{"publish-ci-image:build"}},
		{Target: "k8s:deploy", Deps: []string{"k8s:cue"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tasks = %+v, want %+v", got, want)
	}
}

// With no --image, the cd-only tier plans every image. The ci-base default would
// drop the build-image targets, which declare ci-publish.
func TestCDOnlyWithoutImagePlansEveryImage(t *testing.T) {
	got, err := planTargetsWith(t.Context(), Planner{Runner: cdOnlyRunner(t)}, CDOnly, "", "base", "head", nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"ci:build-image-ci-publish-amd64", "k8s:deploy"}; !slices.Equal(got, want) {
		t.Fatalf("targets = %v, want %v", got, want)
	}
}

// --image keeps its meaning on the cd-only tier: it selects by project image.
func TestCDOnlyImageFilterKeepsMeaning(t *testing.T) {
	for _, test := range []struct {
		image string
		want  []string
	}{
		{image: "ci-publish", want: []string{"ci:build-image-ci-publish-amd64"}},
		{image: "ci-k8s", want: []string{"k8s:deploy"}},
		{image: "ci-rust", want: nil},
	} {
		t.Run(test.image, func(t *testing.T) {
			got, err := planTargetsWith(t.Context(), Planner{Runner: cdOnlyRunner(t)}, CDOnly, test.image, "base", "head", nil)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, test.want) {
				t.Fatalf("targets = %v, want %v", got, test.want)
			}
		})
	}
}

// A cd-only task is affected only through a changed dependency: its own inputs do
// not change, but pub:build does. moon reports it only under the deep walk, so the
// cd-only tier must query deep. Under none it plans nothing and reads green.
func TestCDOnlyPlansTaskAffectedThroughDependency(t *testing.T) {
	graph := `{"tasks":{"pub":{"build":{"target":"pub:build"}},"ci":{"image":{"target":"ci:image","tags":["cd-only"],"deps":[{"target":"pub:build"}],"options":{"runInCI":true}}}}}`
	affected := map[string]string{
		"deep": `{"tasks":{"pub":{"build":{"target":"pub:build"}},"ci":{"image":{"target":"ci:image"}}}}`,
		"none": `{"tasks":{"pub":{"build":{"target":"pub:build"}}}}`,
	}
	planner := Planner{Runner: func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		if reflect.DeepEqual(args, []string{"query", "tasks"}) {
			return []byte(graph), nil
		}
		if len(args) == 5 && args[0] == "query" && args[3] == "--downstream" {
			return []byte(affected[args[4]]), nil
		}
		t.Fatalf("unexpected moon call %v", args)
		return nil, nil
	}}
	got, err := planner.Targets(t.Context(), CDOnly, "base", "head")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"ci:image"}; !slices.Equal(got, want) {
		t.Fatalf("targets = %v, want %v", got, want)
	}
}

// AffectedTargets keeps runInCI:false deploy targets, which PlanTargets drops.
// The fixture holds an affected runInCI:false deploy target (aws:up), an
// affected runInCI:true target (cloudflare:up), and a target not in known.
func TestAffectedTargetsKeepsRunInCIFalseDeployTargets(t *testing.T) {
	query := []byte(`{"tasks":{"aws":{"up":{"target":"aws:up","tags":["cd-only"],"options":{"runInCI":false}}},"cloudflare":{"up":{"target":"cloudflare:up","tags":["cd-only"],"options":{"runInCI":true}}},"lint":{"check":{"target":"lint:check","options":{"runInCI":true}}}}}`)
	run := func(_ context.Context, env []string, args ...string) ([]byte, error) {
		if !slices.Contains(env, "CI=") {
			t.Fatalf("env = %v, want CI= so the query runs outside CI", env)
		}
		if !reflect.DeepEqual(args, []string{"query", "tasks", "--affected", "--downstream", "deep"}) {
			t.Fatalf("args = %v", args)
		}
		return query, nil
	}
	got, err := affectedTargetsWith(t.Context(), run, "base", "head", []string{"aws:up", "cloudflare:up"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"aws:up", "cloudflare:up"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("AffectedTargets = %v, want %v", got, want)
	}
}

// AffectedTargets returns only the known targets.
func TestAffectedTargetsFiltersToKnown(t *testing.T) {
	query := []byte(`{"tasks":{"aws":{"up":{"target":"aws:up","tags":["cd-only"],"options":{"runInCI":false}}},"x":{"y":{"target":"x:y","tags":["cd-only"],"options":{"runInCI":true}}}}}`)
	run := func(_ context.Context, _ []string, _ ...string) ([]byte, error) { return query, nil }
	got, err := affectedTargetsWith(t.Context(), run, "base", "head", []string{"aws:up"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"aws:up"}) {
		t.Fatalf("AffectedTargets = %v, want [aws:up]", got)
	}
}
