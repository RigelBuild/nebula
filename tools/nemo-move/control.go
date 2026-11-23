package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec" //nolint:depguard // kubectl is the control path.
	"strconv"
	"strings"
)

const (
	maintName          = "nemo-move-maint"
	replicasAnnotation = "nemo-move/replicas-before-open"
)

// apiDefaults names fields the API server fills in on objects this tool did not
// write. A live object may carry them; any other field outside the wanted spec
// counts as drift. Empty objects such as resources: {} are ignored too.
var apiDefaults = map[string]bool{
	"nodeName": true, "serviceAccount": true, "serviceAccountName": true, "schedulerName": true,
	"dnsPolicy": true, "enableServiceLinks": true, "preemptionPolicy": true, "priority": true,
	"terminationGracePeriodSeconds": true, "tolerations": true, "imagePullPolicy": true,
	"terminationMessagePath": true, "terminationMessagePolicy": true,
}

// kubeRun runs kubectl with args and optional stdin, and returns stdout.
type kubeRun func(ctx context.Context, input io.Reader, args ...string) ([]byte, error)

// kubectlRun runs kubectl with the kubeconfig at config.
func kubectlRun(config string) kubeRun {
	return func(ctx context.Context, input io.Reader, args ...string) ([]byte, error) {
		argv := append([]string{"--kubeconfig", config}, args...)
		cmd := exec.CommandContext(ctx, "kubectl", argv...) //nolint:gosec // Fixed executable and discrete arguments.
		cmd.Stdin = input
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("kubectl %s: %w: %s", strings.Join(args, " "), err, stderr.String())
		}
		return stdout.Bytes(), nil
	}
}

// discoverNamespace returns the namespace of the one StatefulSet named nemo.
func discoverNamespace(ctx context.Context, run kubeRun) (string, error) {
	out, err := run(ctx, nil, "get", "statefulsets", "--all-namespaces", "-o", "json")
	if err != nil {
		return "", err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return "", fmt.Errorf("decode statefulsets: %w", err)
	}
	namespace := ""
	for _, item := range list.Items {
		if item.Metadata.Name != "nemo" {
			continue
		}
		if namespace != "" {
			return "", errors.New("multiple nemo statefulsets")
		}
		namespace = item.Metadata.Namespace
	}
	if namespace == "" {
		return "", errors.New("nemo statefulset not found")
	}
	return namespace, nil
}

// controlPath is the maintenance Pod and deny-all NetworkPolicy that cutover and
// restore share. One owner holds it at a time. While held, the replica count to
// restore on close is kept in an annotation on StatefulSet nemo, so a rerun that
// finds nemo already at zero still restores the right value.
type controlPath struct {
	run       kubeRun
	namespace string // resolved on first use when empty
	held      bool
	replicas  int // the count close restores
}

// kubectl runs args in the control path's namespace.
func (c *controlPath) kubectl(ctx context.Context, input io.Reader, args ...string) ([]byte, error) {
	if c.namespace == "" {
		ns, err := discoverNamespace(ctx, c.run)
		if err != nil {
			return nil, err
		}
		c.namespace = ns
	}
	return c.run(ctx, input, append([]string{"-n", c.namespace}, args...)...)
}

// execIn runs cmd in the maintenance Pod, with input on stdin.
func (c *controlPath) execIn(ctx context.Context, input io.Reader, cmd ...string) ([]byte, error) {
	return c.kubectl(ctx, input, append([]string{"exec", "-i", maintName, "--"}, cmd...)...)
}

// maint runs one maint-side command in the maintenance Pod.
func (c *controlPath) maint(ctx context.Context, input io.Reader, args ...string) ([]byte, error) {
	return c.execIn(ctx, input, append([]string{"nemo-move", "maint-side"}, args...)...)
}

// stsState is the part of StatefulSet nemo that the control path reads.
type stsState struct {
	replicas    int
	image       string
	annotations map[string]string
}

