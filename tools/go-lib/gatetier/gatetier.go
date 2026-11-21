// SPDX-License-Identifier: MIT OR Apache-2.0

package gatetier

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec" //nolint:depguard // Moon is invoked with an argv vector, never a shell.
	"slices"
	"strings"
)

// Tier selects the breadth of affected Moon tasks to run.
type Tier string

const (
	Fast   Tier = "fast"
	Full   Tier = "full"
	CDOnly Tier = "cd-only"
)

// Runner executes Moon with the supplied environment and arguments.
type Runner func(ctx context.Context, env []string, args ...string) ([]byte, error)

// Planner queries Moon and selects affected targets for a gate tier.
type Planner struct {
	Runner Runner
}

// Task is a selected target, its transitive dependencies, and the CI image its
// project declares. An undeclared project runs on ci-base.
type Task struct {
	Target string
	Deps   []string
	Image  string
}

type moonTask struct {
	Target string `json:"target"`
	Deps   []struct {
		Target string `json:"target"`
	} `json:"deps"`
	Tags    []string `json:"tags"`
	Type    string   `json:"type"`
	Options struct {
		RunInCI *bool `json:"runInCI"`
	} `json:"options"`
}

type moonQuery struct {
	Tasks map[string]map[string]moonTask `json:"tasks"`
}

// PlanTargets returns the sorted targets for image on tier. An empty image means
// ci-base, the default image for an undeclared project, except on the cd-only
// tier, where it means every image. The full tier refuses a plan with a task whose
// image has no job in jobImages (UncoveredError), so no planned task is dropped.
func PlanTargets(ctx context.Context, tier Tier, image, base, head string, jobImages []string) ([]string, error) {
	return planTargetsWith(ctx, Planner{Runner: runMoon}, tier, image, base, head, jobImages)
}

// planTargetsWith is PlanTargets with an injectable runner, so a test can give
// it a fixed moon graph and reach the uncovered-image refusal.
func planTargetsWith(ctx context.Context, planner Planner, tier Tier, image, base, head string, jobImages []string) ([]string, error) {
	if tier == CDOnly {
		// cd-only selects by target-name prefix in gate-runner. An empty image means
		// every image, because the ci-base default would drop build-image targets. A
		// given image still filters by the project's declared image.
		if image == "" {
			return planner.Targets(ctx, tier, base, head)
		}
		return planner.TargetsForImage(ctx, tier, image, base, head)
	}
	if image == "" {
		image = "ci-base"
	}
	if tier == Full {
		all, err := planner.PlanAll(ctx, tier, base, head)
		if err != nil {
			return nil, err
		}
		if missing := Uncovered(all, jobImages); len(missing) > 0 {
			return nil, &UncoveredError{Targets: targetsOf(missing), Image: missing[0].Image}
		}
	}
	return planner.TargetsForImage(ctx, tier, image, base, head)
}

// Targets returns sorted affected Moon targets using this planner's runner.
func (p Planner) Targets(ctx context.Context, tier Tier, base, head string) ([]string, error) {
	tasks, err := p.Plan(ctx, tier, base, head)
	if err != nil {
		return nil, err
	}
	targets := make([]string, 0, len(tasks))
	for _, task := range tasks {
		targets = append(targets, task.Target)
	}
	return targets, nil
}

// Plan returns selected targets with their transitive dependencies.
func (p Planner) Plan(ctx context.Context, tier Tier, base, head string) ([]Task, error) {
	if tier != Fast && tier != Full && tier != CDOnly {
		return nil, fmt.Errorf("invalid gate tier %q", tier)
	}
	if p.Runner == nil {
		p.Runner = runMoon
	}
	if tier == CDOnly {
		return p.planCDOnly(ctx, base, head)
	}
	downstream := "none"
	if tier == Full {
		downstream = "direct"
	}
	data, err := p.Runner(ctx, []string{"MOON_BASE=" + base, "MOON_HEAD=" + head}, "query", "tasks", "--affected", "--downstream", downstream)
	if err != nil {
		return nil, fmt.Errorf("query affected Moon tasks: %w", err)
	}
	var query moonQuery
	if err := json.Unmarshal(data, &query); err != nil {
		return nil, fmt.Errorf("decode affected Moon tasks: %w", err)
	}
	if query.Tasks == nil {
		return nil, errors.New("affected Moon task query has no tasks")
	}
	// Both tiers read the whole task graph, so a cd-only dependency that is not
	// itself affected is still found and its dependents excluded.
	data, err = p.Runner(ctx, nil, "query", "tasks")
	if err != nil {
		return nil, fmt.Errorf("query Moon task graph: %w", err)
	}
	graph := moonQuery{}
	if err := json.Unmarshal(data, &graph); err != nil || graph.Tasks == nil {
		return nil, errors.New("invalid Moon task graph")
	}
	return selectTasks(query, graph), nil
}

