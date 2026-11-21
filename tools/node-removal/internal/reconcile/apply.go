package reconcile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/RigelBuild/nebula/tools/node-removal/internal/kube"
)

// Applied is the outcome of one action: written (OK), held with a reason, or skipped.
type Applied struct {
	OK      bool
	Hold    HoldReason
	Skipped string
}

// Runner applies planned actions. Out receives one JSON line per action.
type Runner struct {
	ReadDesired func() (DesiredSet, error)
	Now         func() time.Time
	Client      kube.Client
	Out         io.Writer
}

// logLine fixes the key order: event, node, mode, then reason and pod, or result.
type logLine struct {
	Event  string `json:"event"`
	Node   string `json:"node"`
	Mode   string `json:"mode"`
	Reason string `json:"reason,omitempty"`
	Pod    string `json:"pod,omitempty"`
	Result string `json:"result,omitempty"`
}

// Apply is the only write path. In dry-run it logs and calls no client method.
func (r Runner) Apply(action Action) (Applied, error) {
	desired, err := r.ReadDesired()
	if err != nil {
		result := Applied{Hold: HoldInvalidDesired}
		if action.Kind == KindHold {
			result = Applied{OK: true}
		}
		return result, r.logResult(action, ModeDryRun, result)
	}
	// Holds log the desired mode too, so a run's log shows which mode it ran in.
	mode := desired.Mode
	var result Applied
	if action.Kind == KindHold || mode == ModeDryRun {
		result = Applied{OK: true}
	} else {
		snap, err := Read(desired, r.Client)
		if err != nil {
			return Applied{}, err
		}
		fresh := Classify(desired, snap, r.Now())
		var current *Action
		for i := range fresh.Actions {
			if sameAction(action, fresh.Actions[i]) {
				current = &fresh.Actions[i]
				break
			}
		}
		if current == nil {
			result = Applied{Hold: HoldStalePlan}
		} else if result, err = r.execute(*current); err != nil {
			return Applied{}, err
		}
	}
	return result, r.logResult(action, mode, result)
}

// Run applies actions in order and stops on the first unmapped API failure.
func (r Runner) Run(p Plan) error {
	for _, action := range p.Actions {
		if _, err := r.Apply(action); err != nil {
			return err
		}
	}
	return nil
}

func (r Runner) logResult(action Action, mode Mode, result Applied) error {
	var line logLine
	switch {
	case action.Kind == KindHold:
		line = logLine{Event: "hold", Node: action.Node, Mode: string(mode), Reason: string(action.Reason), Pod: action.Pod}
	case result.Hold != "":
		line = logLine{Event: "hold", Node: action.Node, Mode: string(mode), Reason: string(result.Hold)}
	default:
		line = logLine{Event: string(action.Kind), Node: action.Node, Mode: string(mode), Result: resultName(result)}
	}
	encoded, err := EncodeLogLine(line)
	if err != nil {
		return fmt.Errorf("encoding log line: %w", err)
	}
	if _, err := fmt.Fprintln(r.Out, string(encoded)); err != nil {
		return fmt.Errorf("writing log line: %w", err)
	}
	return nil
}

func resultName(result Applied) string {
	if result.Skipped != "" {
		return result.Skipped
	}
	return "ok"
}

func sameAction(planned, current Action) bool {
	return planned.Kind != KindHold && current.Kind != KindHold &&
		planned.Kind == current.Kind && planned.Key() == current.Key()
}

// mapWriteResponse turns an apiserver answer into an outcome. Unmapped failures are errors.
func mapWriteResponse(response kube.Response, action Action) (Applied, error) {
	if response.OK() {
		return Applied{OK: true}, nil
	}
	switch response.Status {
	case 404:
		return Applied{Skipped: "not-found"}, nil
	case 409:
		return Applied{Hold: HoldConflict}, nil
	}
	if action.Kind == KindEvictPod && response.Status == 429 {
		return Applied{Hold: HoldEvictionRefused}, nil
	}
	if response.Status == 400 || response.Status == 403 {
		var value map[string]any
		if err := json.Unmarshal(response.Body, &value); err == nil {
			if msg, ok := value["message"].(string); ok && strings.HasPrefix(msg, "admission webhook") {
				return Applied{Hold: HoldWebhookRefused}, nil
			}
		}
	}
	return Applied{}, fmt.Errorf("Kubernetes write %s failed: HTTP %d %s", action.Kind, response.Status, response.Body) //nolint:staticcheck // ST1005: user-facing message, text is fixed.
}

// patchBody is a merge patch carrying the read-time UID and resourceVersion as preconditions.
// A nil annotation value is JSON null, which deletes the annotation.
func patchBody(target Target, spec map[string]any, annotations map[string]any) map[string]any {
	return map[string]any{
		"metadata": map[string]any{
			uidKey:            target.UID,
			"resourceVersion": target.ResourceVersion,
			"annotations":     annotations,
		},
		"spec": spec,
	}
}

func deleteBody(target Target) map[string]any {
	return map[string]any{
		"apiVersion":    "v1",
		"kind":          "DeleteOptions",
		"preconditions": map[string]any{uidKey: target.UID},
	}
}

