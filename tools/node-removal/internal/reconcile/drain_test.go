package reconcile

import (
	"reflect"
	"testing"
)

// Ports drain.test.ts. Each test names the TS case it maps.

func cordonedNodes() []NodeView {
	return []NodeView{
		testNode("member-a", nil), testNode("member-b", nil), testNode("member-c", nil),
		testNode("retiring-a", func(n *NodeView) { n.Unschedulable = true }),
	}
}

// TestHeldPodsReplicasAndOrdinaryPodsGateDrained maps "held pods, replicas, and ordinary pods gate drained marking".
func TestHeldPodsReplicasAndOrdinaryPodsGateDrained(t *testing.T) {
	t.Parallel()
	held := Classify(testDesired, testSnapshot(cordonedNodes(), nil,
		[]PodView{testPod("pinned", func(p *PodView) { p.PinnedTo = "retiring-a" })}), testNow).Actions
	if !containsAction(held, Action{Kind: KindHold, Node: "retiring-a", Reason: HoldPinned, Pod: "workloads/pinned"}) {
		t.Fatalf("missing pinned hold: %+v", held)
	}
	if containsKind(held, KindMarkDrained) {
		t.Fatalf("pinned pod must block mark-drained: %+v", held)
	}
	replicas := Classify(testDesired, testSnapshot(cordonedNodes(),
		[]LonghornNodeView{testLonghorn("retiring-a", func(l *LonghornNodeView) { l.ReplicaCount = 1 })}, nil), testNow).Actions
	if containsKind(replicas, KindMarkDrained) {
		t.Fatalf("replicas must block mark-drained: %+v", replicas)
	}
	empty := Classify(testDesired, testSnapshot(cordonedNodes(), []LonghornNodeView{testLonghorn("retiring-a", nil)}, nil), testNow).Actions
	if !containsAction(empty, Action{Kind: KindMarkDrained, Node: "retiring-a", Target: testTarget("retiring-a")}) {
		t.Fatalf("missing mark-drained: %+v", empty)
	}
	ordinary := Classify(testDesired, testSnapshot(cordonedNodes(), nil, []PodView{testPod("ordinary", nil)}), testNow).Actions
	if !containsKind(ordinary, KindEvictPod) || containsKind(ordinary, KindMarkDrained) {
		t.Fatalf("ordinary pod: want evict-pod and no mark-drained: %+v", ordinary)
	}
}

// TestOneDeleteUnitCRBackedFirst maps "one delete unit is selected and a CR-backed NotReady Node starts with its CR".
func TestOneDeleteUnitCRBackedFirst(t *testing.T) {
	t.Parallel()
	nodes := []NodeView{
		testNode("member-a", nil), testNode("member-b", nil), testNode("member-c", nil),
		testNode("dead-a", func(n *NodeView) { n.Ready, n.ReadySince = "False", "2026-10-06T09:00:00Z" }),
		testNode("dead-b", func(n *NodeView) { n.Ready, n.ReadySince = "Unknown", "2026-10-06T09:00:00Z" }),
	}
	actions := Classify(testDesired, testSnapshot(nodes,
		[]LonghornNodeView{testLonghorn("dead-a", nil), testLonghorn("dead-b", nil)}, nil), testNow).Actions
	deletes := []Action{}
	for _, action := range actions {
		if action.Kind == KindDeleteLonghornNode || action.Kind == KindDeleteNode {
			deletes = append(deletes, action)
		}
	}
	if len(deletes) != 1 || deletes[0].Kind != KindDeleteLonghornNode || deletes[0].Node != "dead-a" {
		t.Fatalf("deletes = %+v, want one delete-longhorn-node for dead-a", deletes)
	}
	if !containsAction(actions, Action{Kind: KindHold, Node: "dead-b", Reason: HoldDeleteBudget}) {
		t.Fatalf("missing delete-budget hold for dead-b: %+v", actions)
	}
}

// TestCRLessNotReadyNodePlannedForDeletion maps "an eligible CR-less NotReady Node is planned for Node deletion only after absence".
func TestCRLessNotReadyNodePlannedForDeletion(t *testing.T) {
	t.Parallel()
	nodes := []NodeView{
		testNode("member-a", nil), testNode("member-b", nil), testNode("member-c", nil),
		testNode("dead", func(n *NodeView) { n.Ready, n.ReadySince = "False", "2026-10-06T09:00:00Z" }),
	}
	actions := Classify(testDesired, testSnapshot(nodes, []LonghornNodeView{}, nil), testNow).Actions
	if !containsAction(actions, Action{Kind: KindDeleteNode, Node: "dead", Target: testTarget("dead")}) {
		t.Fatalf("missing delete-node for dead: %+v", actions)
	}
}

