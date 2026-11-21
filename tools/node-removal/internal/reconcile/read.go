package reconcile

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/RigelBuild/nebula/tools/node-removal/internal/kube"
)

type obj = map[string]any

func asObj(value any) (obj, bool) {
	o, ok := value.(obj)
	return o, ok
}

// objOrEmpty returns an empty object for a missing or non-object value.
func objOrEmpty(value any) obj {
	if o, ok := asObj(value); ok {
		return o
	}
	return obj{}
}

func requiredString(value any, field string) (string, error) {
	s, ok := value.(string)
	if !ok || s == "" {
		return "", fmt.Errorf("Kubernetes response has invalid %s", field) //nolint:staticcheck // ST1005: user-facing message, text is fixed.
	}
	return s, nil
}

func targetFrom(value any) (Target, error) {
	metadata := objOrEmpty(objOrEmpty(value)["metadata"])
	uid, err := requiredString(metadata[uidKey], "metadata.uid")
	if err != nil {
		return Target{}, err
	}
	rv, err := requiredString(metadata["resourceVersion"], "metadata.resourceVersion")
	if err != nil {
		return Target{}, err
	}
	return Target{UID: uid, ResourceVersion: rv}, nil
}

func nodePrior(annotations obj) (*NodePrior, error) {
	raw, present := annotations[nodePriorKey]
	if !present {
		return nil, nil //nolint:nilnil // an absent annotation is no prior
	}
	s, ok := raw.(string)
	if !ok {
		return nil, fmt.Errorf("invalid %s annotation", nodePriorKey)
	}
	var value obj
	if err := json.Unmarshal([]byte(s), &value); err != nil {
		return nil, fmt.Errorf("invalid %s annotation: %w", nodePriorKey, err)
	}
	unsched, ok := value["unschedulable"].(bool)
	if !ok {
		return nil, fmt.Errorf("invalid %s annotation", nodePriorKey)
	}
	return &NodePrior{Unschedulable: unsched}, nil
}

func longhornPrior(annotations obj) (*LonghornPrior, error) {
	raw, present := annotations[longhornPriorKey]
	if !present {
		return nil, nil //nolint:nilnil // an absent annotation is no prior
	}
	s, ok := raw.(string)
	if !ok {
		return nil, fmt.Errorf("invalid %s annotation", longhornPriorKey)
	}
	var value obj
	if err := json.Unmarshal([]byte(s), &value); err != nil {
		return nil, fmt.Errorf("invalid %s annotation: %w", longhornPriorKey, err)
	}
	allow, okAllow := value["allowScheduling"].(bool)
	evict, okEvict := value["evictionRequested"].(bool)
	if !okAllow || !okEvict {
		return nil, fmt.Errorf("invalid %s annotation", longhornPriorKey)
	}
	return &LonghornPrior{AllowScheduling: allow, EvictionRequested: evict}, nil
}

// itemsFrom returns the list items; each item must be an object.
func itemsFrom(value any, field string) ([]obj, error) {
	list, ok := asObj(value)
	if !ok {
		return nil, fmt.Errorf("invalid Kubernetes %s list", field)
	}
	raw, ok := list["items"].([]any)
	if !ok {
		return nil, fmt.Errorf("invalid Kubernetes %s list", field)
	}
	items := make([]obj, 0, len(raw))
	for _, item := range raw {
		o, ok := asObj(item)
		if !ok {
			return nil, fmt.Errorf("invalid Kubernetes %s list", field)
		}
		items = append(items, o)
	}
	return items, nil
}

// jsonBody decodes a 2xx response into an object. A non-2xx status is an error with the body.
func jsonBody(response kube.Response, operation string) (obj, error) {
	if !response.OK() {
		return nil, fmt.Errorf("%s failed: HTTP %d %s", operation, response.Status, response.Body)
	}
	var value any
	if err := json.Unmarshal(response.Body, &value); err != nil {
		return nil, fmt.Errorf("%s returned invalid JSON: %w", operation, err)
	}
	o, ok := asObj(value)
	if !ok {
		return nil, fmt.Errorf("%s returned a non-object response", operation)
	}
	return o, nil
}

