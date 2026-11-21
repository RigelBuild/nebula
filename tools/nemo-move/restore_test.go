package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/RigelBuild/nebula/tools/nemo-move/internal/phase"
)

type restoreFake struct {
	calls          []string
	record         phase.RestorePrepareRecord
	replicas       int
	owner, lostAck bool
	fault          error
}

func newRestoreFake() *restoreFake { return &restoreFake{} }
func (f *restoreFake) deps() restoreDeps {
	return restoreDeps{backend: func(string) restoreBackend { return f }, nonce: func() (string, error) { return "random-nonce", nil }, fault: func(string) error { return f.fault }}
}
func (f *restoreFake) deleteNode(context.Context, string) error {
	f.calls = append(f.calls, "delete-node")
	return nil
}
func (f *restoreFake) scale(_ context.Context, n int) error {
	f.replicas = n
	if n == 0 {
		f.calls = append(f.calls, "scale-0")
	} else {
		f.calls = append(f.calls, "scale-1")
	}
	return nil
}
func (f *restoreFake) retainDeleteClaims(context.Context, string, string) error {
	f.calls = append(f.calls, "retain-delete-claims")
	return nil
}
func (f *restoreFake) waitRestored(_ context.Context, kind string) error {
	f.calls = append(f.calls, "wait-"+kind)
	return nil
}
func (f *restoreFake) bindClaims(context.Context, string, string) error {
	f.calls = append(f.calls, "bind-claims")
	return nil
}
func (f *restoreFake) restoreBackup(_ context.Context, kind, _ string) (string, error) {
	f.calls = append(f.calls, "restore-"+kind)
	return kind + "-time", nil
}
func (f *restoreFake) open(context.Context) error { f.calls = append(f.calls, "open"); return nil }
func (f *restoreFake) writePhase(_ context.Context, v phase.Value) error {
	f.owner = v == phase.Owner
	if f.owner {
		f.calls = append(f.calls, "owner")
		if f.lostAck {
			return errors.New("lost ack")
		}
	} else {
		f.calls = append(f.calls, "blocked")
	}
	return nil
}
func (f *restoreFake) readPhase(context.Context) (phase.Value, error) {
	f.calls = append(f.calls, "fresh-read-owner")
	if f.owner {
		return phase.Owner, nil
	}
	return phase.Blocked, nil
}
func (f *restoreFake) writeRecord(_ context.Context, r phase.RestorePrepareRecord) error {
	f.calls = append(f.calls, "write-record")
	f.record = r
	return nil
}
func (f *restoreFake) readRecord(context.Context) (phase.RestorePrepareRecord, error) {
	f.calls = append(f.calls, "read-record")
	return f.record, nil
}
func (f *restoreFake) close(context.Context) error                   { f.calls = append(f.calls, "close"); return nil }
func (f *restoreFake) clearReplicasAnnotation(context.Context) error { return nil }
func containsRestoreCall(calls []string, s string) bool {
	for _, call := range calls {
		if call == s {
			return true
		}
	}
	return false
}

func TestRestorePrepare(t *testing.T) {
	args := []string{"--kubeconfig", "config", "--node-confirmed-off", "node", "--backup-data", "data", "--backup-control", "control"}
	t.Run("confirmation required", func(t *testing.T) {
		f := newRestoreFake()
		if err := restore(context.Background(), []string{"--kubeconfig", "config", "--backup-data", "data", "--backup-control", "control"}, f.deps(), &bytes.Buffer{}); err == nil || len(f.calls) != 0 {
			t.Fatalf("err=%v calls=%v", err, f.calls)
		}
	})
	t.Run("ordered prepare", func(t *testing.T) {
		f := newRestoreFake()
		var out bytes.Buffer
		if err := restore(context.Background(), args, f.deps(), &out); err != nil {
			t.Fatal(err)
		}
		want := []string{"delete-node", "scale-0", "retain-delete-claims", "restore-data", "wait-data", "restore-control", "wait-control", "bind-claims", "open", "blocked", "write-record", "read-record", "close"}
		if !reflect.DeepEqual(f.calls, want) {
			t.Fatalf("calls=%v want=%v", f.calls, want)
		}
		if f.record.BackupData != "data" || f.record.BackupControl != "control" || f.record.Nonce == "" {
			t.Fatalf("record=%+v", f.record)
		}
		for _, s := range []string{"data-time", "control-time", f.record.Nonce} {
			if !strings.Contains(out.String(), s) {
				t.Fatalf("output %q missing %q", out.String(), s)
			}
		}
	})
	t.Run("mid restore fault", func(t *testing.T) {
		f := newRestoreFake()
		f.fault = errors.New("mid-restore")
		if err := restore(context.Background(), args, f.deps(), &bytes.Buffer{}); !errors.Is(err, f.fault) {
			t.Fatalf("err=%v", err)
		}
		if f.replicas != 0 || f.owner {
			t.Fatalf("replicas=%d owner=%v", f.replicas, f.owner)
		}
	})
}