// TestOrphanLifecycle maps "orphans are marked, held for safety checks, then deleted; returned Nodes clear the mark".
func TestOrphanLifecycle(t *testing.T) {
	t.Parallel()
	marked := Classify(testDesired, testSnapshot(nil, []LonghornNodeView{testLonghorn("orphan", nil)}, nil), testNow).Actions
	if !containsAction(marked, Action{Kind: KindMarkOrphan, Node: "orphan", Target: testTarget("lh-orphan"), At: nowISO}) {
		t.Fatalf("missing mark-orphan: %+v", marked)
	}
	holds := []struct {
		candidate LonghornNodeView
		reason    HoldReason
	}{
		{testLonghorn("replicas", func(l *LonghornNodeView) {
			l.OrphanSince, l.ReplicaCount = new("2026-10-06T09:00:00Z"), 1
		}), HoldLonghornReplicas},
		{testLonghorn("grace", func(l *LonghornNodeView) { l.OrphanSince = new("2026-10-06T11:59:00Z") }), HoldGrace},
		{testLonghorn("deleting", func(l *LonghornNodeView) {
			l.OrphanSince, l.DeletionTimestamp = new("2026-10-06T09:00:00Z"), new(nowISO)
		}), HoldLonghornDeleting},
	}
	for _, h := range holds {
		actions := Classify(testDesired, testSnapshot(nil, []LonghornNodeView{h.candidate}, nil), testNow).Actions
		if !containsAction(actions, Action{Kind: KindHold, Node: h.candidate.Name, Reason: h.reason}) {
			t.Errorf("%s: missing %s hold in %+v", h.candidate.Name, h.reason, actions)
		}
	}
	eligible := testLonghorn("eligible", func(l *LonghornNodeView) { l.OrphanSince = new("2026-10-06T09:00:00Z") })
	deleted := Classify(testDesired, testSnapshot(nil, []LonghornNodeView{eligible}, nil), testNow).Actions
	if !containsAction(deleted, Action{Kind: KindDeleteLonghornNode, Node: "eligible", Target: eligible.Target}) {
		t.Fatalf("missing orphan deletion: %+v", deleted)
	}
	returned := testLonghorn("member-a", func(l *LonghornNodeView) { l.OrphanSince = new("2026-10-06T09:00:00Z") })
	unmarked := Classify(testDesired, testSnapshot(nil, []LonghornNodeView{returned}, nil), testNow).Actions
	if !containsAction(unmarked, Action{Kind: KindUnmarkOrphan, Node: "member-a", Target: returned.Target}) {
		t.Fatalf("missing unmark-orphan: %+v", unmarked)
	}
}

// TestBelowMinimumOnlySafeActions maps "below minimum permits only safe restorative and protective actions; zero members means no actions".
func TestBelowMinimumOnlySafeActions(t *testing.T) {
	t.Parallel()
	protective := testNode("unknown-ready", nil)
	result := Classify(testDesired, testSnapshot(
		[]NodeView{testNode("member-a", func(n *NodeView) { n.Prior = &NodePrior{Unschedulable: true} }), testNode("member-b", nil), protective},
		[]LonghornNodeView{testLonghorn("member-a", func(l *LonghornNodeView) {
			l.Prior = &LonghornPrior{AllowScheduling: false, EvictionRequested: true}
		})}, nil), testNow)
	if !result.BelowMinNodes {
		t.Fatal("BelowMinNodes = false, want true")
	}
	for _, action := range result.Actions {
		if !action.SafeBelowMin() {
			t.Errorf("unsafe action below minimum: %+v", action)
		}
	}
	want := Action{Kind: KindCordon, Node: "unknown-ready", Target: protective.Target, NodePrior: &NodePrior{}, Protective: true}
	if !containsAction(result.Actions, want) {
		t.Fatalf("missing protective cordon: %+v", result.Actions)
	}
	empty := Classify(testDesired, testSnapshot([]NodeView{testNode("unknown-ready", nil)}, nil, nil), testNow)
	if !empty.BelowMinNodes || len(empty.Actions) != 0 {
		t.Fatalf("zero members: got %+v, want belowMinNodes and no actions", empty)
	}
}

// TestRetiringKeepsOrphanMarkBelowMinimum maps "retiring node keeps orphan mark below minimum".
func TestRetiringKeepsOrphanMarkBelowMinimum(t *testing.T) {
	t.Parallel()
	result := Classify(testDesired, testSnapshot(
		[]NodeView{testNode("member-a", nil), testNode("retiring-a", nil)},
		[]LonghornNodeView{testLonghorn("retiring-a", func(l *LonghornNodeView) { l.OrphanSince = new("2026-10-06T09:00:00Z") })},
		nil), testNow)
	if !result.BelowMinNodes {
		t.Fatal("BelowMinNodes = false, want true")
	}
	for _, action := range result.Actions {
		if action.Node == "retiring-a" {
			t.Errorf("unexpected retiring-a action below minimum: %+v", action)
		}
	}
}

