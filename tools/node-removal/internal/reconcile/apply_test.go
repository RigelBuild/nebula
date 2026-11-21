package reconcile

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// Ports apply.test.ts. Each test names the TS case it maps.

var errInvalidDesired = errors.New("invalid desired")

const webhookBody = `{"message":"admission webhook denied"}`

func dryRunActions() []Action {
	return []Action{
		{Kind: KindCordon, Node: "retiring-a", Target: testTarget("retiring-a")},
		{Kind: KindLonghornEvict, Node: "retiring-a", Target: testTarget("lh-retiring-a")},
		{Kind: KindLonghornRestore, Node: "retiring-a", Target: testTarget("lh-retiring-a"), LonghornPrior: &LonghornPrior{AllowScheduling: true}},
		{Kind: KindUncordon, Node: "retiring-a", Target: testTarget("retiring-a"), NodePrior: &NodePrior{}},
		{Kind: KindEvictPod, Node: "retiring-a", Namespace: "workloads", Name: "ordinary", Target: testTarget("pod-ordinary")},
		{Kind: KindMarkDrained, Node: "retiring-a", Target: testTarget("retiring-a")},
		{Kind: KindDeleteLonghornNode, Node: "orphan", Target: testTarget("lh-orphan")},
		{Kind: KindDeleteNode, Node: "dead", Target: testTarget("dead")},
		{Kind: KindMarkOrphan, Node: "orphan", Target: testTarget("lh-orphan"), At: nowISO},
		{Kind: KindUnmarkOrphan, Node: "orphan", Target: testTarget("lh-orphan")},
		{Kind: KindHold, Node: "held", Reason: HoldReady},
	}
}

func wantStale(t *testing.T, fake *fakeAPI, result Applied) {
	t.Helper()
	if result != (Applied{Hold: HoldStalePlan}) {
		t.Fatalf("result = %+v, want stale-plan hold", result)
	}
	if len(fake.writes) != 0 {
		t.Fatalf("writes = %+v, want none", fake.writes)
	}
}

func assertStaleNode(t *testing.T, mutate func(*fakeState)) {
	t.Helper()
	deadDesired := withDesired(func(d *DesiredSet) { d.Retiring = []string{} })
	state := rawCluster(rawOpts{unschedulable: true})
	state.nodes = []obj{rawNode("member-a", rawOpts{}), rawNode("member-b", rawOpts{}), rawNode("member-c", rawOpts{}),
		rawNode("dead", rawOpts{ready: "False", readySince: "2026-10-06T09:00:00Z"})}
	state.longhorn = []obj{}
	fake := newFake(t, state)
	deletion := findKind(t, currentPlan(t, fake, deadDesired).Actions, KindDeleteNode)
	mutate(state)
	wantStale(t, fake, mustApply(t, runner(fake, deadDesired, nil, nil), deletion))
}

func assertStalePods(t *testing.T) {
	t.Helper()
	for _, pinned := range []bool{false, true} {
		state := rawCluster(rawOpts{unschedulable: true})
		state.pods = []obj{rawPod("work", "uid-original")}
		fake := newFake(t, state)
		initial := findKind(t, currentPlan(t, fake, testDesired).Actions, KindEvictPod)
		if pinned {
			state.ownerTemplate = obj{"spec": obj{"template": obj{"spec": obj{"nodeName": "retiring-a"}}}}
		} else {
			state.pods[0] = rawPod("work", "uid-replacement")
		}
		wantStale(t, fake, mustApply(t, runner(fake, testDesired, nil, nil), initial))
	}
}

func orphanState() *fakeState {
	return &fakeState{
		nodes:    []obj{rawNode("member-a", rawOpts{}), rawNode("member-b", rawOpts{}), rawNode("member-c", rawOpts{})},
		longhorn: []obj{rawLonghorn("orphan", rawOpts{orphanSince: "2026-10-06T09:00:00Z"})},
		pods:     []obj{},
	}
}