func TestRestoreCommit(t *testing.T) {
	t.Run("invalid arguments cause no mutations", func(t *testing.T) {
		for _, args := range [][]string{
			{"--commit", "--kubeconfig", "config", "--backup-data", "data", "--backup-control", "control"},
			{"--commit", "--kubeconfig", "config", "--backup-data", "data", "--backup-control", "control", "--nonce", "nonce", "--node-confirmed-off", "node"},
		} {
			f := newRestoreFake()
			if err := restore(context.Background(), args, f.deps(), &bytes.Buffer{}); err == nil || len(f.calls) != 0 {
				t.Fatalf("args=%v err=%v calls=%v", args, err, f.calls)
			}
		}
	})
	for _, tc := range []struct {
		name, data, control, nonce string
		match                      bool
	}{
		{"data", "wrong", "control", "nonce", false}, {"control", "data", "wrong", "nonce", false}, {"nonce", "data", "control", "wrong", false}, {"match", "data", "control", "nonce", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRestoreFake()
			f.record.BackupData = "data"
			f.record.BackupControl = "control"
			f.record.Nonce = "nonce"
			err := restore(context.Background(), []string{"--commit", "--kubeconfig", "config", "--backup-data", tc.data, "--backup-control", tc.control, "--nonce", tc.nonce}, f.deps(), &bytes.Buffer{})
			if tc.match {
				if err != nil || !f.owner || f.replicas != 1 {
					t.Fatalf("err=%v owner=%v replicas=%d", err, f.owner, f.replicas)
				}
			} else if err == nil || f.owner || f.replicas != 0 {
				t.Fatalf("err=%v owner=%v replicas=%d", err, f.owner, f.replicas)
			}
			if !tc.match && (containsRestoreCall(f.calls, "open") || containsRestoreCall(f.calls, "scale-0")) {
				t.Fatalf("mismatch opened maintenance or scaled down: %v", f.calls)
			}
			if tc.match && containsRestoreCall(f.calls, "open") {
				t.Fatalf("commit reopened maintenance: %v", f.calls)
			}
			for _, call := range f.calls {
				if strings.Contains(call, "mac") || call == "delete-node" {
					t.Fatalf("unexpected call %q", call)
				}
			}
		})
	}
	t.Run("lost acknowledgement", func(t *testing.T) {
		f := newRestoreFake()
		f.record.BackupData = "data"
		f.record.BackupControl = "control"
		f.record.Nonce = "nonce"
		f.lostAck = true
		if err := restore(context.Background(), []string{"--commit", "--kubeconfig", "config", "--backup-data", "data", "--backup-control", "control", "--nonce", "nonce"}, f.deps(), &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		if !containsRestoreCall(f.calls, "fresh-read-owner") || f.replicas != 1 {
			t.Fatalf("calls=%v replicas=%d", f.calls, f.replicas)
		}
	})
}

func TestRestoreBackendRejectsForeignBackup(t *testing.T) {
	k := &restoreKube{volumes: map[string]*restoreVolume{"data": {oldHandle: "source"}}}
	k.run = func(_ context.Context, _ []byte, _ ...string) ([]byte, error) {
		return []byte(`{"status":{"state":"Completed","url":"s3://backup","volumeSize":"1024","backupCreatedAt":"time","volumeName":"other"}}`), nil
	}
	if _, err := k.restoreBackup(context.Background(), "data", "backup"); err == nil {
		t.Fatal("foreign backup accepted")
	}
}

