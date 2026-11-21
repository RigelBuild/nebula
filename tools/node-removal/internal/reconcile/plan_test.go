package reconcile

import (
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/RigelBuild/nebula/tools/node-removal/internal/kube"
)

// TestUnknownReadyNodeIsProtectivelyCordonedAndHeld maps plan.test.ts: an unknown Ready Node gets a
// protective cordon and a ready hold, and is never deleted.
func TestUnknownReadyNodeIsProtectivelyCordonedAndHeld(t *testing.T) {
	t.Parallel()
	unknown := testNode("unknown", nil)
	nodes := slices.Concat(testSnapshot(nil, nil, nil).Nodes, []NodeView{unknown})
	actions := Classify(testDesired, testSnapshot(nodes, nil, nil), testNow).Actions
	wantCordon := Action{
		Kind:       KindCordon,
		Node:       "unknown",
		Target:     unknown.Target,
		NodePrior:  &NodePrior{Unschedulable: false},
		Protective: true,
	}
	if !containsAction(actions, wantCordon) {
		t.Fatalf("missing protective cordon %+v in %+v", wantCordon, actions)
	}
	if !containsAction(actions, Action{Kind: KindHold, Node: "unknown", Reason: HoldReady}) {
		t.Fatalf("missing ready hold for unknown in %+v", actions)
	}
	if containsKind(actions, KindDeleteNode) || containsKind(actions, KindDeleteLonghornNode) {
		t.Fatalf("unknown Ready node must never be deleted: %+v", actions)
	}
}

// TestControlPlaneNodesOnlyHold maps plan.test.ts: control-plane nodes produce only control-plane holds
// for retiring, unknown Ready, unknown NotReady, and member priors.
func TestControlPlaneNodesOnlyHold(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		node NodeView
	}{
		{"retiring-a", testNode("retiring-a", func(n *NodeView) { n.ControlPlane = true })},
		{"unknown-ready", testNode("unknown-ready", func(n *NodeView) { n.ControlPlane = true })},
		{"unknown-dead", testNode("unknown-dead", func(n *NodeView) {
			n.ControlPlane = true
			n.Ready = "Unknown"
			n.ReadySince = "2026-10-06T09:00:00Z"
		})},
		{"member-a", testNode("member-a", func(n *NodeView) {
			n.ControlPlane = true
			n.Prior = &NodePrior{Unschedulable: true}
		})},
	}
	for _, c := range cases {
		nodes := []NodeView{testNode("member-a", nil), testNode("member-b", nil), testNode("member-c", nil), c.node}
		if c.name == "member-a" {
			nodes = []NodeView{c.node, testNode("member-b", nil), testNode("member-c", nil)}
		}
		lh := []LonghornNodeView{testLonghorn(c.name, func(l *LonghornNodeView) {
			l.Prior = &LonghornPrior{AllowScheduling: true, EvictionRequested: false}
		})}
		actions := Classify(testDesired, testSnapshot(nodes, lh, nil), testNow).Actions
		want := []Action{{Kind: KindHold, Node: c.name, Reason: HoldControlPlane}}
		if !reflect.DeepEqual(actions, want) {
			t.Errorf("%s: actions = %+v, want %+v", c.name, actions, want)
		}
	}
}

// TestRenamedReadyControlPlaneStaysHeld maps plan.test.ts: a Ready control-plane Node that is renamed remains held.
func TestRenamedReadyControlPlaneStaysHeld(t *testing.T) {
	t.Parallel()
	renamed := testNode("renamed-node", func(n *NodeView) { n.ControlPlane = true })
	desired := testDesired
	desired.Retiring = []string{}
	desired.Renamed = []string{"renamed-node"}
	nodes := slices.Concat(testSnapshot(nil, nil, nil).Nodes, []NodeView{renamed})
	actions := Classify(desired, testSnapshot(nodes, []LonghornNodeView{}, nil), testNow).Actions
	if !containsAction(actions, Action{Kind: KindHold, Node: "renamed-node", Reason: HoldControlPlane}) {
		t.Fatalf("renamed Ready control-plane not held: %+v", actions)
	}
}

// TestNotReadyControlPlaneStaysHeld maps plan.test.ts: a non-renamed NotReady control-plane Node remains held.
func TestNotReadyControlPlaneStaysHeld(t *testing.T) {
	t.Parallel()
	server := testNode("old-server", func(n *NodeView) {
		n.ControlPlane = true
		n.Ready = "False"
		n.ReadySince = "2026-10-06T09:00:00Z"
	})
	desired := testDesired
	desired.Retiring = []string{}
	nodes := slices.Concat(testSnapshot(nil, nil, nil).Nodes, []NodeView{server})
	actions := Classify(desired, testSnapshot(nodes, nil, nil), testNow).Actions
	if !containsAction(actions, Action{Kind: KindHold, Node: "old-server", Reason: HoldControlPlane}) {
		t.Fatalf("NotReady control-plane not held: %+v", actions)
	}
}