func assertStaleOrphans(t *testing.T) {
	t.Helper()
	cleared := newFake(t, orphanState())
	orphanDelete := findKind(t, currentPlan(t, cleared, testDesired).Actions, KindDeleteLonghornNode)
	metadataOf(cleared.state.longhorn[0])["annotations"] = obj{}
	if got := mustApply(t, runner(cleared, testDesired, nil, nil), orphanDelete); got != (Applied{Hold: HoldStalePlan}) {
		t.Fatalf("cleared orphan mark: result = %+v, want stale-plan", got)
	}
	appeared := newFake(t, orphanState())
	appearedDelete := findKind(t, currentPlan(t, appeared, testDesired).Actions, KindDeleteLonghornNode)
	appeared.state.nodes = append(appeared.state.nodes, rawNode("orphan", rawOpts{}))
	if got := mustApply(t, runner(appeared, testDesired, nil, nil), appearedDelete); got != (Applied{Hold: HoldStalePlan}) {
		t.Fatalf("new Node: result = %+v, want stale-plan", got)
	}
}

// TestDryRunWritesNothing maps "dry-run performs no PATCH, POST or DELETE for every action".
func TestDryRunWritesNothing(t *testing.T) {
	t.Parallel()
	state := rawCluster(rawOpts{unschedulable: true})
	state.pods = []obj{rawPod("ordinary", "")}
	fake := newFake(t, state)
	dryRun := withDesired(func(d *DesiredSet) { d.Mode = ModeDryRun })
	for _, action := range dryRunActions() {
		mustApply(t, runner(fake, dryRun, nil, nil), action)
	}
	if len(fake.writes) != 0 {
		t.Fatalf("writes = %+v, want none", fake.writes)
	}
}

// TestPlanTimeHoldsLogDesiredMode maps "plan-time holds log the desired mode and never write".
func TestPlanTimeHoldsLogDesiredMode(t *testing.T) {
	t.Parallel()
	fake := newFake(t, rawCluster(rawOpts{}))
	hold := Action{Kind: KindHold, Node: "cp", Reason: HoldControlPlane}
	var out bytes.Buffer
	results := make([]Applied, 0, 3)
	for _, mode := range []Mode{ModeEnforce, ModeDryRun} {
		desired := withDesired(func(d *DesiredSet) { d.Mode = mode })
		results = append(results, mustApply(t, runner(fake, desired, nil, &out), hold))
	}
	results = append(results, mustApply(t, runner(fake, DesiredSet{}, errInvalidDesired, &out), hold))
	modes := make([]string, 0, 3)
	for line := range strings.SplitSeq(strings.TrimSpace(out.String()), "\n") {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		mode, _ := parsed["mode"].(string)
		modes = append(modes, mode)
	}
	if !reflect.DeepEqual(modes, []string{"enforce", "dry-run", "dry-run"}) {
		t.Fatalf("modes = %v", modes)
	}
	ok := Applied{OK: true}
	if !reflect.DeepEqual(results, []Applied{ok, ok, ok}) {
		t.Fatalf("results = %+v", results)
	}
	if len(fake.writes) != 0 {
		t.Fatalf("writes = %+v, want none", fake.writes)
	}
}

// TestLogLineKeyOrder pins logResult's key order: event, node, mode, then reason and pod, or result.
func TestLogLineKeyOrder(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	r := Runner{Out: &out}
	lines := []struct {
		action Action
		result Applied
	}{
		{Action{Kind: KindHold, Node: "n", Reason: HoldPinned, Pod: "ns/p"}, Applied{OK: true}},
		{Action{Kind: KindCordon, Node: "n"}, Applied{Hold: HoldConflict}},
		{Action{Kind: KindCordon, Node: "n"}, Applied{Skipped: "not-found"}},
		{Action{Kind: KindCordon, Node: "n"}, Applied{OK: true}},
	}
	for _, l := range lines {
		if err := r.logResult(l.action, ModeEnforce, l.result); err != nil {
			t.Fatalf("logResult: %v", err)
		}
	}
	want := `{"event":"hold","node":"n","mode":"enforce","reason":"pinned","pod":"ns/p"}
{"event":"hold","node":"n","mode":"enforce","reason":"conflict"}
{"event":"cordon","node":"n","mode":"enforce","result":"not-found"}
{"event":"cordon","node":"n","mode":"enforce","result":"ok"}
`
	if out.String() != want {
		t.Fatalf("log =\n%s\nwant\n%s", out.String(), want)
	}
}

// TestEncodeLogLineKeepsHTMLUnescaped pins log bytes to JSON.stringify: <, > and & stay literal.
func TestEncodeLogLineKeepsHTMLUnescaped(t *testing.T) {
	t.Parallel()
	encoded, err := EncodeLogLine(map[string]string{"event": "abort", "reason": "<a>&b"})
	if err != nil {
		t.Fatalf("EncodeLogLine: %v", err)
	}
	if want := `{"event":"abort","reason":"<a>&b"}`; string(encoded) != want {
		t.Fatalf("encoded = %s, want %s", encoded, want)
	}
}