func TestRestoreBackupResumesLostCreateResponse(t *testing.T) {
	var created map[string]any
	creates := 0
	run := func(_ context.Context, input []byte, args ...string) ([]byte, error) {
		call := strings.Join(args, " ")
		switch {
		case strings.Contains(call, "get backups.longhorn.io"):
			return []byte(`{"status":{"state":"Completed","url":"s3://backup","volumeSize":"1024","backupCreatedAt":"time","volumeName":"source"}}`), nil
		case strings.Contains(call, "get volumes.longhorn.io"):
			if created == nil {
				return []byte(""), nil
			}
			return json.Marshal(created)
		case call == "create -f -":
			creates++
			if err := json.Unmarshal(input, &created); err != nil {
				return nil, err
			}
			return nil, errors.New("response lost")
		default:
			return nil, fmt.Errorf("unexpected command %s", call)
		}
	}
	newBackend := func() *restoreKube {
		return &restoreKube{run: run, volumes: map[string]*restoreVolume{"data": {oldHandle: "source", longhorn: map[string]any{}}}}
	}
	if _, err := newBackend().restoreBackup(context.Background(), "data", "backup"); err == nil {
		t.Fatal("expected lost response")
	}
	if _, err := newBackend().restoreBackup(context.Background(), "data", "backup"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if creates != 1 {
		t.Fatalf("created %d volumes, want one", creates)
	}
}

func TestRestoreAbsentClaimRejectsLegacyPV(t *testing.T) {
	k := &restoreKube{namespace: "nemo", volumes: map[string]*restoreVolume{}}
	k.run = func(_ context.Context, _ []byte, args ...string) ([]byte, error) {
		call := strings.Join(args, " ")
		switch {
		case strings.Contains(call, "delete pod nemo-move-maint"):
			return nil, nil
		case strings.Contains(call, "get pvc"):
			return []byte(""), nil
		case strings.Contains(call, "get pv"):
			return []byte(`{"items":[{"metadata":{"name":"legacy-pv"},"spec":{"persistentVolumeReclaimPolicy":"Retain","claimRef":{"namespace":"nemo","name":"nemo-data"}}}]}`), nil
		default:
			return nil, fmt.Errorf("unexpected command %s", call)
		}
	}
	err := k.retainDeleteClaims(context.Background(), "data", "control")
	if err == nil || !strings.Contains(err.Error(), "legacy-pv") {
		t.Fatalf("error = %v, want legacy-pv", err)
	}
}

func TestRestoreResumesAfterFreshPVCreate(t *testing.T) {
	name := restoreVolumeName("nemo", "data", "backup")
	saved := map[string]any{"pvc": map[string]any{"kind": "PersistentVolumeClaim", "metadata": map[string]any{"name": "nemo-data"}, "spec": map[string]any{"volumeName": "old"}}, "pv": map[string]any{"kind": "PersistentVolume", "metadata": map[string]any{"name": "old"}, "spec": map[string]any{"csi": map[string]any{"driver": "driver.longhorn.io", "volumeHandle": "source"}}}, "longhorn": map[string]any{"numberOfReplicas": float64(1)}, "oldHandle": "source"}
	encoded, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	ref := map[string]any{"namespace": "nemo", "name": "nemo-data"}
	old := map[string]any{"metadata": map[string]any{"name": "old", "annotations": map[string]any{restoreAnnotation: string(encoded)}}, "spec": map[string]any{"persistentVolumeReclaimPolicy": "Retain", "claimRef": ref}}
	fresh := map[string]any{"metadata": map[string]any{"name": name, "labels": map[string]any{"nemo-move/backup": "backup"}, "annotations": map[string]any{restoreAnnotation: string(encoded)}}, "spec": map[string]any{"persistentVolumeReclaimPolicy": "Retain", "claimRef": ref, "csi": map[string]any{"volumeHandle": name}}}
	createdPV, createdPVC := 0, 0
	k := &restoreKube{namespace: "nemo", volumes: map[string]*restoreVolume{}}
	k.run = func(_ context.Context, input []byte, args ...string) ([]byte, error) {
		call := strings.Join(args, " ")
		switch {
		case call == "get pv -o json":
			return json.Marshal(map[string]any{"items": []any{old, fresh}})
		case strings.Contains(call, "get pv "+name):
			return json.Marshal(fresh)
		case call == "create -f -":
			var obj map[string]any
			if err := json.Unmarshal(input, &obj); err != nil {
				return nil, err
			}
			if stringField(obj, "kind") == "PersistentVolume" {
				createdPV++
			} else {
				createdPVC++
			}
			return nil, nil
		case strings.Contains(call, "wait --for="):
			return nil, nil
		default:
			return nil, fmt.Errorf("unexpected command %s", call)
		}
	}
	if err := k.recoverClaim(context.Background(), "data", "backup"); err != nil {
		t.Fatal(err)
	}
	k.volumes["data"].name = name
	k.volumes["control"] = &restoreVolume{reused: true}
	if err := k.bindClaims(context.Background(), "backup", "control"); err != nil {
		t.Fatal(err)
	}
	if createdPV != 0 || createdPVC != 1 {
		t.Fatalf("created PV=%d PVC=%d", createdPV, createdPVC)
	}
}

func TestRestoreExistingFreshPVRejectsWrongBackup(t *testing.T) {
	name := restoreVolumeName("nemo", "data", "backup")
	v := &restoreVolume{name: name, backup: "backup", oldHandle: "source", longhorn: map[string]any{"numberOfReplicas": float64(1)}, pvc: map[string]any{"metadata": map[string]any{}, "spec": map[string]any{}}, pv: map[string]any{"metadata": map[string]any{}, "spec": map[string]any{"csi": map[string]any{}}}}
	k := &restoreKube{namespace: "nemo", volumes: map[string]*restoreVolume{"data": v, "control": {reused: true}}}
	k.run = func(_ context.Context, _ []byte, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "get pv "+name) {
			return json.Marshal(map[string]any{"metadata": map[string]any{"name": name, "labels": map[string]any{"nemo-move/backup": "wrong"}}, "spec": map[string]any{"csi": map[string]any{"volumeHandle": name}, "claimRef": map[string]any{"namespace": "nemo", "name": "nemo-data"}}})
		}
		return nil, fmt.Errorf("unexpected mutation: %s", strings.Join(args, " "))
	}
	err := k.bindClaims(context.Background(), "backup", "control")
	if err == nil || !strings.Contains(err.Error(), "identity mismatch") {
		t.Fatalf("error = %v, want identity mismatch", err)
	}
}

