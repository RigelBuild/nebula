package cdgate

import (
	"context"
	"errors"
	"io/fs"
	"path"
	"slices"
	"strings"
)

// Decision gathers the Decide input for one CD target. It reads the own
// pipeline, the newer main pipelines and main's first-parent ranks, then
// applies the gate. Each external read is injected, so the rule is tested
// without Woodpecker or git.
type Decision struct {
	// Pipelines reads pipelines for the repo the step runs in.
	Pipelines PipelineSource
	// Ranks returns main's head commit and its first-parent ranks, head at 0.
	Ranks func(ctx context.Context) (head string, ranks map[string]int, err error)
	// Members are the workflow names this step belongs to, from --members.
	Members []string
	// PulumiStacks contains registered Pulumi project names.
	PulumiStacks map[string]bool
}

// PipelineSource is the read side of PipelineAPI, so tests can stand in.
type PipelineSource interface {
	Fetch(ctx context.Context, number int64) (PipelineDetail, error)
	MainPipelines(ctx context.Context, ownNumber int64) ([]PipelineItem, error)
}

// Own describes the pipeline the step runs in, from the CI environment.
type Own struct {
	Number int64
	Commit string
	Parent int64
	Target string
}

// Read builds the Decide verdict for own. A restart off the head, a target
// missing from its own pipeline, or a commit off main's history follows
// Decide's rules. A newer pipeline whose commit differs from its list row is
// an error.
func (d Decision) Read(ctx context.Context, own Own) (Verdict, error) {
	self, err := d.Pipelines.Fetch(ctx, own.Number)
	if err != nil {
		return Verdict{}, err
	}
	members := d.memberNames(self)
	if !slices.Contains(members, WorkflowForTarget(own.Target, d.PulumiStacks)) {
		return Verdict{}, errors.New("target is not emitted in its pipeline")
	}

	history, err := d.Pipelines.MainPipelines(ctx, own.Number)
	if err != nil {
		return Verdict{}, err
	}
	head, ranks, err := d.Ranks(ctx)
	if err != nil {
		return Verdict{}, err
	}

	rank := func(commit string) (int, bool) {
		r, ok := ranks[commit]
		return r, ok
	}
	var newer []Newer
	selfRank, selfRanked := rank(own.Commit)
	for _, item := range history {
		candidateRank, ok := rank(item.Commit)
		if !ok || !selfRanked || candidateRank >= selfRank {
			continue
		}
		detail, err := d.Pipelines.Fetch(ctx, item.Number)
		if err != nil {
			return Verdict{}, err
		}
		if detail.Commit != item.Commit {
			return Verdict{}, errors.New("pipeline commit mismatch")
		}
		states := make(map[string]string, len(members))
		for _, member := range members {
			if state, present := detail.Workflows[member]; present {
				states[member] = state
			}
		}
		newer = append(newer, Newer{
			Pipeline: item.Number,
			Commit:   item.Commit,
			States:   states,
		})
	}

	return Decide(Input{
		Self: Self{
			Pipeline: own.Number,
			Commit:   own.Commit,
			Parent:   own.Parent,
			Members:  members,
		},
		Head:  head,
		Rank:  rank,
		Newer: newer,
	}), nil
}

// memberNames returns the own pipeline's workflow names that are in d.Members.
// Map order is not defined, so callers must not rely on the order.
func (d Decision) memberNames(self PipelineDetail) []string {
	var members []string
	for name := range self.Workflows {
		if slices.Contains(d.Members, name) {
			members = append(members, name)
		}
	}
	return members
}

// WorkflowForTarget maps a target to the workflow name emitted by its pipeline.
// Only registered Pulumi projects with up or preview tasks use Pulumi workflow names.
func WorkflowForTarget(target string, pulumiStacks map[string]bool) string {
	separator := strings.IndexByte(target, ':')
	if separator < 1 {
		return strings.ReplaceAll(target, ":", "-")
	}
	project := target[:separator]
	task := target[separator+1:]
	if !pulumiStacks[project] || (task != "up" && task != "preview") {
		return strings.ReplaceAll(target, ":", "-")
	}
	return "pulumi-" + task + "-" + project
}

// isPulumiUp reports whether target is an up task for a registered Pulumi project.
func isPulumiUp(target string, pulumiStacks map[string]bool) bool {
	separator := strings.IndexByte(target, ':')
	return separator > 0 && pulumiStacks[target[:separator]] && target[separator+1:] == "up"
}

// ReadPulumiStacks discovers projects with a Pulumi.yaml in each of tiers,
// read beneath root. The caller supplies both: the repo layout is a deployment
// choice, so there is no default tier list or root.
func ReadPulumiStacks(fsys fs.FS, root string, tiers []string) map[string]bool {
	stacks := make(map[string]bool)
	for _, tier := range tiers {
		tierPath := path.Join(root, tier)
		entries, err := fs.ReadDir(fsys, tierPath)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			manifest := path.Join(tierPath, entry.Name(), "Pulumi.yaml")
			if _, err := fs.Stat(fsys, manifest); err == nil {
				stacks[entry.Name()] = true
			}
		}
	}
	return stacks
}