// TestApplyTimeReplanningRejectsStale maps "apply-time replanning rejects Node Ready, grace, control-plane,
// replacement/pinned pods, cleared orphan marks, and new Nodes".
func TestApplyTimeReplanningRejectsStale(t *testing.T) {
	t.Parallel()
	assertStaleNode(t, func(s *fakeState) { s.nodes[3] = rawNode("dead", rawOpts{}) })
	assertStaleNode(t, func(s *fakeState) {
		s.nodes[3] = rawNode("dead", rawOpts{ready: "False", readySince: "2026-10-06T11:50:00Z"})
	})
	assertStaleNode(t, func(s *fakeState) {
		s.nodes[3] = rawNode("dead", rawOpts{ready: "False", readySince: "2026-10-06T09:00:00Z", labels: obj{controlPlaneLabel: ""}})
	})
	assertStalePods(t)
	assertStaleOrphans(t)
}

// TestCancellationAndInvalidDesiredHold maps "desired cancellation and invalid desired input hold without writes".
func TestCancellationAndInvalidDesiredHold(t *testing.T) {
	t.Parallel()
	fake := newFake(t, rawCluster(rawOpts{}))
	initial := findKind(t, currentPlan(t, fake, testDesired).Actions, KindLonghornEvict)
	cancelled := withDesired(func(d *DesiredSet) {
		d.Retiring = []string{}
		d.Members = append(d.Members, "retiring-a")
	})
	wantStale(t, fake, mustApply(t, runner(fake, cancelled, nil, nil), initial))
	if got := mustApply(t, runner(fake, DesiredSet{}, errInvalidDesired, nil), initial); got != (Applied{Hold: HoldInvalidDesired}) {
		t.Fatalf("invalid desired: result = %+v", got)
	}
	if len(fake.writes) != 0 {
		t.Fatalf("writes = %+v, want none", fake.writes)
	}
}

// TestCordonRefusalBlocksLaterDrain maps "cordon refusal leaves Node schedulable, so no later pod eviction or drained marker is planned".
func TestCordonRefusalBlocksLaterDrain(t *testing.T) {
	t.Parallel()
	state := rawCluster(rawOpts{})
	state.pods = []obj{rawPod("ordinary", "")}
	state.writeStatus, state.writeBody = 403, webhookBody
	fake := newFake(t, state)
	cordon := findKind(t, currentPlan(t, fake, testDesired).Actions, KindCordon)
	if got := mustApply(t, runner(fake, testDesired, nil, nil), cordon); got != (Applied{Hold: HoldWebhookRefused}) {
		t.Fatalf("result = %+v, want webhook-refused", got)
	}
	if objOrEmpty(findNamed(state.nodes, "retiring-a")["spec"])["unschedulable"] != false {
		t.Fatal("retiring-a must stay schedulable")
	}
	later := currentPlan(t, fake, testDesired).Actions
	if containsKind(later, KindEvictPod) || containsKind(later, KindMarkDrained) {
		t.Fatalf("later plan drains a schedulable Node: %+v", later)
	}
}

// TestMapsWriteStatuses maps "maps write 404, 409, eviction 429 and webhook refusal".
func TestMapsWriteStatuses(t *testing.T) {
	t.Parallel()
	cordon := Action{Kind: KindCordon, Node: "retiring-a", Target: testTarget("retiring-a")}
	cases := []struct {
		status int
		body   string
		want   Applied
	}{
		{404, "", Applied{Skipped: "not-found"}},
		{409, "", Applied{Hold: HoldConflict}},
		{403, webhookBody, Applied{Hold: HoldWebhookRefused}},
	}
	for _, c := range cases {
		state := rawCluster(rawOpts{})
		state.writeStatus, state.writeBody = c.status, c.body
		fake := newFake(t, state)
		if got := mustApply(t, runner(fake, testDesired, nil, nil), cordon); got != c.want {
			t.Errorf("HTTP %d: result = %+v, want %+v", c.status, got, c.want)
		}
	}
	state := rawCluster(rawOpts{unschedulable: true})
	state.pods = []obj{rawPod("ordinary", "")}
	state.writeStatus = 429
	fake := newFake(t, state)
	eviction := findKind(t, currentPlan(t, fake, testDesired).Actions, KindEvictPod)
	if got := mustApply(t, runner(fake, testDesired, nil, nil), eviction); got != (Applied{Hold: HoldEvictionRefused}) {
		t.Fatalf("eviction 429: result = %+v", got)
	}
}

