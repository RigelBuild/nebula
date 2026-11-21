package cdgate

import (
	"context"
	"errors"
	"maps"
	"testing"
	"testing/fstest"
)

// fakePipelines serves pipelines from memory. A missing pipeline fails the
// test, so a read the rule should not make cannot pass silently.
type fakePipelines struct {
	t       *testing.T
	details map[int64]PipelineDetail
	list    []PipelineItem
	listErr error
	reads   []int64
}

func (f *fakePipelines) Fetch(_ context.Context, number int64) (PipelineDetail, error) {
	f.reads = append(f.reads, number)
	d, ok := f.details[number]
	if !ok {
		f.t.Fatalf("unexpected pipeline read %d", number)
	}
	return d, nil
}

func (f *fakePipelines) MainPipelines(context.Context, int64) ([]PipelineItem, error) {
	return f.list, f.listErr
}

// rankedHead returns a Ranks function for the main head and rank map.
func rankedHead(ranks map[string]int) func(context.Context) (string, map[string]int, error) {
	return func(context.Context) (string, map[string]int, error) {
		return shaA, ranks, nil
	}
}

// ownPipeline is pipeline 20 at shaB running target deploy, a member of the group.
func ownPipeline() PipelineDetail {
	return PipelineDetail{Number: 20, Commit: shaB, Workflows: map[string]string{"pulumi-up-aws": "running"}}
}

func TestDecisionDeploysWhenNoNewerPipelineCovers(t *testing.T) {
	src := &fakePipelines{t: t, details: map[int64]PipelineDetail{20: ownPipeline()}}
	d := Decision{
		Pipelines:    src,
		Ranks:        rankedHead(map[string]int{shaA: 0, shaB: 1}),
		Members:      []string{"pulumi-up-aws"},
		PulumiStacks: map[string]bool{"aws": true},
	}
	v, err := d.Read(context.Background(), Own{Number: 20, Commit: shaB, Target: "aws:up"})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !v.Deploy {
		t.Fatalf("verdict = %+v, want deploy with no covering pipeline", v)
	}
}

func TestDecisionSupersededByNewerCoveringPipeline(t *testing.T) {
	// shaA is newer (rank 0 < rank 1). Its pipeline 30 has the member running,
	// which covers it, so the own pipeline 20 is superseded.
	src := &fakePipelines{
		t: t,
		details: map[int64]PipelineDetail{
			20: ownPipeline(),
			30: {Number: 30, Commit: shaA, Workflows: map[string]string{"pulumi-up-aws": "running"}},
		},
		list: []PipelineItem{{Number: 30, Commit: shaA, Branch: "main", Event: "push"}},
	}
	d := Decision{
		Pipelines:    src,
		Ranks:        rankedHead(map[string]int{shaA: 0, shaB: 1}),
		Members:      []string{"pulumi-up-aws"},
		PulumiStacks: map[string]bool{"aws": true},
	}
	v, err := d.Read(context.Background(), Own{Number: 20, Commit: shaB, Target: "aws:up"})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if v.Deploy || v.Reason != ReasonSuperseded || v.By != 30 {
		t.Fatalf("verdict = %+v, want superseded by pipeline 30", v)
	}
}

func TestDecisionFailedNewerPipelineDoesNotCover(t *testing.T) {
	src := &fakePipelines{
		t: t,
		details: map[int64]PipelineDetail{
			20: ownPipeline(),
			30: {Number: 30, Commit: shaA, Workflows: map[string]string{"pulumi-up-aws": "failure"}},
		},
		list: []PipelineItem{{Number: 30, Commit: shaA, Branch: "main", Event: "push"}},
	}
	d := Decision{
		Pipelines:    src,
		Ranks:        rankedHead(map[string]int{shaA: 0, shaB: 1}),
		Members:      []string{"pulumi-up-aws"},
		PulumiStacks: map[string]bool{"aws": true},
	}
	v, err := d.Read(context.Background(), Own{Number: 20, Commit: shaB, Target: "aws:up"})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !v.Deploy {
		t.Fatalf("verdict = %+v, want deploy: a failed newer run does not cover", v)
	}
}

func TestDecisionRejectsTargetMissingFromPipeline(t *testing.T) {
	src := &fakePipelines{t: t, details: map[int64]PipelineDetail{20: ownPipeline()}}
	d := Decision{
		Pipelines:    src,
		Ranks:        rankedHead(map[string]int{shaA: 0, shaB: 1}),
		Members:      []string{"pulumi-up-aws"},
		PulumiStacks: map[string]bool{"aws": true},
	}
	_, err := d.Read(context.Background(), Own{Number: 20, Commit: shaB, Target: "nix:deploy"})
	if err == nil || err.Error() != "target is not emitted in its pipeline" {
		t.Fatalf("err = %v, want the target-not-emitted error", err)
	}
}