func TestRestoreMultipleCandidatesRejectWrongIdentity(t *testing.T) {
	name := restoreVolumeName("nemo", "data", "backup")
	for _, tc := range []struct{ name, label, handle string }{
		{"wrong label", "other", name},
		{"wrong handle", "backup", "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref := map[string]any{"namespace": "nemo", "name": "nemo-data"}
			pv := func(n, label, handle string) map[string]any {
				return map[string]any{"metadata": map[string]any{"name": n, "labels": map[string]any{"nemo-move/backup": label}}, "spec": map[string]any{"persistentVolumeReclaimPolicy": "Retain", "claimRef": ref, "csi": map[string]any{"volumeHandle": handle}}}
			}
			k := &restoreKube{namespace: "nemo", volumes: map[string]*restoreVolume{}}
			k.run = func(_ context.Context, _ []byte, _ ...string) ([]byte, error) {
				return json.Marshal(map[string]any{"items": []any{pv("old", "", "old"), pv(name, tc.label, tc.handle)}})
			}
			err := k.recoverClaim(context.Background(), "data", "backup")
			if err == nil || !strings.Contains(err.Error(), "old") || !strings.Contains(err.Error(), name) {
				t.Fatalf("error = %v, want all candidates", err)
			}
		})
	}
}

func TestRestoreMultipleExactCandidatesFailClosed(t *testing.T) {
	name := restoreVolumeName("nemo", "data", "backup")
	pv := map[string]any{"metadata": map[string]any{"name": name, "labels": map[string]any{"nemo-move/backup": "backup"}}, "spec": map[string]any{"persistentVolumeReclaimPolicy": "Retain", "claimRef": map[string]any{"namespace": "nemo", "name": "nemo-data"}, "csi": map[string]any{"volumeHandle": name}}}
	k := &restoreKube{namespace: "nemo", volumes: map[string]*restoreVolume{}, run: func(_ context.Context, _ []byte, _ ...string) ([]byte, error) {
		return json.Marshal(map[string]any{"items": []any{pv, pv}})
	}}
	err := k.recoverClaim(context.Background(), "data", "backup")
	if err == nil || !strings.Contains(err.Error(), "2 retained PV candidates") || strings.Count(err.Error(), name) != 2 {
		t.Fatalf("error = %v", err)
	}
}