func deadCluster(longhorn obj, writeStatus int, writeBody string) *fakeState {
	state := rawCluster(rawOpts{unschedulable: true})
	state.nodes[3] = rawNode("dead", rawOpts{ready: "False", readySince: "2026-10-06T09:00:00Z"})
	state.longhorn = []obj{longhorn}
	state.writeStatus, state.writeBody = writeStatus, writeBody
	return state
}

// TestRunStopsAfterWebhookRefusalAndHoldsPendingDeletion maps "run stops after webhook refusal deleting the
// Longhorn CR, and holds pending CR deletion".
func TestRunStopsAfterWebhookRefusalAndHoldsPendingDeletion(t *testing.T) {
	t.Parallel()
	deadDesired := withDesired(func(d *DesiredSet) { d.Retiring = []string{} })
	refused := newFake(t, deadCluster(rawLonghorn("dead", rawOpts{}), 403, webhookBody))
	mustRun(t, runner(refused, deadDesired, nil, nil), currentPlan(t, refused, deadDesired))
	if len(refused.writes) != 1 || refused.writes[0].method != "DELETE" ||
		!strings.Contains(refused.writes[0].path, "longhorn-system/nodes/dead") {
		t.Fatalf("writes = %+v, want one Longhorn DELETE for dead", refused.writes)
	}
	pending := newFake(t, deadCluster(rawLonghorn("dead", rawOpts{deletionTimestamp: nowISO}), 0, ""))
	pendingPlan := currentPlan(t, pending, deadDesired)
	if !containsAction(pendingPlan.Actions, Action{Kind: KindHold, Node: "dead", Reason: HoldLonghornDeleting}) {
		t.Fatalf("missing longhorn-deleting hold: %+v", pendingPlan.Actions)
	}
	mustRun(t, runner(pending, deadDesired, nil, nil), pendingPlan)
	if len(pending.writes) != 0 {
		t.Fatalf("writes = %+v, want none", pending.writes)
	}
}

// TestLonghornGet404PermitsLaterNodeDeletion maps "a fresh Longhorn GET 404 permits Node deletion on a later run".
func TestLonghornGet404PermitsLaterNodeDeletion(t *testing.T) {
	t.Parallel()
	deadDesired := withDesired(func(d *DesiredSet) { d.Retiring = []string{} })
	fake := newFake(t, deadCluster(rawLonghorn("dead", rawOpts{}), 0, ""))
	mustRun(t, runner(fake, deadDesired, nil, nil), currentPlan(t, fake, deadDesired))
	if len(fake.state.longhorn) != 0 {
		t.Fatalf("longhorn = %+v, want empty", fake.state.longhorn)
	}
	mustRun(t, runner(fake, deadDesired, nil, nil), currentPlan(t, fake, deadDesired))
	last := fake.writes[len(fake.writes)-1]
	if last.method != "DELETE" || last.path != "/api/v1/nodes/dead" {
		t.Fatalf("last write = %+v, want DELETE /api/v1/nodes/dead", last)
	}
}

// TestWrite404SkipsAndUnmapped500Stops maps "patch/eviction 404 skips that action; first unmapped 500 rejects before later calls".
func TestWrite404SkipsAndUnmapped500Stops(t *testing.T) {
	t.Parallel()
	actions := []Action{
		{Kind: KindCordon, Node: "retiring-a", Target: testTarget("retiring-a")},
		{Kind: KindLonghornEvict, Node: "retiring-a", Target: testTarget("lh-retiring-a")},
	}
	skipping := rawCluster(rawOpts{})
	skipping.writeStatus = 404
	fake := newFake(t, skipping)
	mustRun(t, runner(fake, testDesired, nil, nil), Plan{Actions: actions})
	if len(fake.writes) != 2 {
		t.Fatalf("writes = %d, want 2", len(fake.writes))
	}
	failing := rawCluster(rawOpts{})
	failing.writeStatus = 500
	failure := newFake(t, failing)
	err := runner(failure, testDesired, nil, nil).Run(Plan{Actions: actions})
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("Run error = %v, want HTTP 500", err)
	}
	if len(failure.writes) != 1 {
		t.Fatalf("writes = %d, want 1", len(failure.writes))
	}
}
