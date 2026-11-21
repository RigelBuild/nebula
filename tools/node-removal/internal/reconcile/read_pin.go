package reconcile

import (
	"encoding/json"
	"net/url"
	"strings"

	"github.com/RigelBuild/nebula/tools/node-removal/internal/kube"
)

// hostnamePin returns the value of a single-valued kubernetes.io/hostname In requirement.
func hostnamePin(r any) (string, bool) {
	expr, ok := asObj(r)
	if !ok {
		return "", false
	}
	values, _ := expr["values"].([]any)
	if expr["key"] != "kubernetes.io/hostname" || expr["operator"] != "In" || len(values) != 1 {
		return "", false
	}
	s, ok := values[0].(string)
	return s, ok
}

// termPin returns the first hostname pin in one nodeSelectorTerm, checking expressions then fields.
func termPin(t any) (string, bool) {
	term, ok := asObj(t)
	if !ok {
		return "", false
	}
	for _, key := range []string{"matchExpressions", "matchFields"} {
		requirements, _ := term[key].([]any)
		for _, r := range requirements {
			if s, ok := hostnamePin(r); ok {
				return s, true
			}
		}
	}
	return "", false
}

// requiredNodePin returns the single kubernetes.io/hostname value a selector pins to, or "".
func requiredNodePin(required obj) string {
	terms, _ := required["nodeSelectorTerms"].([]any)
	for _, t := range terms {
		if s, ok := termPin(t); ok {
			return s
		}
	}
	return ""
}

// templatePin returns the node a pod template pins to, or "" when unpinned.
func templatePin(value any) string {
	spec := objOrEmpty(objOrEmpty(value)["spec"])
	if s, ok := spec["nodeName"].(string); ok && s != "" {
		return s
	}
	if s, ok := objOrEmpty(spec["nodeSelector"])["kubernetes.io/hostname"].(string); ok {
		return s
	}
	affinity := objOrEmpty(objOrEmpty(spec["affinity"])["nodeAffinity"])
	return requiredNodePin(objOrEmpty(affinity["requiredDuringSchedulingIgnoredDuringExecution"]))
}

// ownerPath maps a controller owner to the apps or batch GET path, or "" for other kinds.
func ownerPath(owner obj, metadata obj) string {
	kind, _ := owner["kind"].(string)
	apiVersion, _ := owner["apiVersion"].(string)
	name, _ := owner["name"].(string)
	namespace, _ := metadata["namespace"].(string)
	ns := url.PathEscape(namespace)
	n := url.PathEscape(name)
	switch {
	case kind == "ReplicaSet" && strings.HasPrefix(apiVersion, "apps/"):
		return "/apis/apps/v1/namespaces/" + ns + "/replicasets/" + n
	case kind == "StatefulSet" && strings.HasPrefix(apiVersion, "apps/"):
		return "/apis/apps/v1/namespaces/" + ns + "/statefulsets/" + n
	case kind == "Job" && strings.HasPrefix(apiVersion, "batch/"):
		return "/apis/batch/v1/namespaces/" + ns + "/jobs/" + n
	default:
		return ""
	}
}

type ownerInfo struct {
	controller bool
	ownerKnown bool
	pinnedTo   string
}

// ownerDetails resolves a pod's controller owner. A failed GET or unreadable owner body leaves
// it unknown, which holds the pod.
func ownerDetails(pod obj, client kube.Client) (ownerInfo, error) {
	metadata := objOrEmpty(pod["metadata"])
	refs, _ := metadata["ownerReferences"].([]any)
	var owner obj
	for _, r := range refs {
		if ref, ok := asObj(r); ok && ref["controller"] == true {
			owner = ref
			break
		}
	}
	if owner == nil {
		return ownerInfo{controller: false, ownerKnown: true}, nil
	}
	path := ownerPath(owner, metadata)
	if path == "" {
		return ownerInfo{controller: true, ownerKnown: false}, nil
	}
	response, err := client.Get(path)
	if err != nil {
		return ownerInfo{}, err
	}
	if !response.OK() {
		return ownerInfo{controller: true, ownerKnown: false}, nil
	}
	var raw any
	if err := json.Unmarshal(response.Body, &raw); err != nil {
		return ownerInfo{controller: true, ownerKnown: false}, nil //nolint:nilerr // an unreadable owner holds the pod
	}
	o, ok := asObj(raw)
	if !ok {
		return ownerInfo{controller: true, ownerKnown: false}, nil
	}
	return ownerInfo{controller: true, ownerKnown: true, pinnedTo: templatePin(objOrEmpty(o["spec"])["template"])}, nil
}
