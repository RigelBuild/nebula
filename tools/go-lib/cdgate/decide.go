// Package cdgate decides whether a CD target deploys or is skipped because a
// newer pipeline already covers it.
package cdgate

// Self describes the pipeline a CD target runs in.
type Self struct {
	Pipeline int64
	Commit   string
	// Parent is zero for a fresh pipeline and nonzero for a restart.
	Parent  int64
	Members []string
}

// Newer is a descendant main pipeline and the states of the group members it
// ran.
type Newer struct {
	Pipeline int64
	Commit   string
	States   map[string]string
}

// Input is the data the gate decides from. Rank reports a commit's distance
// from main's head; a higher rank is an older commit. It reports false for a
// commit that is not on main's history.
type Input struct {
	Self  Self
	Head  string
	Rank  func(commit string) (int, bool)
	Newer []Newer
}

// Verdict is the gate outcome. Deploy means run the command. Otherwise Reason
// says why it was skipped, and By names the covering pipeline when superseded.
type Verdict struct {
	Deploy bool
	Reason string
	By     int64
}

const (
	ReasonSuperseded     = "superseded"
	ReasonRestartNotHead = "restart-not-at-head"
)

// coveringStates are workflow states that cover a group member: the copy has
// started or finished. A pending copy has not started, so it never covers.
// Failed, killed, errored, skipped, blocked, and declined runs do not cover
// either.
var coveringStates = map[string]bool{"running": true, "success": true}

// Decide applies the CD gate. A restart off the head skips. Otherwise a newer
// main pipeline that covers every group member supersedes this one. The lowest
// covering pipeline number is reported for legacy callers.
func Decide(in Input) Verdict {
	if in.Self.Parent != 0 && in.Self.Commit != in.Head {
		return Verdict{Reason: ReasonRestartNotHead}
	}
	selfRank, ok := in.Rank(in.Self.Commit)
	if !ok {
		return Verdict{Deploy: true}
	}
	var covering int64
	found := false
	for _, p := range in.Newer {
		candidateRank, ok := in.Rank(p.Commit)
		if !ok || candidateRank >= selfRank || !coversAll(in.Self.Members, p.States) {
			continue
		}
		if !found || p.Pipeline < covering {
			covering = p.Pipeline
			found = true
		}
	}
	if !found {
		return Verdict{Deploy: true}
	}
	return Verdict{Reason: ReasonSuperseded, By: covering}
}

func coversAll(members []string, states map[string]string) bool {
	for _, m := range members {
		if !coveringStates[states[m]] {
			return false
		}
	}
	return true
}