func nodeView(raw obj) (NodeView, error) {
	metadata := objOrEmpty(raw["metadata"])
	status := objOrEmpty(raw["status"])
	conditions, _ := status["conditions"].([]any)
	var ready obj
	for _, c := range conditions {
		if condition, ok := asObj(c); ok && condition["type"] == "Ready" {
			ready = condition
			break
		}
	}
	name, err := requiredString(metadata["name"], "metadata.name")
	if err != nil {
		return NodeView{}, err
	}
	target, err := targetFrom(raw)
	if err != nil {
		return NodeView{}, err
	}
	labels := objOrEmpty(metadata["labels"])
	annotations := objOrEmpty(metadata["annotations"])
	prior, err := nodePrior(annotations)
	if err != nil {
		return NodeView{}, err
	}
	readyStatus, _ := ready["status"].(string)
	readyValue := "Unknown"
	if readyStatus == readyTrue || readyStatus == "False" {
		readyValue = readyStatus
	}
	readySince, _ := ready["lastTransitionTime"].(string)
	_, cpLabel := labels["node-role.kubernetes.io/control-plane"]
	_, masterLabel := labels["node-role.kubernetes.io/master"]
	_, drained := annotations[nodeDrainedKey].(string)
	return NodeView{
		Name:          name,
		Target:        target,
		Ready:         readyValue,
		ReadySince:    readySince,
		ControlPlane:  cpLabel || masterLabel,
		Unschedulable: objOrEmpty(raw["spec"])["unschedulable"] == true,
		Prior:         prior,
		Drained:       drained,
	}, nil
}

// countReplicas counts scheduled replicas across every disk in status.diskStatus.
func countReplicas(status any) int {
	disks := objOrEmpty(objOrEmpty(status)["diskStatus"])
	count := 0
	for _, disk := range disks {
		count += len(objOrEmpty(objOrEmpty(disk)["scheduledReplica"]))
	}
	return count
}

func longhornView(raw obj) (LonghornNodeView, error) {
	metadata := objOrEmpty(raw["metadata"])
	annotations := objOrEmpty(metadata["annotations"])
	spec := objOrEmpty(raw["spec"])
	name, err := requiredString(metadata["name"], "metadata.name")
	if err != nil {
		return LonghornNodeView{}, err
	}
	target, err := targetFrom(raw)
	if err != nil {
		return LonghornNodeView{}, err
	}
	prior, err := longhornPrior(annotations)
	if err != nil {
		return LonghornNodeView{}, err
	}
	view := LonghornNodeView{
		Name:              name,
		Target:            target,
		AllowScheduling:   spec["allowScheduling"] != false,
		EvictionRequested: spec["evictionRequested"] == true,
		ReplicaCount:      countReplicas(raw["status"]),
		Prior:             prior,
	}
	if s, ok := annotations[orphanSinceKey].(string); ok {
		view.OrphanSince = &s
	}
	if s, ok := metadata["deletionTimestamp"].(string); ok {
		view.DeletionTimestamp = &s
	}
	return view, nil
}

func podView(raw obj, client kube.Client) (PodView, error) {
	metadata := objOrEmpty(raw["metadata"])
	spec := objOrEmpty(raw["spec"])
	refs, _ := metadata["ownerReferences"].([]any)
	annotations := objOrEmpty(metadata["annotations"])
	namespace, err := requiredString(metadata["namespace"], "pod.metadata.namespace")
	if err != nil {
		return PodView{}, err
	}
	name, err := requiredString(metadata["name"], "pod.metadata.name")
	if err != nil {
		return PodView{}, err
	}
	target, err := targetFrom(raw)
	if err != nil {
		return PodView{}, err
	}
	node, err := requiredString(spec["nodeName"], "pod.spec.nodeName")
	if err != nil {
		return PodView{}, err
	}
	view := PodView{Namespace: namespace, Name: name, Target: target, Node: node}
	for _, r := range refs {
		owner, ok := asObj(r)
		if !ok {
			continue
		}
		if owner["kind"] == "DaemonSet" {
			view.DaemonSet = true
		}
		if apiVersion, _ := owner["apiVersion"].(string); strings.HasPrefix(apiVersion, "longhorn.io/") {
			view.LonghornOwned = true
		}
	}
	_, view.Mirror = annotations["kubernetes.io/config.mirror"]
	info, err := ownerDetails(raw, client)
	if err != nil {
		return PodView{}, err
	}
	view.Controller = info.controller
	view.OwnerKnown = info.ownerKnown
	view.PinnedTo = info.pinnedTo
	return view, nil
}