func TestRestoreBoundClaimRejectsWrongBackup(t *testing.T) {
	name := restoreVolumeName("nemo", "data", "other")
	k := &restoreKube{namespace: "nemo", volumes: map[string]*restoreVolume{}}
	k.run = func(_ context.Context, _ []byte, args ...string) ([]byte, error) {
		call := strings.Join(args, " ")
		switch {
		case strings.Contains(call, "get pvc"):
			return json.Marshal(map[string]any{"metadata": map[string]any{"labels": map[string]any{"nemo-move/backup": "other"}}, "spec": map[string]any{"volumeName": name}, "status": map[string]any{"phase": "Bound"}})
		case strings.Contains(call, "get pv"):
			return json.Marshal(map[string]any{"metadata": map[string]any{"name": name, "labels": map[string]any{"nemo-move/backup": "other"}}, "spec": map[string]any{"csi": map[string]any{"driver": "driver.longhorn.io", "volumeHandle": name}}})
		case strings.Contains(call, "get volumes.longhorn.io"):
			return []byte(`{"spec":{"numberOfReplicas":1},"status":{"restoreRequired":false}}`), nil
		default:
			return nil, fmt.Errorf("unexpected mutation: %s", call)
		}
	}
	err := k.captureClaim(context.Background(), "data", "backup")
	if err == nil || !strings.Contains(err.Error(), "identity mismatch") {
		t.Fatalf("error = %v, want identity mismatch", err)
	}
}

type restoreCommandFake struct {
	objects       map[string]map[string]any
	fail          string
	volumeCreates map[string]int
}

func newRestoreCommandFake() *restoreCommandFake {
	f := &restoreCommandFake{objects: map[string]map[string]any{}, volumeCreates: map[string]int{}}
	for _, kind := range []string{"data", "control"} {
		old := "old-" + kind
		f.objects["pvc/nemo-"+kind] = map[string]any{"kind": "PersistentVolumeClaim", "metadata": map[string]any{"name": "nemo-" + kind, "namespace": "nemo"}, "spec": map[string]any{"volumeName": old}}
		f.objects["pv/"+old] = map[string]any{"kind": "PersistentVolume", "metadata": map[string]any{"name": old}, "spec": map[string]any{"persistentVolumeReclaimPolicy": "Delete", "claimRef": map[string]any{"namespace": "nemo", "name": "nemo-" + kind}, "csi": map[string]any{"driver": "driver.longhorn.io", "volumeHandle": old}}}
		f.objects["volume/"+old] = map[string]any{"spec": map[string]any{"numberOfReplicas": float64(1)}}
		f.objects["backup/backup-"+kind] = map[string]any{"status": map[string]any{"state": "Completed", "url": "s3://" + kind, "volumeSize": "1024", "backupCreatedAt": "time", "volumeName": old}}
	}
	return f
}