func TestDecisionRejectsNewerCommitMismatch(t *testing.T) {
	// The list row says shaA, but its detail reports shaC. The mismatch is an error.
	src := &fakePipelines{
		t: t,
		details: map[int64]PipelineDetail{
			20: ownPipeline(),
			30: {Number: 30, Commit: shaC, Workflows: map[string]string{"pulumi-up-aws": "running"}},
		},
		list: []PipelineItem{{Number: 30, Commit: shaA, Branch: "main", Event: "push"}},
	}
	d := Decision{
		Pipelines:    src,
		Ranks:        rankedHead(map[string]int{shaA: 0, shaB: 1}),
		Members:      []string{"pulumi-up-aws"},
		PulumiStacks: map[string]bool{"aws": true},
	}
	if _, err := d.Read(context.Background(), Own{Number: 20, Commit: shaB, Target: "aws:up"}); err == nil || err.Error() != "pipeline commit mismatch" {
		t.Fatalf("err = %v, want the commit-mismatch error", err)
	}
}

func TestDecisionSkipsRestartOffHead(t *testing.T) {
	// A restart (parent != 0) of a commit that is not the head is skipped.
	src := &fakePipelines{t: t, details: map[int64]PipelineDetail{20: ownPipeline()}}
	d := Decision{
		Pipelines:    src,
		Ranks:        rankedHead(map[string]int{shaA: 0, shaB: 1}),
		Members:      []string{"pulumi-up-aws"},
		PulumiStacks: map[string]bool{"aws": true},
	}
	v, err := d.Read(context.Background(), Own{Number: 20, Commit: shaB, Parent: 5, Target: "aws:up"})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if v.Deploy || v.Reason != ReasonRestartNotHead {
		t.Fatalf("verdict = %+v, want restart-not-at-head", v)
	}
}

func TestDecisionPropagatesRankError(t *testing.T) {
	boom := errors.New("git: no origin")
	src := &fakePipelines{t: t, details: map[int64]PipelineDetail{20: ownPipeline()}}
	d := Decision{
		Pipelines: src,
		Ranks: func(context.Context) (string, map[string]int, error) {
			return "", nil, boom
		},
		Members:      []string{"pulumi-up-aws"},
		PulumiStacks: map[string]bool{"aws": true},
	}
	if _, err := d.Read(context.Background(), Own{Number: 20, Commit: shaB, Target: "aws:up"}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the rank failure surfaced", err)
	}
}

func TestWorkflowForTarget(t *testing.T) {
	stacks := map[string]bool{"aws": true}
	tests := []struct {
		name     string
		target   string
		expected string
	}{
		{name: "registered up task", target: "aws:up", expected: "pulumi-up-aws"},
		{name: "registered preview task", target: "aws:preview", expected: "pulumi-preview-aws"},
		{name: "unregistered project falls back", target: "other:up", expected: "other-up"},
		{name: "other task falls back", target: "aws:destroy", expected: "aws-destroy"},
		{name: "target without separator remains unchanged", target: "nix-deploy-fleet", expected: "nix-deploy-fleet"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := WorkflowForTarget(test.target, stacks); got != test.expected {
				t.Fatalf("WorkflowForTarget(%q) = %q, want %q", test.target, got, test.expected)
			}
		})
	}
}

func TestIsPulumiUp(t *testing.T) {
	stacks := map[string]bool{"aws": true}
	tests := []struct {
		name     string
		target   string
		expected bool
	}{
		{name: "registered up task", target: "aws:up", expected: true},
		{name: "registered preview task", target: "aws:preview"},
		{name: "unregistered project", target: "other:up"},
		{name: "other task", target: "aws:destroy"},
		{name: "target without separator", target: "aws"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isPulumiUp(test.target, stacks); got != test.expected {
				t.Fatalf("isPulumiUp(%q) = %t, want %t", test.target, got, test.expected)
			}
		})
	}
}

func TestReadPulumiStacks(t *testing.T) {
	fsys := fstest.MapFS{
		"infra/pulumi/platform/aws/Pulumi.yaml":       {Data: []byte("name: aws")},
		"infra/pulumi/services/service/Pulumi.yaml":   {Data: []byte("name: service")},
		"infra/pulumi/platform/not-a-stack/README.md": {Data: []byte("no manifest")},
	}
	got, err := ReadPulumiStacks(fsys, "infra/pulumi", []string{"platform", "services"})
	if err != nil {
		t.Fatalf("ReadPulumiStacks() error = %v", err)
	}
	want := map[string]bool{"aws": true, "service": true}
	if !maps.Equal(got, want) {
		t.Fatalf("ReadPulumiStacks() = %v, want %v", got, want)
	}
}

func TestReadPulumiStacksRejectsBadInput(t *testing.T) {
	tests := []struct {
		name  string
		root  string
		tiers []string
		want  string
	}{
		{name: "empty root", root: "", tiers: []string{"platform"}, want: "pulumi stack root is empty"},
		{name: "no tiers", root: "infra/pulumi", want: "pulumi stack tiers are empty"},
		{name: "empty tier", root: "infra/pulumi", tiers: []string{"platform", ""}, want: "pulumi stack tier 1 is blank"},
		{name: "blank tier", root: "infra/pulumi", tiers: []string{" "}, want: "pulumi stack tier 0 is blank"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ReadPulumiStacks(fstest.MapFS{}, test.root, test.tiers)
			if err == nil || err.Error() != test.want {
				t.Fatalf("ReadPulumiStacks() error = %v, want %q", err, test.want)
			}
			if got != nil {
				t.Fatalf("ReadPulumiStacks() = %v with error, want nil map", got)
			}
		})
	}
}