func encodeJSON(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encoding annotation value: %w", err)
	}
	return string(encoded), nil
}

// EncodeLogLine returns one log line as JSON with <, > and & left unescaped, as JSON.stringify
// writes them. The trailing newline the encoder adds is dropped.
func EncodeLogLine(value any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// nodeRequest returns the request for a Node or Node-delete action, or nil when the kind is not one.
func (r Runner) nodeRequest(action Action, path string) (kube.Response, bool, error) {
	client := r.Client
	switch action.Kind {
	case KindCordon:
		annotations := map[string]any{}
		if action.NodePrior != nil {
			prior, err := encodeJSON(action.NodePrior)
			if err != nil {
				return kube.Response{}, true, err
			}
			annotations[nodePriorKey] = prior
		}
		resp, err := client.Patch(path, patchBody(action.Target, map[string]any{"unschedulable": true}, annotations))
		return resp, true, err
	case KindUncordon:
		resp, err := client.Patch(path, patchBody(
			action.Target,
			map[string]any{"unschedulable": action.NodePrior.Unschedulable},
			map[string]any{nodePriorKey: nil, nodeDrainedKey: nil},
		))
		return resp, true, err
	case KindMarkDrained:
		resp, err := client.Patch(path, patchBody(action.Target, map[string]any{},
			map[string]any{nodeDrainedKey: r.Now().UTC().Format(isoMillis)}))
		return resp, true, err
	case KindDeleteLonghornNode, KindDeleteNode:
		resp, err := client.Delete(path, deleteBody(action.Target))
		return resp, true, err
	default:
		return kube.Response{}, false, nil
	}
}

// longhornRequest returns the request for a Longhorn Node action, or nil when the kind is not one.
func (r Runner) longhornRequest(action Action, path string) (kube.Response, bool, error) {
	client := r.Client
	switch action.Kind {
	case KindLonghornEvict:
		annotations := map[string]any{}
		if action.LonghornPrior != nil {
			prior, err := encodeJSON(action.LonghornPrior)
			if err != nil {
				return kube.Response{}, true, err
			}
			annotations[longhornPriorKey] = prior
		}
		resp, err := client.Patch(path, patchBody(action.Target,
			map[string]any{"allowScheduling": false, "evictionRequested": true}, annotations))
		return resp, true, err
	case KindLonghornRestore:
		resp, err := client.Patch(path, patchBody(action.Target,
			map[string]any{
				"allowScheduling":   action.LonghornPrior.AllowScheduling,
				"evictionRequested": action.LonghornPrior.EvictionRequested,
			},
			map[string]any{longhornPriorKey: nil}))
		return resp, true, err
	case KindMarkOrphan:
		resp, err := client.Patch(path, patchBody(action.Target, map[string]any{}, map[string]any{orphanSinceKey: action.At}))
		return resp, true, err
	case KindUnmarkOrphan:
		resp, err := client.Patch(path, patchBody(action.Target, map[string]any{}, map[string]any{orphanSinceKey: nil}))
		return resp, true, err
	default:
		return kube.Response{}, false, nil
	}
}

// evictRequest posts a pod eviction with the read-time UID and resourceVersion as preconditions.
func (r Runner) evictRequest(action Action) (kube.Response, bool, error) {
	if action.Kind != KindEvictPod {
		return kube.Response{}, false, nil
	}
	path := "/api/v1/namespaces/" + url.PathEscape(action.Namespace) + "/pods/" + url.PathEscape(action.Name) + "/eviction"
	resp, err := r.Client.Post(path, map[string]any{
		"apiVersion": "policy/v1",
		"kind":       "Eviction",
		"metadata":   map[string]any{"name": action.Name, "namespace": action.Namespace},
		"deleteOptions": map[string]any{
			"preconditions": map[string]any{
				uidKey:            action.Target.UID,
				"resourceVersion": action.Target.ResourceVersion,
			},
		},
	})
	return resp, true, err
}

// execute sends the one write for a fresh-planned action.
func (r Runner) execute(action Action) (Applied, error) {
	longhornAction := action.Kind == KindLonghornEvict || action.Kind == KindLonghornRestore ||
		action.Kind == KindDeleteLonghornNode || action.Kind == KindMarkOrphan || action.Kind == KindUnmarkOrphan
	base := "/api/v1/nodes"
	if longhornAction {
		base = "/apis/longhorn.io/v1beta2/namespaces/longhorn-system/nodes"
	}
	path := base + "/" + url.PathEscape(action.Node)
	resp, ok, err := r.nodeRequest(action, path)
	if err != nil {
		return Applied{}, err
	}
	if !ok {
		if resp, ok, err = r.longhornRequest(action, path); err != nil {
			return Applied{}, err
		}
	}
	if !ok {
		if resp, ok, err = r.evictRequest(action); err != nil {
			return Applied{}, err
		}
	}
	if !ok {
		return Applied{}, fmt.Errorf("no write for %s", action.Kind)
	}
	return mapWriteResponse(resp, action)
}