// Read lists cluster inputs and GETs each candidate Node's matching Longhorn CR; only 404
// means absent. Any other failure aborts the read.
func Read(desired DesiredSet, client kube.Client) (Snapshot, error) {
	nodesResponse, err := client.Get("/api/v1/nodes")
	if err != nil {
		return Snapshot{}, err
	}
	lhResponse, err := client.Get("/apis/longhorn.io/v1beta2/namespaces/longhorn-system/nodes")
	if err != nil {
		return Snapshot{}, err
	}
	nodesRaw, err := jsonBody(nodesResponse, "GET Nodes")
	if err != nil {
		return Snapshot{}, err
	}
	longhornRaw, err := jsonBody(lhResponse, "GET Longhorn nodes")
	if err != nil {
		return Snapshot{}, err
	}
	nodes, err := readNodes(nodesRaw)
	if err != nil {
		return Snapshot{}, err
	}
	longhorn, err := readLonghorn(longhornRaw, nodes, client)
	if err != nil {
		return Snapshot{}, err
	}
	pods, err := readPods(desired, client)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Nodes: nodes, Longhorn: longhorn, Pods: pods}, nil
}

// readNodes builds a view for every listed Node.
func readNodes(nodesRaw obj) ([]NodeView, error) {
	nodeItems, err := itemsFrom(nodesRaw, "Node")
	if err != nil {
		return nil, err
	}
	nodes := make([]NodeView, 0, len(nodeItems))
	for _, item := range nodeItems {
		view, err := nodeView(item)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, view)
	}
	return nodes, nil
}

// readLonghorn merges the Longhorn list with a per-Node GET. A CR the GET finds is appended
// after the list order, and a 404 drops the entry.
func readLonghorn(longhornRaw obj, nodes []NodeView, client kube.Client) ([]LonghornNodeView, error) {
	lhItems, err := itemsFrom(longhornRaw, "Longhorn node")
	if err != nil {
		return nil, err
	}
	longhornByName := map[string]LonghornNodeView{}
	order := []string{}
	for _, item := range lhItems {
		view, err := longhornView(item)
		if err != nil {
			return nil, err
		}
		if _, seen := longhornByName[view.Name]; !seen {
			order = append(order, view.Name)
		}
		longhornByName[view.Name] = view
	}
	for _, node := range nodes {
		if err := mergeLonghornNode(node.Name, longhornByName, &order, client); err != nil {
			return nil, err
		}
	}
	longhorn := []LonghornNodeView{}
	for _, name := range order {
		if view, ok := longhornByName[name]; ok {
			longhorn = append(longhorn, view)
		}
	}
	return longhorn, nil
}

// mergeLonghornNode GETs one Node's Longhorn CR into byName and records its name in order when
// the list missed it; a 404 removes it.
func mergeLonghornNode(name string, byName map[string]LonghornNodeView, order *[]string, client kube.Client) error {
	response, err := client.Get("/apis/longhorn.io/v1beta2/namespaces/longhorn-system/nodes/" + url.PathEscape(name))
	if err != nil {
		return err
	}
	if response.Status == 404 {
		delete(byName, name)
		return nil
	}
	raw, err := jsonBody(response, "GET Longhorn node "+name)
	if err != nil {
		return err
	}
	view, err := longhornView(raw)
	if err != nil {
		return err
	}
	if _, seen := byName[name]; !seen {
		*order = append(*order, name)
	}
	byName[name] = view
	return nil
}

// readPods lists pods on each retiring node, then builds their views.
func readPods(desired DesiredSet, client kube.Client) ([]PodView, error) {
	pods := []PodView{}
	for _, name := range desired.Retiring {
		selector := url.QueryEscape("spec.nodeName=" + name)
		response, err := client.Get("/api/v1/pods?fieldSelector=" + selector)
		if err != nil {
			return nil, err
		}
		raw, err := jsonBody(response, "GET pods on "+name)
		if err != nil {
			return nil, err
		}
		items, err := itemsFrom(raw, "Pod")
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			view, err := podView(item, client)
			if err != nil {
				return nil, err
			}
			pods = append(pods, view)
		}
	}
	return pods, nil
}
