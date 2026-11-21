package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/RigelBuild/nebula/tools/nemo-move/internal/phase"
)

const restoreTimeout = 2 * time.Hour

type restoreBackend interface {
	deleteNode(context.Context, string) error
	scale(context.Context, int) error
	retainDeleteClaims(context.Context, string, string) error
	restoreBackup(context.Context, string, string) (string, error)
	waitRestored(context.Context, string) error
	bindClaims(context.Context, string, string) error
	open(context.Context) error
	writePhase(context.Context, phase.Value) error
	readPhase(context.Context) (phase.Value, error)
	writeRecord(context.Context, phase.RestorePrepareRecord) error
	readRecord(context.Context) (phase.RestorePrepareRecord, error)
	clearReplicasAnnotation(context.Context) error
	close(context.Context) error
}

type restoreDeps struct {
	backend func(string) restoreBackend
	nonce   func() (string, error)
	fault   func(string) error
}

// RunRestore prepares or commits a restore using the supplied kubeconfig.
func RunRestore(ctx context.Context, args []string, stdout io.Writer) error {
	return restore(ctx, args, restoreDeps{backend: func(config string) restoreBackend {
		return &restoreKube{config: config, volumes: map[string]*restoreVolume{}}
	}, nonce: restoreNonce}, stdout)
}

func restore(ctx context.Context, args []string, deps restoreDeps, stdout io.Writer) error {
	flags := flag.NewFlagSet("restore", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	kubeconfig := flags.String("kubeconfig", "", "")
	node := flags.String("node-confirmed-off", "", "")
	data := flags.String("backup-data", "", "")
	control := flags.String("backup-control", "", "")
	nonce := flags.String("nonce", "", "")
	commit := flags.Bool("commit", false, "")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse restore flags: %w", err)
	}
	if flags.NArg() != 0 || *kubeconfig == "" || *data == "" || *control == "" || (*commit && (*nonce == "" || *node != "")) || (!*commit && (*node == "" || *nonce != "")) {
		return fmt.Errorf("invalid restore arguments")
	}
	if !isDNS1123Label(*data) || !isDNS1123Label(*control) {
		return fmt.Errorf("backup names must be DNS-1123 labels")
	}
	if !*commit && !isDNS1123Subdomain(*node) {
		return fmt.Errorf("node-confirmed-off must be a DNS-1123 subdomain")
	}
	b := deps.backend(*kubeconfig)
	if *commit {
		record, err := b.readRecord(ctx)
		if err != nil {
			return fmt.Errorf("read prepare record: %w", err)
		}
		if record.BackupData != *data || record.BackupControl != *control || record.Nonce != *nonce {
			return fmt.Errorf("restore prepare record does not match")
		}
		writeErr := b.writePhase(ctx, phase.Owner)
		value, readErr := b.readPhase(ctx)
		if readErr != nil {
			return fmt.Errorf("read owner after write: %w", readErr)
		}
		if value != phase.Owner {
			return fmt.Errorf("owner not verified after write: %v", writeErr)
		}
		if err := b.close(ctx); err != nil {
			return fmt.Errorf("close control path: %w", err)
		}
		if err := b.clearReplicasAnnotation(ctx); err != nil {
			return fmt.Errorf("clear stale replicas annotation: %w", err)
		}
		if err := b.scale(ctx, 1); err != nil {
			return fmt.Errorf("scale nemo: %w", err)
		}
		return nil
	}
	if err := b.deleteNode(ctx, *node); err != nil {
		return fmt.Errorf("delete confirmed-off node: %w", err)
	}
	if err := b.scale(ctx, 0); err != nil {
		return fmt.Errorf("scale nemo to zero: %w", err)
	}
	if err := b.retainDeleteClaims(ctx, *data, *control); err != nil {
		return fmt.Errorf("retain volumes and delete claims: %w", err)
	}
	dataTime, err := b.restoreBackup(ctx, "data", *data)
	if err != nil {
		return fmt.Errorf("restore data: %w", err)
	}
	if err := b.waitRestored(ctx, "data"); err != nil {
		return fmt.Errorf("wait for data restore: %w", err)
	}
	if deps.fault != nil {
		if err := deps.fault("mid-restore"); err != nil {
			return err
		}
	}
	controlTime, err := b.restoreBackup(ctx, "control", *control)
	if err != nil {
		return fmt.Errorf("restore control: %w", err)
	}
	if err := b.waitRestored(ctx, "control"); err != nil {
		return fmt.Errorf("wait for control restore: %w", err)
	}
	if err := b.bindClaims(ctx, *data, *control); err != nil {
		return fmt.Errorf("bind claims: %w", err)
	}
	if err := b.open(ctx); err != nil {
		return fmt.Errorf("open control path: %w", err)
	}
	if err := b.writePhase(ctx, phase.Blocked); err != nil {
		return fmt.Errorf("block restored phase: %w", err)
	}
	n, err := deps.nonce()
	if err != nil {
		return fmt.Errorf("generate nonce: %w", err)
	}
	record := phase.RestorePrepareRecord{BackupData: *data, BackupControl: *control, Nonce: n}
	if err := b.writeRecord(ctx, record); err != nil {
		return fmt.Errorf("write prepare record: %w", err)
	}
	got, err := b.readRecord(ctx)
	if err != nil {
		return fmt.Errorf("read prepare record: %w", err)
	}
	if got != record {
		return fmt.Errorf("prepare record readback mismatch")
	}
	if err := b.close(ctx); err != nil {
		return fmt.Errorf("close control path: %w", err)
	}
	if _, err := fmt.Fprintf(stdout, "data backup: %s\ncontrol backup: %s\nnonce: %s\n", dataTime, controlTime, n); err != nil {
		return fmt.Errorf("write restore output: %w", err)
	}
	return nil
}