func (c *controlPath) statefulSet(ctx context.Context) (stsState, error) {
	out, err := c.kubectl(ctx, nil, "get", "statefulset", "nemo", "-o", "json")
	if err != nil {
		return stsState{}, err
	}
	var obj struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Spec struct {
			Replicas *int `json:"replicas"`
			Template struct {
				Spec struct {
					Containers []struct {
						Name  string `json:"name"`
						Image string `json:"image"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(out, &obj); err != nil {
		return stsState{}, fmt.Errorf("decode statefulset nemo: %w", err)
	}
	if obj.Spec.Replicas == nil {
		return stsState{}, errors.New("statefulset nemo has no replicas")
	}
	sts := stsState{replicas: *obj.Spec.Replicas, annotations: obj.Metadata.Annotations}
	for _, container := range obj.Spec.Template.Spec.Containers {
		if container.Name == "hermes" {
			sts.image = container.Image
		}
	}
	if sts.image == "" {
		return stsState{}, errors.New("nemo hermes container image not found")
	}
	return sts, nil
}

// open is cutover's hold. It records the replica count it first sees in an
// annotation, and close puts that count back. A rerun reads the annotation,
// because the live count is already zero.
func (c *controlPath) open(ctx context.Context) error {
	sts, err := c.statefulSet(ctx)
	if err != nil {
		return err
	}
	keep := sts.replicas
	if recorded, found := sts.annotations[replicasAnnotation]; found {
		if keep, err = strconv.Atoi(recorded); err != nil {
			return fmt.Errorf("decode %s: %w", replicasAnnotation, err)
		}
	} else if _, err := c.kubectl(ctx, nil, "annotate", "statefulset", "nemo", replicasAnnotation+"="+strconv.Itoa(keep), "--overwrite"); err != nil {
		return err
	}
	c.held, c.replicas = true, keep
	return c.stop(ctx)
}

// stop holds nemo at zero, waits for nemo-0 to go, and makes the maintenance
// objects ready. It records no replica count, so restore can use it.
func (c *controlPath) stop(ctx context.Context) error {
	if _, err := c.kubectl(ctx, nil, "scale", "statefulset/nemo", "--replicas=0"); err != nil {
		return err
	}
	current, err := c.kubectl(ctx, nil, "get", "pod", "nemo-0", "--ignore-not-found", "-o", "name")
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(current)) != 0 {
		if _, err := c.kubectl(ctx, nil, "wait", "--for=delete", "pod/nemo-0", "--timeout=10m"); err != nil {
			return fmt.Errorf("wait for nemo-0 to stop: %w", err)
		}
	}
	return c.ensurePod(ctx)
}

// close deletes the maintenance Pod and policy. If the path is held, it sets the
// replica count the hold recorded and removes the annotation. A path that is not
// held leaves nemo's replicas alone.
func (c *controlPath) close(ctx context.Context) error {
	if _, err := c.kubectl(ctx, nil, "delete", "pod", maintName, "--ignore-not-found", "--wait=true"); err != nil {
		return err
	}
	if _, err := c.kubectl(ctx, nil, "delete", "networkpolicy", maintName, "--ignore-not-found", "--wait=true"); err != nil {
		return err
	}
	if !c.held {
		return nil
	}
	if _, err := c.kubectl(ctx, nil, "scale", "statefulset/nemo", "--replicas="+strconv.Itoa(c.replicas)); err != nil {
		return err
	}
	if _, err := c.kubectl(ctx, nil, "annotate", "statefulset", "nemo", replicasAnnotation+"-"); err != nil {
		return err
	}
	c.held = false
	return nil
}

// ensurePod makes the deny-all policy and the maintenance Pod match the spec, then
// waits for the Pod to be Ready.
func (c *controlPath) ensurePod(ctx context.Context) error {
	sts, err := c.statefulSet(ctx)
	if err != nil {
		return err
	}
	if err := c.ensure(ctx, "networkpolicy", maintPolicyObject(c.namespace)); err != nil {
		return err
	}
	if err := c.ensure(ctx, "pod", maintPodObject(c.namespace, sts.image)); err != nil {
		return err
	}
	_, err = c.kubectl(ctx, nil, "wait", "--for=condition=Ready", "pod/"+maintName, "--timeout=10m")
	return err
}

// ensure keeps a live object that matches body. Otherwise it deletes the object
// with --wait and creates body.
func (c *controlPath) ensure(ctx context.Context, kind string, body map[string]any) error {
	name := maintName
	want, err := canonicalMap(body)
	if err != nil {
		return err
	}
	live, err := c.kubectl(ctx, nil, "get", kind, name, "--ignore-not-found", "-o", "json")
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(live)) != 0 {
		var got map[string]any
		if err := json.Unmarshal(live, &got); err != nil {
			return fmt.Errorf("decode %s %s: %w", kind, name, err)
		}
		if reusable(kind, want, got) {
			return nil
		}
		if _, err := c.kubectl(ctx, nil, "delete", kind, name, "--ignore-not-found", "--wait=true"); err != nil {
			return err
		}
	}
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	_, err = c.kubectl(ctx, bytes.NewReader(data), "create", "-f", "-")
	return err
}

// reusable reports whether a live object can stay. It must not be terminating,
// its spec must match, and a Pod must carry the wanted labels.
func reusable(kind string, want, got map[string]any) bool {
	if stringField(mapField(got, "metadata"), "deletionTimestamp") != "" {
		return false
	}
	if kind == "pod" {
		phase := stringField(mapField(got, "status"), "phase")
		if phase != "Pending" && phase != "Running" {
			return false
		}
	}
	if kind == "pod" && !matchesWanted(mapField(mapField(want, "metadata"), "labels"), mapField(mapField(got, "metadata"), "labels")) {
		return false
	}
	return matchesWanted(want["spec"], got["spec"])
}

// canonicalMap round-trips v through JSON, so numbers and lists compare the way
// decoded objects do.
func canonicalMap(v map[string]any) (map[string]any, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// matchesWanted reports whether got holds every field of want with the same value.
// A field of got that want does not name must be an apiDefaults key or an empty
// object. want is in the JSON shape canonicalMap returns.
func matchesWanted(want, got any) bool {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return got == nil && len(w) == 0
		}
		for key, value := range g {
			if _, named := w[key]; !named && !apiDefaults[key] && !isEmptyObject(value) {
				return false
			}
		}
		for key, value := range w {
			if !matchesWanted(value, g[key]) {
				return false
			}
		}
		return true
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return false
		}
		for i := range w {
			if !matchesWanted(w[i], g[i]) {
				return false
			}
		}
		return true
	default:
		return want == got
	}
}

func isEmptyObject(v any) bool {
	m, ok := v.(map[string]any)
	return ok && len(m) == 0
}

// maintPodObject is the maintenance Pod. It has the Nemo image, both claims, and
// root with every capability dropped except the listed ones. It has no service
// account token and no privilege escalation.
func maintPodObject(namespace, image string) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name":      maintName,
			"namespace": namespace,
			"labels":    map[string]any{"nemo-move-maint": "true"},
		},
		"spec": map[string]any{
			"automountServiceAccountToken": false,
			"restartPolicy":                "Never",
			"containers": []any{map[string]any{
				"name":    "maintenance",
				"image":   image,
				"command": []string{"sleep", "infinity"},
				"securityContext": map[string]any{
					"runAsUser":                0,
					"allowPrivilegeEscalation": false,
					"capabilities": map[string]any{
						"drop": []string{"ALL"},
						"add":  []string{"CHOWN", "DAC_OVERRIDE", "FOWNER", "SETUID", "SETGID", "LINUX_IMMUTABLE"},
					},
				},
				"volumeMounts": []any{
					map[string]any{"name": "data", "mountPath": "/opt/data"},
					map[string]any{"name": "control", "mountPath": "/control"},
				},
			}},
			"volumes": []any{
				map[string]any{"name": "data", "persistentVolumeClaim": map[string]any{"claimName": "nemo-data"}},
				map[string]any{"name": "control", "persistentVolumeClaim": map[string]any{"claimName": "nemo-control"}},
			},
		},
	}
}

// maintPolicyObject is the deny-all policy for the maintenance Pod.
func maintPolicyObject(namespace string) map[string]any {
	return map[string]any{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "NetworkPolicy",
		"metadata":   map[string]any{"name": maintName, "namespace": namespace},
		"spec": map[string]any{
			"podSelector": map[string]any{"matchLabels": map[string]any{"nemo-move-maint": "true"}},
			"policyTypes": []string{"Ingress", "Egress"},
		},
	}
}