// containsAction reports whether want equals one planned action, comparing every field.
func containsAction(actions []Action, want Action) bool {
	for _, action := range actions {
		if reflect.DeepEqual(action, want) {
			return true
		}
	}
	return false
}

func baseFiles() map[string]string {
	return map[string]string{
		"members":                 "member-a",
		"retiring":                "",
		"renamed":                 "",
		"mode":                    "dry-run",
		"min-nodes":               "1",
		"not-ready-grace-seconds": "1800",
	}
}

// TestParseDesiredAcceptsMountedKeys maps desired.test.ts: the valid case returns the parsed set.
func TestParseDesiredAcceptsMountedKeys(t *testing.T) {
	t.Parallel()
	parsed, err := ParseDesired(baseFiles())
	if err != nil {
		t.Fatalf("ParseDesired: %v", err)
	}
	want := DesiredSet{
		Members:              []string{"member-a"},
		Retiring:             []string{},
		Renamed:              []string{},
		Mode:                 ModeDryRun,
		MinNodes:             1,
		NotReadyGraceSeconds: 1800,
	}
	if !reflect.DeepEqual(parsed, want) {
		t.Fatalf("ParseDesired = %+v, want %+v", parsed, want)
	}
}

// TestParseDesiredRejectsInvalid maps desired.test.ts: invalid mode, overlap, and min-nodes below one all throw.
func TestParseDesiredRejectsInvalid(t *testing.T) {
	t.Parallel()
	base := baseFiles()
	cases := map[string]map[string]string{
		"unknown mode":        withKey(base, "mode", "unknown"),
		"member also retired": withKey(base, "retiring", "member-a"),
		"member also renamed": withKey(base, "renamed", "member-a"),
		"min-nodes zero":      withKey(base, "min-nodes", "0"),
	}
	for name, files := range cases {
		if _, err := ParseDesired(files); err == nil {
			t.Errorf("%s: ParseDesired accepted invalid input", name)
		}
	}
}

func withKey(base map[string]string, key, value string) map[string]string {
	out := maps.Clone(base)
	out[key] = value
	return out
}

// Ports renamed.test.ts. Each test names the TS case it maps.

func renamedDesired() DesiredSet {
	return withDesired(func(d *DesiredSet) {
		d.Members = append(d.Members, "member-node")
		d.Renamed = []string{"renamed-node"}
	})
}

func deadServer(o rawOpts) obj {
	if o.ready == "" {
		o.ready, o.readySince = "False", "2026-10-06T09:00:00Z"
	}
	o.labels = obj{controlPlaneLabel: ""}
	return rawNode("renamed-node", o)
}

func renamedCluster(longhorn []obj) *fakeState {
	state := rawCluster(rawOpts{})
	state.nodes = append(state.nodes, rawNode("member-node", rawOpts{}), deadServer(rawOpts{}))
	state.longhorn = longhorn
	return state
}

func replaceServer(state *fakeState, replacement obj) {
	for i, raw := range state.nodes {
		if metadataOf(raw)["name"] == "renamed-node" {
			state.nodes[i] = replacement
		}
	}
}

// TestRenamedNotReadyControlPlaneRemovalRevalidated maps "renamed NotReady control-plane Node removal is revalidated at apply".
func TestRenamedNotReadyControlPlaneRemovalRevalidated(t *testing.T) {
	t.Parallel()
	desired := renamedDesired()
	fake := newFake(t, renamedCluster([]obj{}))
	deletion := findKind(t, currentPlan(t, fake, desired).Actions, KindDeleteNode)
	want := Action{Kind: KindDeleteNode, Node: "renamed-node", Target: testTarget("renamed-node")}
	if !reflect.DeepEqual(deletion, want) {
		t.Fatalf("deletion = %+v, want %+v", deletion, want)
	}
	if got := mustApply(t, runner(fake, desired, nil, nil), deletion); got != (Applied{OK: true}) {
		t.Fatalf("result = %+v, want ok", got)
	}
	if findNamed(fake.state.nodes, "renamed-node") != nil {
		t.Fatal("renamed-node Node still present")
	}
}

// TestRenamedDeletionBlockedByApplyTimeChanges maps "renamed NotReady control-plane deletions are blocked by apply-time changes".
func TestRenamedDeletionBlockedByApplyTimeChanges(t *testing.T) {
	t.Parallel()
	desired := renamedDesired()
	leftRenamed := desired
	leftRenamed.Renamed = []string{}
	cases := []struct {
		name    string
		desired DesiredSet
		mutate  func(*fakeState)
	}{
		{"becomes Ready", desired, func(s *fakeState) { replaceServer(s, deadServer(rawOpts{ready: "True"})) }},
		{"leaves renamed", leftRenamed, func(*fakeState) {}},
		{"remains within grace", desired, func(s *fakeState) {
			replaceServer(s, deadServer(rawOpts{ready: "False", readySince: "2026-10-06T11:50:00Z"}))
		}},
		{"still holds Longhorn replicas", desired, func(s *fakeState) {
			s.longhorn = append(s.longhorn, rawLonghorn("renamed-node", rawOpts{replicaCount: 1}))
		}},
	}
	for _, c := range cases {
		state := renamedCluster([]obj{})
		fake := newFake(t, state)
		deletion := findKind(t, currentPlan(t, fake, desired).Actions, KindDeleteNode)
		c.mutate(state)
		result := mustApply(t, runner(fake, c.desired, nil, nil), deletion)
		if result != (Applied{Hold: HoldStalePlan}) || len(fake.writes) != 0 {
			t.Errorf("%s: result = %+v writes = %+v, want stale-plan and no writes", c.name, result, fake.writes)
		}
	}
}