func restoreNonce() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func isDNS1123Label(value string) bool {
	if len(value) == 0 || len(value) > 63 || !isDNS1123Alphanumeric(value[0]) || !isDNS1123Alphanumeric(value[len(value)-1]) {
		return false
	}
	for i := 1; i < len(value)-1; i++ {
		if !isDNS1123Alphanumeric(value[i]) && value[i] != '-' {
			return false
		}
	}
	return true
}

func isDNS1123Subdomain(value string) bool {
	if len(value) == 0 || len(value) > 253 {
		return false
	}
	for _, part := range strings.Split(value, ".") {
		if !isDNS1123Label(part) {
			return false
		}
	}
	return true
}

func isDNS1123Alphanumeric(char byte) bool {
	return char >= 'a' && char <= 'z' || char >= '0' && char <= '9'
}

type restoreVolume struct {
	longhorn  map[string]any
	pvc       map[string]any
	pv        map[string]any
	oldHandle string
	backup    string
	reused    bool
	name      string
}
type restoreKube struct {
	config, namespace string
	volumes           map[string]*restoreVolume
	run               func(context.Context, []byte, ...string) ([]byte, error)
	control           *controlPath
}

func (k *restoreKube) command(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	if k.run != nil {
		return k.run(ctx, input, args...)
	}
	var stdin io.Reader
	if input != nil {
		stdin = bytes.NewReader(input)
	}
	return kubectlRun(k.config)(ctx, stdin, args...)
}

// controlRun adapts command to the kubeRun the control path uses.
func (k *restoreKube) controlRun() kubeRun {
	return func(ctx context.Context, input io.Reader, args ...string) ([]byte, error) {
		var data []byte
		if input != nil {
			var err error
			if data, err = io.ReadAll(input); err != nil {
				return nil, err
			}
		}
		return k.command(ctx, data, args...)
	}
}

