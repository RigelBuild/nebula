package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// controlFake answers the kubectl calls the control path makes. It keeps
// StatefulSet nemo and the maintenance objects, and it refuses a create over an
// existing object, as the API server does.
type controlFake struct {
	sts     map[string]any
	objects map[string]map[string]any // keyed "pod/nemo-move-maint", "networkpolicy/nemo-move-maint"
	calls   []string
	creates int
}

func newControlFake(replicas int) *controlFake {
	return &controlFake{
		sts: map[string]any{
			"metadata": map[string]any{"name": "nemo", "namespace": "nemo"},
			"spec": map[string]any{
				"replicas": float64(replicas),
				"template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "hermes", "image": "nemo:1"}}}},
			},
		},
		objects: map[string]map[string]any{},
	}
}

func (f *controlFake) replicas() int {
	return int(mapField(f.sts, "spec")["replicas"].(float64))
}

func (f *controlFake) annotation(key string) (string, bool) {
	v, ok := mapField(mapField(f.sts, "metadata"), "annotations")[key].(string)
	return v, ok
}

func (f *controlFake) run(_ context.Context, input io.Reader, args ...string) ([]byte, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	if len(args) >= 2 && args[0] == "get" && args[1] == "statefulsets" {
		return json.Marshal(map[string]any{"items": []any{f.sts}})
	}
	if len(args) < 3 || args[0] != "-n" || args[1] != "nemo" {
		return nil, fmt.Errorf("unexpected kubectl %v", args)
	}
	args = args[2:]
	switch args[0] {
	case "get":
		if args[1] == "statefulset" {
			return json.Marshal(f.sts)
		}
		obj, ok := f.objects[args[1]+"/"+args[2]]
		if !ok {
			return nil, nil
		}
		return json.Marshal(obj)
	case "annotate":
		return nil, f.annotate(args[3])
	case "scale":
		n, err := strconv.Atoi(strings.TrimPrefix(args[2], "--replicas="))
		if err != nil {
			return nil, err
		}
		mapField(f.sts, "spec")["replicas"] = float64(n)
		return nil, nil
	case "create":
		return nil, f.create(input)
	case "delete":
		delete(f.objects, args[1]+"/"+args[2])
		return nil, nil
	case "wait":
		if args[1] == "--for=condition=Ready" {
			if _, ok := f.objects[args[2]]; !ok {
				return nil, fmt.Errorf("%s is not ready", args[2])
			}
		}
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected kubectl %v", args)
}

// annotate sets KEY=VALUE, or removes KEY when the argument ends in "-".
func (f *controlFake) annotate(kv string) error {
	meta := mapField(f.sts, "metadata")
	ann, _ := meta["annotations"].(map[string]any)
	if ann == nil {
		ann = map[string]any{}
		meta["annotations"] = ann
	}
	if key, ok := strings.CutSuffix(kv, "-"); ok {
		if _, present := ann[key]; !present {
			return fmt.Errorf("annotation %s not found", key)
		}
		delete(ann, key)
		return nil
	}
	key, value, _ := strings.Cut(kv, "=")
	ann[key] = value
	return nil
}

func (f *controlFake) create(input io.Reader) error {
	data, err := io.ReadAll(input)
	if err != nil {
		return err
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	key := strings.ToLower(stringField(obj, "kind")) + "/" + stringField(mapField(obj, "metadata"), "name")
	if _, exists := f.objects[key]; exists {
		return fmt.Errorf("%s already exists", key)
	}
	f.objects[key] = obj
	f.creates++
	return nil
}

// countControlCalls counts the calls that run verb.
func countControlCalls(calls []string, verb string) int {
	n := 0
	for _, call := range calls {
		if strings.Contains(call, " "+verb+" ") {
			n++
		}
	}
	return n
}

// liveControlCopy returns obj as the API server stores it: the same spec plus the
// defaults that apiDefaults and the empty-object rule allow.
func liveControlCopy(t *testing.T, obj map[string]any, pod bool) map[string]any {
	t.Helper()
	live, err := canonicalMap(obj)
	if err != nil {
		t.Fatal(err)
	}
	mapField(live, "metadata")["uid"] = "uid-1"
	if pod {
		live["status"] = map[string]any{"phase": "Running"}
		spec := mapField(live, "spec")
		spec["dnsPolicy"] = "ClusterFirst"
		spec["schedulerName"] = "default-scheduler"
		for _, c := range spec["containers"].([]any) {
			container := c.(map[string]any)
			container["imagePullPolicy"] = "IfNotPresent"
			container["terminationMessagePath"] = "/dev/termination-log"
			container["resources"] = map[string]any{}
		}
	}
	return live
}

func controlPodImage(pod map[string]any) string {
	containers := mapField(pod, "spec")["containers"].([]any)
	return stringField(containers[0].(map[string]any), "image")
}

func TestCutoverResumeRefusesStartedRecord(t *testing.T) {
	f := &fakeCutover{id: "run", state: "started", phase: "blocked"}
	args := []string{"--kubeconfig", "config", "--mac", "example@192.0.2.10", "--home-root", "/Users/example", "--ssh-user", "example", "--launchd-label", "com.example.agent:gui", "--mac-binary", "mac-binary", "--resume", "run"}
	err := cutover(t.Context(), args, cutoverDeps{ops: f, newID: func() (string, error) { return "new", nil }, fault: f.inject}, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("--resume accepted a started record")
	}
	if want := []string{"open", "record", "close"}; !reflect.DeepEqual(f.events, want) {
		t.Fatalf("events = %v, want %v", f.events, want)
	}
}

func TestCutoverRerunReusesMatchingControlObjects(t *testing.T) {
	f := newControlFake(1)
	f.objects["pod/"+maintName] = liveControlCopy(t, maintPodObject("nemo", "nemo:1"), true)
	f.objects["networkpolicy/"+maintName] = liveControlCopy(t, maintPolicyObject("nemo"), false)
	p := &productionCutover{ctl: controlPath{run: f.run}}
	if err := p.open(t.Context()); err != nil {
		t.Fatalf("open: %v", err)
	}
	if f.creates != 0 || countControlCalls(f.calls, "delete") != 0 {
		t.Fatalf("rerun touched matching objects: creates=%d calls=%v", f.creates, f.calls)
	}
	if f.replicas() != 0 {
		t.Fatalf("open left replicas at %d, want 0", f.replicas())
	}
	if err := p.close(t.Context()); err != nil {
		t.Fatalf("close: %v", err)
	}
	if len(f.objects) != 0 {
		t.Fatalf("close left objects behind: %v", f.objects)
	}
	if f.replicas() != 1 {
		t.Fatalf("close restored replicas to %d, want 1", f.replicas())
	}
	if _, ok := f.annotation(replicasAnnotation); ok {
		t.Fatal("close kept the replicas annotation")
	}
}

func TestCutoverReplacesFailedMaintenancePod(t *testing.T) {
	f := newControlFake(1)
	pod := liveControlCopy(t, maintPodObject("nemo", "nemo:1"), true)
	pod["status"] = map[string]any{"phase": "Failed"}
	f.objects["pod/"+maintName] = pod
	f.objects["networkpolicy/"+maintName] = liveControlCopy(t, maintPolicyObject("nemo"), false)
	p := &productionCutover{ctl: controlPath{run: f.run}}
	if err := p.open(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.creates != 1 || countControlCalls(f.calls, "delete") != 1 {
		t.Fatalf("creates=%d calls=%v", f.creates, f.calls)
	}
}

func TestCutoverRerunReplacesDriftedControlObjects(t *testing.T) {
	f := newControlFake(1)
	f.objects["pod/"+maintName] = liveControlCopy(t, maintPodObject("nemo", "nemo:old"), true)
	drifted := maintPolicyObject("nemo")
	mapField(drifted, "spec")["policyTypes"] = []string{"Ingress"}
	f.objects["networkpolicy/"+maintName] = liveControlCopy(t, drifted, false)
	p := &productionCutover{ctl: controlPath{run: f.run}}
	if err := p.open(t.Context()); err != nil {
		t.Fatalf("open: %v", err)
	}
	if got := controlPodImage(f.objects["pod/"+maintName]); got != "nemo:1" {
		t.Fatalf("pod image = %q, want nemo:1", got)
	}
	if got := len(mapField(f.objects["networkpolicy/"+maintName], "spec")["policyTypes"].([]any)); got != 2 {
		t.Fatalf("policyTypes has %d entries, want 2", got)
	}
	if n := countControlCalls(f.calls, "delete"); n != 2 {
		t.Fatalf("deletes = %d, want 2 (drifted pod and policy)", n)
	}
}

func TestCutoverRerunRestoresReplicasSeenAtFirstOpen(t *testing.T) {
	f := newControlFake(3)
	first := &productionCutover{ctl: controlPath{run: f.run}}
	if err := first.open(t.Context()); err != nil {
		t.Fatalf("first open: %v", err)
	}
	// The first run dies here, before close.
	second := &productionCutover{ctl: controlPath{run: f.run}}
	if err := second.open(t.Context()); err != nil {
		t.Fatalf("second open: %v", err)
	}
	if err := second.close(t.Context()); err != nil {
		t.Fatalf("close: %v", err)
	}
	if f.replicas() != 3 {
		t.Fatalf("replicas = %d, want 3 as seen at the first open", f.replicas())
	}
	if _, ok := f.annotation(replicasAnnotation); ok {
		t.Fatal("close kept the replicas annotation")
	}
}

func TestControlPathCloseWithoutOpenKeepsReplicas(t *testing.T) {
	f := newControlFake(0)
	c := &controlPath{run: f.run, namespace: "nemo"}
	if err := c.close(t.Context()); err != nil {
		t.Fatalf("close: %v", err)
	}
	if f.replicas() != 0 || countControlCalls(f.calls, "scale") != 0 {
		t.Fatalf("close without open touched replicas: %d, calls %v", f.replicas(), f.calls)
	}
}

func TestRestoreClearsReplicasAnnotation(t *testing.T) {
	for _, present := range []bool{false, true} {
		t.Run(strconv.FormatBool(present), func(t *testing.T) {
			f := newControlFake(0)
			if present {
				if err := f.annotate(replicasAnnotation + "=3"); err != nil {
					t.Fatal(err)
				}
			}
			k := &restoreKube{namespace: "nemo", run: func(ctx context.Context, input []byte, args ...string) ([]byte, error) {
				return f.run(ctx, bytes.NewReader(input), args...)
			}}
			if err := k.clearReplicasAnnotation(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, ok := f.annotation(replicasAnnotation); ok {
				t.Fatal("stale annotation remains")
			}
		})
	}
}
