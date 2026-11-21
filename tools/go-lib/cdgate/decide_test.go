package cdgate

import "testing"

const (
	oldCommit    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	middleCommit = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	headCommit   = "cccccccccccccccccccccccccccccccccccccccc"
	otherCommit  = "dddddddddddddddddddddddddddddddddddddddd"
)

var members = []string{"pulumi-up-aws", "nix-deploy-fleet"}

// ranks: head is zero, middle is one, old is two. A higher rank is older.
var ranks = map[string]int{headCommit: 0, middleCommit: 1, oldCommit: 2}

func rank(commit string) (int, bool) {
	r, ok := ranks[commit]
	return r, ok
}

func allIn(state string) map[string]string {
	m := make(map[string]string, len(members))
	for _, member := range members {
		m[member] = state
	}
	return m
}

func baseInput() Input {
	return Input{
		Self: Self{Pipeline: 10, Commit: oldCommit, Parent: 0, Members: members},
		Head: headCommit,
		Rank: rank,
	}
}

func TestRestartOffHeadSkipsBeforeCoverage(t *testing.T) {
	in := baseInput()
	in.Self.Parent = 9
	in.Newer = []Newer{{Pipeline: 11, Commit: headCommit, States: allIn("success")}}
	got := Decide(in)
	if got.Deploy || got.Reason != ReasonRestartNotHead {
		t.Fatalf("got %+v, want skip restart-not-at-head", got)
	}
}

func TestRestartAtHeadDeploysWhenNoCover(t *testing.T) {
	in := baseInput()
	in.Self.Commit = headCommit
	in.Self.Parent = 9
	if got := Decide(in); !got.Deploy {
		t.Fatalf("got %+v, want deploy", got)
	}
}

func TestStartedStatesSupersede(t *testing.T) {
	for _, state := range []string{"running", "success"} {
		t.Run(state, func(t *testing.T) {
			in := baseInput()
			in.Newer = []Newer{{Pipeline: 11, Commit: middleCommit, States: allIn(state)}}
			got := Decide(in)
			if got.Deploy || got.Reason != ReasonSuperseded || got.By != 11 {
				t.Fatalf("got %+v, want superseded by 11", got)
			}
		})
	}
}

func TestNonCoveringStatesDeploy(t *testing.T) {
	for _, state := range []string{"pending", "failure", "killed", "error", "skipped", "blocked", "declined"} {
		t.Run(state, func(t *testing.T) {
			in := baseInput()
			in.Newer = []Newer{{Pipeline: 11, Commit: middleCommit, States: allIn(state)}}
			if got := Decide(in); !got.Deploy {
				t.Fatalf("got %+v, want deploy", got)
			}
		})
	}
}

func TestMissingMemberDeploys(t *testing.T) {
	in := baseInput()
	in.Newer = []Newer{{Pipeline: 11, Commit: middleCommit, States: map[string]string{members[0]: "success"}}}
	if got := Decide(in); !got.Deploy {
		t.Fatalf("got %+v, want deploy", got)
	}
}

func TestSameCommitOrNonDescendantDoesNotCover(t *testing.T) {
	in := baseInput()
	in.Newer = []Newer{
		{Pipeline: 12, Commit: oldCommit, States: allIn("success")},
		{Pipeline: 13, Commit: otherCommit, States: allIn("success")},
	}
	if got := Decide(in); !got.Deploy {
		t.Fatalf("got %+v, want deploy", got)
	}
}

func TestLowestNumberedCoveringPipelineChosen(t *testing.T) {
	in := baseInput()
	in.Newer = []Newer{
		{Pipeline: 17, Commit: headCommit, States: allIn("success")},
		{Pipeline: 12, Commit: middleCommit, States: allIn("success")},
	}
	got := Decide(in)
	if got.Deploy || got.Reason != ReasonSuperseded || got.By != 12 {
		t.Fatalf("got %+v, want superseded by 12", got)
	}
}

func TestUnknownCommitRankDeploys(t *testing.T) {
	in := baseInput()
	in.Self.Commit = otherCommit
	if got := Decide(in); !got.Deploy {
		t.Fatalf("got %+v, want deploy", got)
	}
}

func TestPendingNewerDoesNotSupersede(t *testing.T) {
	in := baseInput()
	in.Newer = []Newer{{Pipeline: 11, Commit: middleCommit, States: allIn("pending")}}
	if got := Decide(in); !got.Deploy {
		t.Fatalf("got %+v, want deploy: a pending newer copy has not started", got)
	}
}

// One member running and the other pending is not every member started, so the
// target deploys. Both orders of the two members are checked.
func TestMixedRunningAndPendingDeploys(t *testing.T) {
	for _, states := range []map[string]string{
		{members[0]: "running", members[1]: "pending"},
		{members[0]: "pending", members[1]: "running"},
	} {
		in := baseInput()
		in.Newer = []Newer{{Pipeline: 11, Commit: middleCommit, States: states}}
		if got := Decide(in); !got.Deploy {
			t.Fatalf("states %v: got %+v, want deploy", states, got)
		}
	}
}