func (f *restoreCommandFake) run(_ context.Context, input []byte, args ...string) ([]byte, error) {
	call := strings.Join(args, " ")
	if strings.Contains(call, "delete pod") {
		return nil, nil
	}
	if strings.Contains(call, "wait --for=") {
		return nil, nil
	}
	if strings.Contains(call, "get pv -o json") {
		items := []any{}
		for key, obj := range f.objects {
			if strings.HasPrefix(key, "pv/") {
				items = append(items, obj)
			}
		}
		return json.Marshal(map[string]any{"items": items})
	}
	for _, typ := range []struct{ command, prefix string }{{"get pvc ", "pvc/"}, {"get pv ", "pv/"}, {"get volumes.longhorn.io ", "volume/"}, {"get backups.longhorn.io ", "backup/"}} {
		if i := strings.Index(call, typ.command); i >= 0 {
			name := strings.Fields(call[i+len(typ.command):])[0]
			obj := f.objects[typ.prefix+name]
			if typ.prefix == "volume/" && obj != nil && !strings.Contains(call, "--ignore-not-found") {
				point := "complete-" + strings.TrimPrefix(stringField(mapField(mapField(obj, "metadata"), "labels"), "nemo-move/backup"), "backup-")
				if point == f.fail {
					f.fail = ""
					return nil, errors.New("interrupted after " + point)
				}
			}
			if obj == nil {
				if strings.Contains(call, "--ignore-not-found") {
					return nil, nil
				}
				return nil, fmt.Errorf("missing %s", name)
			}
			return json.Marshal(obj)
		}
	}
	point := ""
	if strings.HasPrefix(call, "patch pv ") {
		name := args[2]
		var patch map[string]any
		if err := json.Unmarshal([]byte(args[len(args)-1]), &patch); err != nil {
			return nil, err
		}
		obj := f.objects["pv/"+name]
		mapField(obj, "metadata")["annotations"] = mapField(mapField(patch, "metadata"), "annotations")
		mapField(obj, "spec")["persistentVolumeReclaimPolicy"] = "Retain"
		point = "patch-" + strings.TrimPrefix(name, "old-")
	} else if strings.Contains(call, "delete pvc ") {
		name := args[4]
		delete(f.objects, "pvc/"+name)
		point = "delete-" + strings.TrimPrefix(name, "nemo-")
	} else if call == "create -f -" {
		var obj map[string]any
		if err := json.Unmarshal(input, &obj); err != nil {
			return nil, err
		}
		name := stringField(mapField(obj, "metadata"), "name")
		switch stringField(obj, "kind") {
		case "Volume":
			f.volumeCreates[name]++
			obj["status"] = map[string]any{"state": "detached", "restoreRequired": false}
			f.objects["volume/"+name] = obj
			point = "volume-" + strings.TrimPrefix(stringField(mapField(mapField(obj, "metadata"), "labels"), "nemo-move/backup"), "backup-")
		case "PersistentVolume":
			f.objects["pv/"+name] = obj
			point = "pv-" + strings.TrimPrefix(stringField(mapField(mapField(obj, "spec"), "claimRef"), "name"), "nemo-")
		case "PersistentVolumeClaim":
			obj["status"] = map[string]any{"phase": "Bound"}
			f.objects["pvc/"+name] = obj
			point = "pvc-" + strings.TrimPrefix(name, "nemo-")
		default:
			return nil, fmt.Errorf("unexpected kind")
		}
	} else {
		return nil, fmt.Errorf("unexpected command %s", call)
	}
	if point == f.fail {
		f.fail = ""
		return nil, errors.New("interrupted after " + point)
	}
	return nil, nil
}

func TestRestoreRecoveryCrashWindows(t *testing.T) {
	for _, point := range []string{"patch-data", "patch-control", "delete-data", "delete-control", "volume-data", "complete-data", "volume-control", "complete-control", "pv-data", "pvc-data", "pv-control", "pvc-control"} {
		t.Run(point, func(t *testing.T) {
			f := newRestoreCommandFake()
			f.fail = point
			run := func() error {
				k := &restoreKube{namespace: "nemo", volumes: map[string]*restoreVolume{}, run: f.run}
				ctx := context.Background()
				if err := k.retainDeleteClaims(ctx, "backup-data", "backup-control"); err != nil {
					return err
				}
				for _, kind := range []string{"data", "control"} {
					if _, err := k.restoreBackup(ctx, kind, "backup-"+kind); err != nil {
						return err
					}
					if err := k.waitRestored(ctx, kind); err != nil {
						return err
					}
				}
				return k.bindClaims(ctx, "backup-data", "backup-control")
			}
			if err := run(); err == nil {
				t.Fatal("expected interruption")
			}
			if err := run(); err != nil {
				t.Fatalf("recovery: %v", err)
			}
			for _, kind := range []string{"data", "control"} {
				name := restoreVolumeName("nemo", kind, "backup-"+kind)
				if f.volumeCreates[name] != 1 {
					t.Fatalf("%s volume creates = %d", kind, f.volumeCreates[name])
				}
				volume := f.objects["volume/"+name]
				if stringField(mapField(volume, "spec"), "fromBackup") != "s3://"+kind || stringField(mapField(mapField(volume, "metadata"), "labels"), "nemo-move/backup") != "backup-"+kind {
					t.Fatalf("%s restored from wrong backup: %v", kind, volume)
				}
				if stringField(mapField(f.objects["pv/old-"+kind], "spec"), "persistentVolumeReclaimPolicy") != "Retain" {
					t.Fatalf("old %s PV not retained", kind)
				}
			}
		})
	}
}