// planCDOnly plans the affected cd-only targets that run in CI. It cannot reuse
// selectTasks: dependsOnExcluded drops every cd-only task. It keeps a cd-only
// target only when runInCI is not false, so a build-image stitch or non-amd64 leg
// is dropped. It queries the deep walk, the same as the push plan, so a cd-only
// task whose dependency changed is planned even when its own inputs did not.
func (p Planner) planCDOnly(ctx context.Context, base, head string) ([]Task, error) {
	data, err := p.Runner(ctx, []string{"MOON_BASE=" + base, "MOON_HEAD=" + head}, "query", "tasks", "--affected", "--downstream", "deep")
	if err != nil {
		return nil, fmt.Errorf("query affected Moon tasks: %w", err)
	}
	var query moonQuery
	if err := json.Unmarshal(data, &query); err != nil {
		return nil, fmt.Errorf("decode affected Moon tasks: %w", err)
	}
	if query.Tasks == nil {
		return nil, errors.New("affected Moon task query has no tasks")
	}
	data, err = p.Runner(ctx, nil, "query", "tasks")
	if err != nil {
		return nil, fmt.Errorf("query Moon task graph: %w", err)
	}
	graph := moonQuery{}
	if err := json.Unmarshal(data, &graph); err != nil || graph.Tasks == nil {
		return nil, errors.New("invalid Moon task graph")
	}
	return selectCDOnly(query, graph), nil
}

// selectCDOnly returns the affected cd-only targets with runInCI not false, each
// with its transitive dependencies, sorted by target.
func selectCDOnly(query, graph moonQuery) []Task {
	tasks := taskIndex(graph)
	selected := make([]Task, 0)
	for _, project := range query.Tasks {
		for _, affected := range project {
			task, ok := tasks[affected.Target]
			if !ok || !hasTag(task.Tags, "cd-only") {
				continue
			}
			if task.Options.RunInCI != nil && !*task.Options.RunInCI {
				continue
			}
			selected = append(selected, Task{Target: task.Target, Deps: transitiveDeps(tasks, task.Target)})
		}
	}
	slices.SortFunc(selected, func(a, b Task) int { return strings.Compare(a.Target, b.Target) })
	return selected
}

// taskIndex maps each target in the graph to its task.
func taskIndex(graph moonQuery) map[string]moonTask {
	tasks := make(map[string]moonTask)
	for _, project := range graph.Tasks {
		for _, task := range project {
			if task.Target == "" {
				continue
			}
			tasks[task.Target] = task
		}
	}
	return tasks
}

// transitiveDeps returns the sorted targets that target depends on, directly or
// through other tasks in tasks.
func transitiveDeps(tasks map[string]moonTask, target string) []string {
	seen := make(map[string]bool)
	var visit func(string)
	visit = func(current string) {
		for _, dep := range tasks[current].Deps {
			if !seen[dep.Target] {
				seen[dep.Target] = true
				visit(dep.Target)
			}
		}
	}
	visit(target)
	deps := make([]string, 0, len(seen))
	for dep := range seen {
		deps = append(deps, dep)
	}
	slices.Sort(deps)
	return deps
}

// TargetsForImage returns the planned targets whose project declares image. An
// undeclared project declares ci-base. Each full-tier job runs only its image's
// tasks, so a task whose tools the image lacks never runs there.
func (p Planner) TargetsForImage(ctx context.Context, tier Tier, image, base, head string) ([]string, error) {
	tasks, err := p.PlanForImage(ctx, tier, image, base, head)
	if err != nil {
		return nil, err
	}
	targets := make([]string, 0, len(tasks))
	for _, task := range tasks {
		targets = append(targets, task.Target)
	}
	return targets, nil
}

// PlanForImage returns the planned tasks for image, each with its project image.
func (p Planner) PlanForImage(ctx context.Context, tier Tier, image, base, head string) ([]Task, error) {
	all, err := p.PlanAll(ctx, tier, base, head)
	if err != nil {
		return nil, err
	}
	return OnImage(all, image), nil
}

// PlanAll returns every planned task with its project image, across all images.
func (p Planner) PlanAll(ctx context.Context, tier Tier, base, head string) ([]Task, error) {
	tasks, err := p.Plan(ctx, tier, base, head)
	if err != nil {
		return nil, err
	}
	images, err := p.projectImages(ctx)
	if err != nil {
		return nil, err
	}
	for i := range tasks {
		tasks[i].Image = imageFor(images, tasks[i].Target)
	}
	return tasks, nil
}

func targetsOf(tasks []Task) []string {
	targets := make([]string, 0, len(tasks))
	for _, task := range tasks {
		targets = append(targets, task.Target)
	}
	return targets
}

// OnImage returns the tasks whose project declares image.
func OnImage(tasks []Task, image string) []Task {
	selected := make([]Task, 0, len(tasks))
	for _, task := range tasks {
		if task.Image == image {
			selected = append(selected, task)
		}
	}
	return selected
}

// Uncovered returns the tasks whose image is not in images. Each one would run in
// no job, so a caller must refuse the plan rather than drop them.
func Uncovered(tasks []Task, images []string) []Task {
	var missing []Task
	for _, task := range tasks {
		if !slices.Contains(images, task.Image) {
			missing = append(missing, task)
		}
	}
	return missing
}