// TestWriteOncePriorsSurviveRetireAndCancel maps "write-once priors survive retire, repeat runs, and cancellation of a human-cordoned Node".
func TestWriteOncePriorsSurviveRetireAndCancel(t *testing.T) {
	t.Parallel()
	state := rawCluster(rawOpts{unschedulable: true})
	fake := newFake(t, state)
	for range 3 {
		mustRun(t, runner(fake, testDesired, nil, nil), currentPlan(t, fake, testDesired))
	}
	lh := state.longhorn[0]
	if spec := lh["spec"]; !reflect.DeepEqual(spec, obj{"allowScheduling": false, "evictionRequested": true}) {
		t.Fatalf("longhorn spec = %+v", spec)
	}
	if got := objOrEmpty(metadataOf(lh)["annotations"])[longhornPriorKey]; got != `{"allowScheduling":true,"evictionRequested":false}` {
		t.Fatalf("longhorn prior = %v", got)
	}
	cancelled := withDesired(func(d *DesiredSet) {
		d.Retiring = []string{}
		d.Members = append(d.Members, "retiring-a")
	})
	mustRun(t, runner(fake, cancelled, nil, nil), currentPlan(t, fake, cancelled))
	if spec := lh["spec"]; !reflect.DeepEqual(spec, obj{"allowScheduling": true, "evictionRequested": false}) {
		t.Fatalf("restored longhorn spec = %+v", spec)
	}
	retiring := findNamed(state.nodes, "retiring-a")
	if got := metadataOf(retiring)["annotations"]; !reflect.DeepEqual(got, obj{nodeDrainedKey: nowISO}) {
		t.Fatalf("retiring-a annotations = %+v", got)
	}
	if objOrEmpty(retiring["spec"])["unschedulable"] != true {
		t.Fatal("human-cordoned retiring-a must stay unschedulable")
	}
}

// TestHumanCordonedUnknownStaysCordonedAsMember maps "an already human-cordoned unknown Ready Node stays cordoned when it becomes a member".
func TestHumanCordonedUnknownStaysCordonedAsMember(t *testing.T) {
	t.Parallel()
	state := rawCluster(rawOpts{unschedulable: true})
	unknown := rawNode("unknown", rawOpts{unschedulable: true})
	state.nodes = append(state.nodes, unknown)
	state.longhorn = []obj{}
	fake := newFake(t, state)
	unknownDesired := withDesired(func(d *DesiredSet) { d.Retiring = []string{} })
	for range 2 {
		mustRun(t, runner(fake, unknownDesired, nil, nil), currentPlan(t, fake, unknownDesired))
	}
	memberDesired := withDesired(func(d *DesiredSet) {
		d.Retiring = []string{}
		d.Members = append(d.Members, "unknown")
	})
	mustRun(t, runner(fake, memberDesired, nil, nil), currentPlan(t, fake, memberDesired))
	if objOrEmpty(unknown["spec"])["unschedulable"] != true {
		t.Fatal("unknown must stay unschedulable")
	}
	if got := metadataOf(unknown)["annotations"]; !reflect.DeepEqual(got, obj{}) {
		t.Fatalf("unknown annotations = %+v, want empty", got)
	}
}

// TestPodPinsAndOwnersHold maps "pod pins by nodeName and hostname, unknown owners hold; DaemonSet, mirror and Longhorn owners skip".
func TestPodPinsAndOwnersHold(t *testing.T) {
	t.Parallel()
	holds := []struct {
		pod    PodView
		reason HoldReason
	}{
		{testPod("nodeName", func(p *PodView) { p.PinnedTo = "retiring-a" }), HoldPinned},
		{testPod("hostname", func(p *PodView) { p.PinnedTo = "retiring-a" }), HoldPinned},
		{testPod("unreadable", func(p *PodView) { p.OwnerKnown = false }), HoldPinnedUnknown},
		{testPod("other-owner", func(p *PodView) { p.OwnerKnown = false }), HoldPinnedUnknown},
		{testPod("unmanaged", func(p *PodView) { p.Controller = false }), HoldUnmanaged},
	}
	for _, h := range holds {
		actions := Classify(testDesired, testSnapshot(cordonedNodes(), nil, []PodView{h.pod}), testNow).Actions
		want := Action{Kind: KindHold, Node: "retiring-a", Reason: h.reason, Pod: "workloads/" + h.pod.Name}
		if !containsAction(actions, want) {
			t.Errorf("%s: missing %+v in %+v", h.pod.Name, want, actions)
		}
	}
	exempt := []PodView{
		testPod("daemon", func(p *PodView) { p.DaemonSet = true }),
		testPod("mirror", func(p *PodView) { p.Mirror = true }),
		testPod("longhorn", func(p *PodView) { p.LonghornOwned = true }),
	}
	for _, pod := range exempt {
		actions := Classify(testDesired, testSnapshot(cordonedNodes(), nil, []PodView{pod}), testNow).Actions
		if containsKind(actions, KindEvictPod) {
			t.Errorf("%s: exempt pod evicted: %+v", pod.Name, actions)
		}
		for _, action := range actions {
			if action.Kind == KindHold && action.Node == "retiring-a" {
				t.Errorf("%s: exempt pod held: %+v", pod.Name, action)
			}
		}
	}
}

// TestActionKeyIsKindAndUID maps "action keys include action kind and target UID".
func TestActionKeyIsKindAndUID(t *testing.T) {
	t.Parallel()
	if got := (Action{Kind: KindDeleteNode, Node: "dead", Target: testTarget("dead")}).Key(); got != "delete-node/uid-dead" {
		t.Fatalf("Key = %q", got)
	}
}