func TestRestoreCommitRecreatesMissingMaintenance(t *testing.T) {
	var calls []string
	k := &restoreKube{namespace: "nemo", volumes: map[string]*restoreVolume{}}
	k.run = func(_ context.Context, _ []byte, args ...string) ([]byte, error) {
		call := strings.Join(args, " ")
		calls = append(calls, call)
		switch {
		case strings.Contains(call, "get pod nemo-move-maint"):
			return []byte(""), nil
		case strings.Contains(call, "get statefulset nemo"):
			return []byte(`{"spec":{"replicas":0,"template":{"spec":{"containers":[{"name":"hermes","image":"nemo"}]}}}}`), nil
		case strings.Contains(call, "get pod nemo-0"):
			return []byte(""), nil
		case strings.Contains(call, "restore-read"):
			return []byte(`{"backupData":"data","backupControl":"control","nonce":"nonce"}`), nil
		default:
			return []byte(""), nil
		}
	}
	record, err := k.readRecord(context.Background())
	if err != nil || record.Nonce != "nonce" {
		t.Fatalf("record=%+v err=%v", record, err)
	}
	joined := strings.Join(calls, "\n")
	if !strings.Contains(joined, "create -f -") || strings.Contains(joined, " scale ") || strings.Contains(joined, " delete ") {
		t.Fatalf("calls=%v", calls)
	}
}
func TestRestoreBackendWaitRequiresRestoreCompletion(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	k := &restoreKube{volumes: map[string]*restoreVolume{"data": {name: "restored"}}}
	k.run = func(_ context.Context, _ []byte, _ ...string) ([]byte, error) {
		cancel()
		return []byte(`{"status":{"state":"detached","restoreRequired":true}}`), nil
	}
	if err := k.waitRestored(ctx, "data"); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait error = %v, want cancellation", err)
	}
}

func TestRestoreRejectsInvalidDNSNamesBeforeBackend(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantRefusal bool
	}{
		{name: "all selector is refused", args: []string{"--kubeconfig", "config", "--node-confirmed-off", "--all", "--backup-data", "data", "--backup-control", "control"}, wantRefusal: true},
		{name: "invalid node name", args: []string{"--kubeconfig", "config", "--node-confirmed-off", "Bad_Node", "--backup-data", "data", "--backup-control", "control"}, wantRefusal: true},
		{name: "invalid data backup name", args: []string{"--kubeconfig", "config", "--node-confirmed-off", "node-1", "--backup-data", "bad/name", "--backup-control", "control"}, wantRefusal: true},
		{name: "invalid control backup name", args: []string{"--kubeconfig", "config", "--node-confirmed-off", "node-1", "--backup-data", "data", "--backup-control", "bad/name"}, wantRefusal: true},
		{name: "valid DNS labels reach the backend", args: []string{"--kubeconfig", "config", "--node-confirmed-off", "node-1", "--backup-data", "data-2026-11-15", "--backup-control", "control-2026-11-15"}},
		{name: "node DNS subdomain accepted", args: []string{"--kubeconfig", "config", "--node-confirmed-off", "node-1.lan", "--backup-data", "data", "--backup-control", "control"}},
		{name: "empty node label rejected", args: []string{"--kubeconfig", "config", "--node-confirmed-off", "node..lan", "--backup-data", "data", "--backup-control", "control"}, wantRefusal: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			deps := restoreDeps{
				backend: func(string) restoreBackend { called = true; return newRestoreFake() },
				nonce:   func() (string, error) { return "nonce", nil },
			}
			err := restore(context.Background(), tt.args, deps, &bytes.Buffer{})
			if tt.wantRefusal {
				if err == nil || called {
					t.Fatalf("restore() err=%v backendCalled=%v, want pre-backend refusal", err, called)
				}
				return
			}
			if !called {
				t.Fatalf("restore() err=%v backendCalled=%v, want valid args to reach backend", err, called)
			}
		})
	}
}

func TestRestoreBackendRejectsFailedRestores(t *testing.T) {
	tests := []struct {
		name   string
		status string
		want   string
	}{
		{name: "faulted volume", status: `{"status":{"robustness":"faulted"}}`, want: "faulted"},
		{name: "restore error condition", status: `{"status":{"conditions":[{"type":"Restore","status":"True","reason":"RestoreFailure","message":"restore failed"}]}}`, want: "restore error condition"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := &restoreKube{volumes: map[string]*restoreVolume{"data": {name: "restored"}}}
			k.run = func(_ context.Context, _ []byte, _ ...string) ([]byte, error) { return []byte(tt.status), nil }
			err := k.waitRestored(context.Background(), "data")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("waitRestored() error = %v, want %q", err, tt.want)
			}
		})
	}
}
