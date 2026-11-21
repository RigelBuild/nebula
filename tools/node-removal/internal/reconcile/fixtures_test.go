package reconcile

import (
	"bytes"
	"encoding/json"
	"maps"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/RigelBuild/nebula/tools/node-removal/internal/kube"
)

// Shared fixtures for the reconcile tests, ported from tests/fixtures.ts.
var (
	testNow     = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	testDesired = DesiredSet{
		Members:              []string{"member-a", "member-b", "member-c"},
		Retiring:             []string{"retiring-a"},
		Renamed:              []string{},
		Mode:                 ModeEnforce,
		MinNodes:             3,
		NotReadyGraceSeconds: 1800,
	}
)

const (
	longhornNodesPath = "/apis/longhorn.io/v1beta2/namespaces/longhorn-system/nodes"
	controlPlaneLabel = "node-role.kubernetes.io/control-plane"
	nowISO            = "2026-10-06T12:00:00.000Z"
)

func testTarget(id string) Target {
	return Target{UID: "uid-" + id, ResourceVersion: "rv-" + id}
}

func testNode(name string, mutate func(*NodeView)) NodeView {
	view := NodeView{
		Name:       name,
		Target:     testTarget(name),
		Ready:      "True",
		ReadySince: "2026-10-06T10:00:00Z",
	}
	if mutate != nil {
		mutate(&view)
	}
	return view
}

func testLonghorn(name string, mutate func(*LonghornNodeView)) LonghornNodeView {
	view := LonghornNodeView{
		Name:            name,
		Target:          testTarget("lh-" + name),
		AllowScheduling: true,
	}
	if mutate != nil {
		mutate(&view)
	}
	return view
}

func testPod(name string, mutate func(*PodView)) PodView {
	view := PodView{
		Namespace:  "workloads",
		Name:       name,
		Target:     testTarget("pod-" + name),
		Node:       "retiring-a",
		Controller: true,
		OwnerKnown: true,
	}
	if mutate != nil {
		mutate(&view)
	}
	return view
}

func testSnapshot(nodes []NodeView, longhorn []LonghornNodeView, pods []PodView) Snapshot {
	if nodes == nil {
		nodes = []NodeView{
			testNode("member-a", nil),
			testNode("member-b", nil),
			testNode("member-c", nil),
			testNode("retiring-a", nil),
		}
	}
	if longhorn == nil {
		longhorn = []LonghornNodeView{testLonghorn("retiring-a", nil)}
	}
	return Snapshot{Nodes: nodes, Longhorn: longhorn, Pods: pods}
}

func kinds(actions []Action) []ActionKind {
	out := make([]ActionKind, 0, len(actions))
	for _, action := range actions {
		out = append(out, action.Kind)
	}
	return out
}

func containsKind(actions []Action, kind ActionKind) bool {
	return slices.Contains(kinds(actions), kind)
}

// rawOpts holds the optional fields of the raw Node and Longhorn builders.
type rawOpts struct {
	ready, readySince, orphanSince, deletionTimestamp, prior string
	labels                                                   obj
	unschedulable, evictionRequested, blockScheduling        bool
	replicaCount                                             int
}

func rawNode(name string, o rawOpts) obj {
	labels := o.labels
	if labels == nil {
		labels = obj{}
	}
	ready, since := o.ready, o.readySince
	if ready == "" {
		ready = "True"
	}
	if since == "" {
		since = "2026-10-06T10:00:00Z"
	}
	return obj{
		"metadata": obj{"name": name, "uid": "uid-" + name, "resourceVersion": "rv-" + name, "labels": labels, "annotations": obj{}},
		"spec":     obj{"unschedulable": o.unschedulable},
		"status":   obj{"conditions": []any{obj{"type": "Ready", "status": ready, "lastTransitionTime": since}}},
	}
}

func rawLonghorn(name string, o rawOpts) obj {
	replicas := obj{}
	for i := range o.replicaCount {
		replicas["replica-"+string(rune('0'+i))] = obj{}
	}
	annotations := obj{}
	if o.orphanSince != "" {
		annotations[orphanSinceKey] = o.orphanSince
	}
	if o.prior != "" {
		annotations[longhornPriorKey] = o.prior
	}
	metadata := obj{"name": name, "uid": "uid-lh-" + name, "resourceVersion": "rv-lh-" + name, "annotations": annotations}
	if o.deletionTimestamp != "" {
		metadata["deletionTimestamp"] = o.deletionTimestamp
	}
	return obj{
		"metadata": metadata,
		"spec":     obj{"allowScheduling": !o.blockScheduling, "evictionRequested": o.evictionRequested},
		"status":   obj{"diskStatus": obj{"default": obj{"scheduledReplica": replicas}}},
	}
}