// UncoveredError reports planned targets whose image has no full-tier job. Each
// would run in no job, so the gate refuses the plan instead of dropping them.
type UncoveredError struct {
	Targets []string
	Image   string
}

func (e *UncoveredError) Error() string {
	return fmt.Sprintf("planned tasks %s declare image %s, which has no full-tier job", strings.Join(e.Targets, ", "), e.Image)
}

// imageFor returns the CI image a target's project declares, or ci-base.
func imageFor(images map[string]string, target string) string {
	project, _, _ := strings.Cut(target, ":")
	if image := images[project]; image != "" {
		return image
	}
	return "ci-base"
}

// projectImages reads each project's declared CI image from moon query projects.
// The path is config.project.metadata.metadata.pipeline.image in moon 2.6.5.
func (p Planner) projectImages(ctx context.Context) (map[string]string, error) {
	data, err := p.Runner(ctx, nil, "query", "projects")
	if err != nil {
		return nil, fmt.Errorf("query Moon projects: %w", err)
	}
	var projects struct {
		Projects []struct {
			ID     string `json:"id"`
			Config struct {
				Project struct {
					Metadata struct {
						Metadata struct {
							Pipeline struct {
								Image string `json:"image"`
							} `json:"pipeline"`
						} `json:"metadata"`
					} `json:"metadata"`
				} `json:"project"`
			} `json:"config"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(data, &projects); err != nil {
		return nil, fmt.Errorf("decode Moon projects: %w", err)
	}
	images := make(map[string]string, len(projects.Projects))
	for _, project := range projects.Projects {
		images[project.ID] = project.Config.Project.Metadata.Metadata.Pipeline.Image
	}
	return images, nil
}

func selectTasks(query, graph moonQuery) []Task {
	tasks := taskIndex(graph)
	affected := make(map[string]bool)
	for _, project := range query.Tasks {
		for _, task := range project {
			affected[task.Target] = true
		}
	}
	excludedMemo := make(map[string]bool, len(tasks))
	selected := make([]Task, 0, len(tasks))
	for target, task := range tasks {
		if !affected[target] {
			continue
		}
		if task.Options.RunInCI != nil && !*task.Options.RunInCI {
			continue
		}
		if dependsOnExcluded(target, tasks, make(map[string]bool), excludedMemo) {
			continue
		}
		selected = append(selected, Task{Target: target, Deps: transitiveDeps(tasks, target)})
	}
	slices.SortFunc(selected, func(a, b Task) int { return strings.Compare(a.Target, b.Target) })
	return selected
}

func runMoon(ctx context.Context, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "moon", args...) //nolint:gosec // Moon is fixed; arguments are constructed above.
	cmd.Env = append(os.Environ(), env...)
	output, err := cmd.Output()
	if err != nil {
		return output, fmt.Errorf("moon %s: %w", strings.Join(args, " "), err)
	}
	return output, nil
}

func hasTag(tags []string, wanted string) bool {
	return slices.Contains(tags, wanted)
}

func dependsOnExcluded(target string, tasks map[string]moonTask, visiting, memo map[string]bool) bool {
	if memo[target] {
		return true
	}
	if visiting[target] {
		return false
	}
	task, ok := tasks[target]
	if !ok {
		return false
	}
	if task.Type == "build" || hasTag(task.Tags, "cd-only") {
		memo[target] = true
		return true
	}
	visiting[target] = true
	for _, dep := range task.Deps {
		if dependsOnExcluded(dep.Target, tasks, visiting, memo) {
			delete(visiting, target)
			memo[target] = true
			return true
		}
	}
	delete(visiting, target)
	return false
}

// AffectedTargets returns the sorted affected targets between base and head
// that are in known. Unlike PlanTargets, it keeps runInCI:false deploy targets
// such as aws:up, which selectTasks drops, so a release can restore a lost
// enqueue. It queries with CI= in the environment, as Petrel's QUERY_ENV does.
func AffectedTargets(ctx context.Context, base, head string, known []string) ([]string, error) {
	return affectedTargetsWith(ctx, runMoon, base, head, known)
}

func affectedTargetsWith(ctx context.Context, run Runner, base, head string, known []string) ([]string, error) {
	data, err := run(ctx, []string{"CI=", "MOON_BASE=" + base, "MOON_HEAD=" + head}, "query", "tasks", "--affected", "--downstream", "deep")
	if err != nil {
		return nil, fmt.Errorf("query affected Moon tasks: %w", err)
	}
	var query moonQuery
	if err := json.Unmarshal(data, &query); err != nil {
		return nil, fmt.Errorf("decode affected Moon tasks: %w", err)
	}
	if query.Tasks == nil {
		return nil, errors.New("affected Moon task query has no tasks")
	}
	var out []string
	for _, project := range query.Tasks {
		for _, task := range project {
			if task.Target == "" || !slices.Contains(known, task.Target) || slices.Contains(out, task.Target) {
				continue
			}
			out = append(out, task.Target)
		}
	}
	slices.Sort(out)
	return out, nil
}