// ctl returns the restore's control path, created on first use.
func (k *restoreKube) ctl() *controlPath {
	if k.control == nil {
		k.control = &controlPath{run: k.controlRun(), namespace: k.namespace}
	}
	return k.control
}
func (k *restoreKube) object(ctx context.Context, args ...string) (map[string]any, error) {
	out, err := k.command(ctx, nil, args...)
	if err != nil {
		return nil, err
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		return nil, fmt.Errorf("decode kubernetes object: %w", err)
	}
	return obj, nil
}
func mapField(m map[string]any, key string) map[string]any { v, _ := m[key].(map[string]any); return v }
func stringField(m map[string]any, key string) string      { v, _ := m[key].(string); return v }
func (k *restoreKube) discover(ctx context.Context) error {
	if k.namespace != "" {
		return nil
	}
	ns, err := discoverNamespace(ctx, k.controlRun())
	if err != nil {
		return err
	}
	k.namespace = ns
	return nil
}
func (k *restoreKube) deleteNode(ctx context.Context, node string) error {
	_, err := k.command(ctx, nil, "delete", "node", node, "--ignore-not-found")
	return err
}
func (k *restoreKube) scale(ctx context.Context, n int) error {
	if err := k.discover(ctx); err != nil {
		return err
	}
	_, err := k.command(ctx, nil, "-n", k.namespace, "scale", "statefulset/nemo", fmt.Sprintf("--replicas=%d", n))
	return err
}

func (k *restoreKube) clearReplicasAnnotation(ctx context.Context) error {
	if err := k.discover(ctx); err != nil {
		return err
	}
	sts, err := k.ctl().statefulSet(ctx)
	if err != nil {
		return err
	}
	if _, ok := sts.annotations[replicasAnnotation]; !ok {
		return nil
	}
	_, err = k.command(ctx, nil, "-n", k.namespace, "annotate", "statefulset", "nemo", replicasAnnotation+"-")
	return err
}

const restoreAnnotation = "nemo-move/recovery"

func (k *restoreKube) retainDeleteClaims(ctx context.Context, data, control string) error {
	if err := k.discover(ctx); err != nil {
		return err
	}
	if _, err := k.command(ctx, nil, "-n", k.namespace, "delete", "pod", "nemo-move-maint", "--ignore-not-found", "--wait=true"); err != nil {
		return err
	}
	for _, item := range []struct{ kind, backup string }{{"data", data}, {"control", control}} {
		if err := k.captureClaim(ctx, item.kind, item.backup); err != nil {
			return err
		}
	}
	return nil
}