func rawPod(name, uid string) obj {
	if uid == "" {
		uid = "uid-pod-" + name
	}
	return obj{
		"metadata": obj{
			"name": name, "namespace": "workloads", "uid": uid, "resourceVersion": "rv-pod-" + name,
			"ownerReferences": []any{obj{"apiVersion": "batch/v1", "kind": "Job", "name": "owner-job", "controller": true}},
		},
		"spec": obj{"nodeName": "retiring-a"},
	}
}

func metadataOf(value obj) obj { return objOrEmpty(value["metadata"]) }

func findNamed(items []obj, name string) obj {
	for _, item := range items {
		if metadataOf(item)["name"] == name {
			return item
		}
	}
	return nil
}

// fakeState is the fake apiserver's objects plus an optional forced write status.
type fakeState struct {
	nodes, longhorn, pods []obj
	ownerTemplate         obj
	writeStatus           int
	writeBody             string
}

type writeRecord struct {
	method, path string
	body         obj
}

// fakeAPI is the Go port of fakeApi: a kube.Client over in-memory objects that records writes.
type fakeAPI struct {
	t      *testing.T
	state  *fakeState
	writes []writeRecord
}

func newFake(t *testing.T, state *fakeState) *fakeAPI {
	t.Helper()
	return &fakeAPI{t: t, state: state}
}

func lastSegment(path string) string {
	name, err := url.PathUnescape(path[strings.LastIndex(path, "/")+1:])
	if err != nil {
		return ""
	}
	return name
}

func (f *fakeAPI) Get(path string) (kube.Response, error) {
	switch {
	case path == "/api/v1/nodes":
		return f.list(f.state.nodes), nil
	case path == longhornNodesPath:
		return f.list(f.state.longhorn), nil
	case strings.HasPrefix(path, longhornNodesPath+"/"):
		if found := findNamed(f.state.longhorn, lastSegment(path)); found != nil {
			return f.json(found), nil
		}
		return kube.Response{Status: 404}, nil
	case strings.HasPrefix(path, "/api/v1/pods?"):
		query, err := url.ParseQuery(path[strings.Index(path, "?")+1:])
		if err != nil {
			f.t.Fatalf("parsing pod query: %v", err)
		}
		node := strings.TrimPrefix(query.Get("fieldSelector"), "spec.nodeName=")
		selected := []obj{}
		for _, pod := range f.state.pods {
			if objOrEmpty(pod["spec"])["nodeName"] == node {
				selected = append(selected, pod)
			}
		}
		return f.list(selected), nil
	case strings.HasPrefix(path, "/apis/apps/"), strings.HasPrefix(path, "/apis/batch/"):
		if f.state.ownerTemplate != nil {
			return f.json(f.state.ownerTemplate), nil
		}
		return f.json(obj{"spec": obj{"template": obj{"spec": obj{}}}}), nil
	}
	return kube.Response{Status: 404}, nil
}

func (f *fakeAPI) Patch(path string, body any) (kube.Response, error) {
	patch, failure := f.record("PATCH", path, body)
	if failure != nil {
		return *failure, nil
	}
	raw := findNamed(*f.collection(path), lastSegment(path))
	if raw == nil {
		return kube.Response{Status: 404}, nil
	}
	metadata := metadataOf(raw)
	annotations := objOrEmpty(metadata["annotations"])
	for key, value := range objOrEmpty(metadataOf(patch)["annotations"]) {
		if value == nil {
			delete(annotations, key)
		} else {
			annotations[key] = value
		}
	}
	metadata["annotations"] = annotations
	raw["metadata"] = metadata
	spec := objOrEmpty(raw["spec"])
	maps.Copy(spec, objOrEmpty(patch["spec"]))
	raw["spec"] = spec
	return kube.Response{Status: 200}, nil
}