// TestRenamedLonghornDeletionRechecksReplicas maps "renamed Node Longhorn deletion rechecks replicas at apply".
func TestRenamedLonghornDeletionRechecksReplicas(t *testing.T) {
	t.Parallel()
	desired := renamedDesired()
	setup := func() (*fakeAPI, Action) {
		fake := newFake(t, renamedCluster([]obj{rawLonghorn("renamed-node", rawOpts{})}))
		return fake, findKind(t, currentPlan(t, fake, desired).Actions, KindDeleteLonghornNode)
	}
	stale, staleDeletion := setup()
	disks := objOrEmpty(objOrEmpty(stale.state.longhorn[0]["status"])["diskStatus"])
	replicas, ok := objOrEmpty(disks["default"])["scheduledReplica"].(obj)
	if !ok {
		t.Fatal("expected existing Longhorn scheduledReplica map")
	}
	replicas["replica-1"] = obj{}
	wantStale(t, stale, mustApply(t, runner(stale, desired, nil, nil), staleDeletion))

	clear, clearDeletion := setup()
	if got := mustApply(t, runner(clear, desired, nil, nil), clearDeletion); got != (Applied{OK: true}) {
		t.Fatalf("result = %+v, want ok", got)
	}
	if len(clear.writes) != 1 || clear.writes[0].method != "DELETE" {
		t.Fatalf("writes = %+v, want one DELETE", clear.writes)
	}
	if findNamed(clear.state.longhorn, "renamed-node") != nil {
		t.Fatal("renamed-node Longhorn node still present")
	}
}

// listWithout serves the Longhorn list without the named CR, as when the CR is created between
// the list and the per-Node GET. Every other path, including that GET, reaches the fake.
type listWithout struct {
	*fakeAPI
	name string
}

func (l listWithout) Get(path string) (kube.Response, error) {
	if path != longhornNodesPath {
		return l.fakeAPI.Get(path)
	}
	kept := []obj{}
	for _, item := range l.state.longhorn {
		if metadataOf(item)["name"] != l.name {
			kept = append(kept, item)
		}
	}
	return l.list(kept), nil
}

// get404 answers the named CR's GET with 404 while the list still returns it.
type get404 struct {
	*fakeAPI
	name string
}

func (g get404) Get(path string) (kube.Response, error) {
	if path == longhornNodesPath+"/"+g.name {
		return kube.Response{Status: 404}, nil
	}
	return g.fakeAPI.Get(path)
}

// TestReadKeepsLonghornCRThatOnlyTheGetFinds maps the TS read: a per-Node GET that finds a CR
// the list missed keeps it, so the replica check still holds the Node.
func TestReadKeepsLonghornCRThatOnlyTheGetFinds(t *testing.T) {
	t.Parallel()
	desired := renamedDesired()
	state := renamedCluster([]obj{rawLonghorn("renamed-node", rawOpts{replicaCount: 1})})
	snap, err := Read(desired, listWithout{fakeAPI: newFake(t, state), name: "renamed-node"})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !slices.ContainsFunc(snap.Longhorn, func(view LonghornNodeView) bool { return view.Name == "renamed-node" }) {
		t.Fatalf("longhorn = %+v, want the GET-only renamed-node CR kept", snap.Longhorn)
	}
	plan := Classify(desired, snap, testNow)
	if containsKind(plan.Actions, KindDeleteNode) {
		t.Fatalf("actions = %v, want no delete-node while the Longhorn CR holds replicas", kinds(plan.Actions))
	}
	hold := findKind(t, plan.Actions, KindHold)
	if hold.Reason != HoldLonghornReplicas {
		t.Fatalf("hold reason = %q, want %q", hold.Reason, HoldLonghornReplicas)
	}
}

// TestReadDropsLonghornCRWhenGetIs404 maps the TS read: a per-Node GET 404 removes a CR the
// list still returned.
func TestReadDropsLonghornCRWhenGetIs404(t *testing.T) {
	t.Parallel()
	desired := renamedDesired()
	state := renamedCluster([]obj{rawLonghorn("renamed-node", rawOpts{})})
	snap, err := Read(desired, get404{fakeAPI: newFake(t, state), name: "renamed-node"})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if slices.ContainsFunc(snap.Longhorn, func(view LonghornNodeView) bool { return view.Name == "renamed-node" }) {
		t.Fatalf("longhorn = %+v, want renamed-node dropped after its GET 404", snap.Longhorn)
	}
}