func (k *restoreKube) captureClaim(ctx context.Context, kind, backup string) error {
	out, err := k.command(ctx, nil, "-n", k.namespace, "get", "pvc", "nemo-"+kind, "--ignore-not-found", "-o", "json")
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return k.recoverClaim(ctx, kind, backup)
	}
	var pvc map[string]any
	if err := json.Unmarshal(out, &pvc); err != nil {
		return err
	}
	pvName := stringField(mapField(pvc, "spec"), "volumeName")
	if pvName == "" {
		return fmt.Errorf("claim nemo-%s has no volume", kind)
	}
	pv, err := k.object(ctx, "get", "pv", pvName, "-o", "json")
	if err != nil {
		return err
	}
	csi := mapField(mapField(pv, "spec"), "csi")
	handle := stringField(csi, "volumeHandle")
	if stringField(csi, "driver") != "driver.longhorn.io" || handle == "" {
		return fmt.Errorf("claim nemo-%s is not a Longhorn volume", kind)
	}
	source, err := k.object(ctx, "-n", "longhorn-system", "get", "volumes.longhorn.io", handle, "-o", "json")
	if err != nil {
		return err
	}
	v := &restoreVolume{pvc: pvc, pv: pv, oldHandle: handle, longhorn: mapField(source, "spec"), backup: backup}
	k.volumes[kind] = v
	pvcBackup := stringField(mapField(mapField(pvc, "metadata"), "labels"), "nemo-move/backup")
	pvBackup := stringField(mapField(mapField(pv, "metadata"), "labels"), "nemo-move/backup")
	if pvcBackup != "" || pvBackup != "" {
		if (pvcBackup != "" && pvcBackup != backup) || (pvBackup != "" && pvBackup != backup) {
			return fmt.Errorf("restore claim nemo-%s identity mismatch", kind)
		}
		prior := stringField(mapField(mapField(pv, "metadata"), "annotations"), restoreAnnotation)
		if prior == "" {
			return fmt.Errorf("pv %s lacks recovery annotation", pvName)
		}
		var saved map[string]any
		if err := json.Unmarshal([]byte(prior), &saved); err != nil {
			return fmt.Errorf("invalid recovery annotation on pv %s: %w", pvName, err)
		}
		if err := loadRecovery(v, saved, pvName); err != nil {
			return err
		}
		v.name, v.reused = handle, true
		return nil
	}
	saved := map[string]any{"pvc": pvc, "pv": pv, "longhorn": v.longhorn, "oldHandle": handle}
	if prior := stringField(mapField(mapField(pv, "metadata"), "annotations"), restoreAnnotation); prior != "" {
		if err := json.Unmarshal([]byte(prior), &saved); err != nil {
			return fmt.Errorf("invalid recovery annotation on pv %s: %w", pvName, err)
		}
	}
	encoded, err := json.Marshal(saved)
	if err != nil {
		return err
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]any{restoreAnnotation: string(encoded)}}, "spec": map[string]any{"persistentVolumeReclaimPolicy": "Retain"}})
	if err != nil {
		return err
	}
	if _, err := k.command(ctx, nil, "patch", "pv", pvName, "--type=merge", "-p", string(patch)); err != nil {
		return err
	}
	if err := loadRecovery(v, saved, pvName); err != nil {
		return err
	}
	_, err = k.command(ctx, nil, "-n", k.namespace, "delete", "pvc", "nemo-"+kind, "--wait=true", "--ignore-not-found")
	return err
}

func loadRecovery(v *restoreVolume, saved map[string]any, pvName string) error {
	v.pvc = mapField(saved, "pvc")
	v.pv = mapField(saved, "pv")
	v.longhorn = mapField(saved, "longhorn")
	v.oldHandle = stringField(saved, "oldHandle")
	if len(v.pvc) == 0 || len(v.pv) == 0 || len(v.longhorn) == 0 || v.oldHandle == "" {
		return fmt.Errorf("pv %s has invalid recovery annotation", pvName)
	}
	return nil
}

func restoreVolumeName(namespace, kind, backup string) string {
	hash := sha256.Sum256([]byte(namespace + "\x00" + kind + "\x00" + backup))
	return fmt.Sprintf("nemo-restore-%s-%x", kind, hash[:16])
}