func (f *fakeAPI) Post(path string, body any) (kube.Response, error) {
	if _, failure := f.record("POST", path, body); failure != nil {
		return *failure, nil
	}
	start := strings.Index(path, "/pods/") + len("/pods/")
	name, err := url.PathUnescape(path[start:strings.LastIndex(path, "/eviction")])
	if err != nil {
		f.t.Fatalf("decoding eviction path: %v", err)
	}
	f.state.pods = slices.DeleteFunc(f.state.pods, func(pod obj) bool {
		return metadataOf(pod)["name"] == name && strings.HasPrefix(path, "/api/v1/namespaces/")
	})
	return kube.Response{Status: 200}, nil
}

func (f *fakeAPI) Delete(path string, body any) (kube.Response, error) {
	if _, failure := f.record("DELETE", path, body); failure != nil {
		return *failure, nil
	}
	items := f.collection(path)
	name := lastSegment(path)
	index := slices.IndexFunc(*items, func(item obj) bool { return metadataOf(item)["name"] == name })
	if index < 0 {
		return kube.Response{Status: 404}, nil
	}
	*items = slices.Delete(*items, index, index+1)
	return kube.Response{Status: 200}, nil
}

// record stores the write with its body decoded from JSON, as the apiserver would see it.
func (f *fakeAPI) record(method, path string, body any) (obj, *kube.Response) {
	encoded, err := json.Marshal(body)
	if err != nil {
		f.t.Fatalf("encoding write body: %v", err)
	}
	var decoded obj
	if err := json.NewDecoder(bytes.NewReader(encoded)).Decode(&decoded); err != nil {
		f.t.Fatalf("decoding write body: %v", err)
	}
	f.writes = append(f.writes, writeRecord{method: method, path: path, body: decoded})
	if f.state.writeStatus != 0 && f.state.writeStatus != 200 {
		return decoded, &kube.Response{Status: f.state.writeStatus, Body: []byte(f.state.writeBody)}
	}
	return decoded, nil
}

func (f *fakeAPI) collection(path string) *[]obj {
	if strings.HasPrefix(path, "/apis/longhorn.io/") {
		return &f.state.longhorn
	}
	return &f.state.nodes
}
func (f *fakeAPI) json(value any) kube.Response {
	encoded, err := json.Marshal(value)
	if err != nil {
		f.t.Fatalf("encoding fake response: %v", err)
	}
	return kube.Response{Status: 200, Body: encoded}
}

func (f *fakeAPI) list(items []obj) kube.Response {
	out := make([]any, 0, len(items))
	for _, item := range items {
		out = append(out, item)
	}
	return f.json(obj{"apiVersion": "v1", "kind": "List", "items": out})
}

// rawCluster is the default fake cluster: three members, retiring-a, and its Longhorn CR.
func rawCluster(retiring rawOpts) *fakeState {
	return &fakeState{
		nodes:    []obj{rawNode("member-a", rawOpts{}), rawNode("member-b", rawOpts{}), rawNode("member-c", rawOpts{}), rawNode("retiring-a", retiring)},
		longhorn: []obj{rawLonghorn("retiring-a", rawOpts{})},
		pods:     []obj{},
	}
}

func currentPlan(t *testing.T, client kube.Client, desired DesiredSet) Plan {
	t.Helper()
	snap, err := Read(desired, client)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return Classify(desired, snap, testNow)
}

// runner is fixedContext: a fixed clock and a desired set, or a desired-set read error.
func runner(client kube.Client, desired DesiredSet, readErr error, out *bytes.Buffer) Runner {
	if out == nil {
		out = &bytes.Buffer{}
	}
	return Runner{
		ReadDesired: func() (DesiredSet, error) { return desired, readErr },
		Now:         func() time.Time { return testNow },
		Client:      client,
		Out:         out,
	}
}

func mustApply(t *testing.T, r Runner, action Action) Applied {
	t.Helper()
	result, err := r.Apply(action)
	if err != nil {
		t.Fatalf("Apply(%s): %v", action.Kind, err)
	}
	return result
}

func mustRun(t *testing.T, r Runner, p Plan) {
	t.Helper()
	if err := r.Run(p); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func findKind(t *testing.T, actions []Action, kind ActionKind) Action {
	t.Helper()
	for _, action := range actions {
		if action.Kind == kind {
			return action
		}
	}
	t.Fatalf("expected planned %s in %+v", kind, actions)
	return Action{}
}

func withDesired(mutate func(*DesiredSet)) DesiredSet {
	desired := testDesired
	desired.Members = slices.Clone(testDesired.Members)
	mutate(&desired)
	return desired
}