func (k *restoreKube) recoverClaim(ctx context.Context, kind, backup string) error {
	list, err := k.object(ctx, "get", "pv", "-o", "json")
	if err != nil {
		return err
	}
	items, ok := list["items"].([]any)
	if !ok {
		return fmt.Errorf("invalid pv list")
	}
	var found map[string]any
	names := []string{}
	matches := 0
	expected := restoreVolumeName(k.namespace, kind, backup)
	for _, item := range items {
		pv, ok := item.(map[string]any)
		if !ok {
			continue
		}
		spec := mapField(pv, "spec")
		ref := mapField(spec, "claimRef")
		if stringField(ref, "namespace") == k.namespace && stringField(ref, "name") == "nemo-"+kind && stringField(spec, "persistentVolumeReclaimPolicy") == "Retain" {
			name := stringField(mapField(pv, "metadata"), "name")
			names = append(names, name)
			if len(names) == 1 {
				found = pv
			}
			if name == expected && stringField(mapField(mapField(pv, "metadata"), "labels"), "nemo-move/backup") == backup && stringField(mapField(spec, "csi"), "volumeHandle") == expected {
				matches++
				found = pv
			}
		}
	}
	if len(names) == 0 || (len(names) > 1 && matches != 1) {
		return fmt.Errorf("claim nemo-%s has %d retained PV candidates: %s", kind, len(names), strings.Join(names, ", "))
	}
	annotation := stringField(mapField(mapField(found, "metadata"), "annotations"), restoreAnnotation)
	pvName := stringField(mapField(found, "metadata"), "name")
	if annotation == "" {
		return fmt.Errorf("retained pv %s lacks recovery annotation", pvName)
	}
	var saved map[string]any
	if err := json.Unmarshal([]byte(annotation), &saved); err != nil {
		return fmt.Errorf("invalid recovery annotation on pv %s: %w", pvName, err)
	}
	v := &restoreVolume{backup: backup}
	if err := loadRecovery(v, saved, pvName); err != nil {
		return err
	}
	k.volumes[kind] = v
	return nil
}
func (k *restoreKube) create(ctx context.Context, obj map[string]any) error {
	data, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	_, err = k.command(ctx, data, "create", "-f", "-")
	return err
}
func (k *restoreKube) restoreBackup(ctx context.Context, kind, backup string) (string, error) {
	v := k.volumes[kind]
	if v == nil {
		return "", fmt.Errorf("missing captured claim %s", kind)
	}
	obj, err := k.object(ctx, "-n", "longhorn-system", "get", "backups.longhorn.io", backup, "-o", "json")
	if err != nil {
		return "", err
	}
	status := mapField(obj, "status")
	url, size, timestamp := stringField(status, "url"), stringField(status, "volumeSize"), stringField(status, "backupCreatedAt")
	if stringField(status, "state") != "Completed" || url == "" || size == "" || timestamp == "" {
		return "", fmt.Errorf("backup %s is incomplete", backup)
	}
	if source := stringField(status, "volumeName"); source != "" && source != v.oldHandle {
		return "", fmt.Errorf("backup %s belongs to another volume", backup)
	}
	if v.reused {
		existing, err := k.object(ctx, "-n", "longhorn-system", "get", "volumes.longhorn.io", v.name, "-o", "json")
		if err != nil {
			return "", err
		}
		if stringField(mapField(mapField(existing, "metadata"), "labels"), "nemo-move/backup") != backup || stringField(mapField(existing, "spec"), "fromBackup") != url {
			return "", fmt.Errorf("restore volume %s identity mismatch", v.name)
		}
		return timestamp, nil
	}
	v.name = restoreVolumeName(k.namespace, kind, backup)
	spec := map[string]any{"size": size, "fromBackup": url}
	for _, key := range []string{"numberOfReplicas", "frontend", "dataEngine", "accessMode", "dataLocality", "backupCompressionMethod", "revisionCounterDisabled"} {
		if value, ok := v.longhorn[key]; ok {
			spec[key] = value
		}
	}
	out, err := k.command(ctx, nil, "-n", "longhorn-system", "get", "volumes.longhorn.io", v.name, "--ignore-not-found", "-o", "json")
	if err != nil {
		return "", err
	}
	if len(bytes.TrimSpace(out)) != 0 {
		var existing map[string]any
		if err := json.Unmarshal(out, &existing); err != nil {
			return "", fmt.Errorf("decode restore volume: %w", err)
		}
		if stringField(mapField(mapField(existing, "metadata"), "labels"), "nemo-move/backup") != backup || stringField(mapField(existing, "spec"), "fromBackup") != url {
			return "", fmt.Errorf("restore volume %s identity mismatch", v.name)
		}
		return timestamp, nil
	}
	volume := map[string]any{"apiVersion": "longhorn.io/v1beta2", "kind": "Volume", "metadata": map[string]any{"name": v.name, "namespace": "longhorn-system", "labels": map[string]any{"nemo-move/backup": backup}}, "spec": spec}
	if err := k.create(ctx, volume); err != nil {
		return "", err
	}
	return timestamp, nil
}
func (k *restoreKube) waitRestored(ctx context.Context, kind string) error {
	v := k.volumes[kind]
	if v == nil {
		return fmt.Errorf("missing restore volume %s", kind)
	}
	waitCtx, cancel := context.WithTimeout(ctx, restoreTimeout)
	defer cancel()
	for {
		obj, err := k.object(waitCtx, "-n", "longhorn-system", "get", "volumes.longhorn.io", v.name, "-o", "json")
		if err != nil {
			if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("wait for %s restore completion: %w", kind, waitCtx.Err())
			}
			return err
		}
		status := mapField(obj, "status")
		if stringField(status, "robustness") == "faulted" {
			return fmt.Errorf("restore volume %s is faulted", kind)
		}
		if conditions, ok := status["conditions"].([]any); ok {
			for _, item := range conditions {
				condition, ok := item.(map[string]any)
				if ok && stringField(condition, "type") == "Restore" && stringField(condition, "reason") == "RestoreFailure" {
					return fmt.Errorf("restore volume %s has restore error condition: %s", kind, stringField(condition, "message"))
				}
			}
		}
		required, exists := status["restoreRequired"].(bool)
		if stringField(status, "state") == "detached" && exists && !required {
			return nil
		}
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			if errors.Is(waitCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				return fmt.Errorf("wait for %s restore completion: %w", kind, waitCtx.Err())
			}
			return waitCtx.Err()
		case <-timer.C:
		}
	}
}
func cleanRestoreMetadata(obj map[string]any, name, namespace string) {
	old := mapField(obj, "metadata")
	meta := map[string]any{"name": name}
	if namespace != "" {
		meta["namespace"] = namespace
	}
	if labels := old["labels"]; labels != nil {
		meta["labels"] = labels
	}
	if annotations, ok := old["annotations"].(map[string]any); ok {
		delete(annotations, "pv.kubernetes.io/bind-completed")
		delete(annotations, "pv.kubernetes.io/bound-by-controller")
		delete(annotations, "volume.kubernetes.io/selected-node")
		meta["annotations"] = annotations
	}
	obj["metadata"] = meta
	delete(obj, "status")
}
func (k *restoreKube) bindClaims(ctx context.Context, _, _ string) error {
	for _, kind := range []string{"data", "control"} {
		v := k.volumes[kind]
		if v.reused {
			if _, err := k.command(ctx, nil, "-n", k.namespace, "wait", "--for=jsonpath={.status.phase}=Bound", "pvc/nemo-"+kind, "--timeout=10m"); err != nil {
				return err
			}
			continue
		}
		pvSpec := mapField(v.pv, "spec")
		pvcSpec := mapField(v.pvc, "spec")
		snapshot, err := json.Marshal(map[string]any{"pvc": v.pvc, "pv": v.pv, "longhorn": v.longhorn, "oldHandle": v.oldHandle})
		if err != nil {
			return err
		}
		cleanRestoreMetadata(v.pv, v.name, "")
		cleanRestoreMetadata(v.pvc, "nemo-"+kind, k.namespace)
		annotations := mapField(mapField(v.pv, "metadata"), "annotations")
		if annotations == nil {
			annotations = map[string]any{}
			mapField(v.pv, "metadata")["annotations"] = annotations
		}
		annotations[restoreAnnotation] = string(snapshot)
		pvLabels := mapField(mapField(v.pv, "metadata"), "labels")
		if pvLabels == nil {
			pvLabels = map[string]any{}
			mapField(v.pv, "metadata")["labels"] = pvLabels
		}
		pvLabels["nemo-move/backup"] = v.backup
		labels := mapField(mapField(v.pvc, "metadata"), "labels")
		if labels == nil {
			labels = map[string]any{}
			mapField(v.pvc, "metadata")["labels"] = labels
		}
		labels["nemo-move/backup"] = v.backup
		pvSpec["persistentVolumeReclaimPolicy"] = "Retain"
		pvSpec["claimRef"] = map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "name": "nemo-" + kind, "namespace": k.namespace}
		mapField(pvSpec, "csi")["volumeHandle"] = v.name
		pvcSpec["volumeName"] = v.name
		delete(pvcSpec, "dataSource")
		delete(pvcSpec, "dataSourceRef")
		out, err := k.command(ctx, nil, "get", "pv", v.name, "--ignore-not-found", "-o", "json")
		if err != nil {
			return err
		}
		if len(bytes.TrimSpace(out)) == 0 {
			if err := k.create(ctx, v.pv); err != nil {
				return err
			}
		} else {
			var existing map[string]any
			if err := json.Unmarshal(out, &existing); err != nil {
				return err
			}
			spec := mapField(existing, "spec")
			ref := mapField(spec, "claimRef")
			if stringField(mapField(spec, "csi"), "volumeHandle") != v.name || stringField(ref, "namespace") != k.namespace || stringField(ref, "name") != "nemo-"+kind || stringField(mapField(mapField(existing, "metadata"), "labels"), "nemo-move/backup") != v.backup {
				return fmt.Errorf("restore pv %s identity mismatch", v.name)
			}
		}
		if err := k.create(ctx, v.pvc); err != nil {
			return err
		}
		if _, err := k.command(ctx, nil, "-n", k.namespace, "wait", "--for=jsonpath={.status.phase}=Bound", "pvc/nemo-"+kind, "--timeout=10m"); err != nil {
			return err
		}
	}
	return nil
}
func (k *restoreKube) open(ctx context.Context) error {
	if err := k.discover(ctx); err != nil {
		return err
	}
	return k.ctl().stop(ctx)
}
func (k *restoreKube) maintenance(ctx context.Context, args ...string) ([]byte, error) {
	return k.ctl().maint(ctx, nil, args...)
}
func (k *restoreKube) writePhase(ctx context.Context, p phase.Value) error {
	_, err := k.maintenance(ctx, "phase-write", string(p))
	return err
}
func (k *restoreKube) readPhase(ctx context.Context) (phase.Value, error) {
	out, err := k.maintenance(ctx, "phase-read")
	return phase.Value(out), err
}
func (k *restoreKube) writeRecord(ctx context.Context, r phase.RestorePrepareRecord) error {
	_, err := k.maintenance(ctx, "restore-write", r.BackupData, r.BackupControl, r.Nonce)
	return err
}
func (k *restoreKube) readRecord(ctx context.Context) (phase.RestorePrepareRecord, error) {
	var r phase.RestorePrepareRecord
	if err := k.discover(ctx); err != nil {
		return r, err
	}
	pod, err := k.command(ctx, nil, "-n", k.namespace, "get", "pod", "nemo-move-maint", "--ignore-not-found", "-o", "name")
	if err != nil {
		return r, err
	}
	if len(bytes.TrimSpace(pod)) == 0 {
		sts, err := k.object(ctx, "-n", k.namespace, "get", "statefulset", "nemo", "-o", "json")
		if err != nil {
			return r, err
		}
		replicas, ok := mapField(sts, "spec")["replicas"].(float64)
		if !ok || replicas != 0 {
			return r, fmt.Errorf("nemo must be scaled to zero before reading restore record")
		}
		old, err := k.command(ctx, nil, "-n", k.namespace, "get", "pod", "nemo-0", "--ignore-not-found", "-o", "name")
		if err != nil {
			return r, err
		}
		if len(bytes.TrimSpace(old)) != 0 {
			return r, fmt.Errorf("nemo-0 must be gone before reading restore record")
		}
		if err := k.ctl().ensurePod(ctx); err != nil {
			return r, err
		}
	}
	out, err := k.maintenance(ctx, "restore-read")
	if err != nil {
		return r, err
	}
	err = json.Unmarshal(out, &r)
	return r, err
}
func (k *restoreKube) close(ctx context.Context) error {
	return k.ctl().close(ctx)
}
