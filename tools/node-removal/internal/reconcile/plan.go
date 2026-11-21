package reconcile

import (
	"slices"
	"strings"
	"time"
)

// isoMillis matches JavaScript's Date.prototype.toISOString output.
const isoMillis = "2006-01-02T15:04:05.000Z"

type deletion struct {
	node   string
	target Target
	kind   ActionKind
	rank   int
}

type step struct {
	actions  []Action
	deletion *deletion
}

type planCtx struct {
	desired       DesiredSet
	pods          []PodView
	belowMinNodes bool
	now           time.Time
}

func hold(node string, reason HoldReason, pod string) Action {
	return Action{Kind: KindHold, Node: node, Reason: reason, Pod: pod}
}

// graceElapsed reports whether since parses and is at least graceSeconds before now.
func graceElapsed(since string, now time.Time, graceSeconds int) bool {
	timestamp, err := time.Parse(time.RFC3339Nano, since)
	return err == nil && now.Sub(timestamp) >= time.Duration(graceSeconds)*time.Second
}

// podHold returns the hold for a pod that blocks draining its Node, or nil.
func podHold(pod PodView, node string) *Action {
	if pod.DaemonSet || pod.Mirror || pod.LonghornOwned {
		return nil
	}
	name := pod.Namespace + "/" + pod.Name
	var h Action
	switch {
	case !pod.Controller:
		h = hold(node, HoldUnmanaged, name)
	case !pod.OwnerKnown:
		h = hold(node, HoldPinnedUnknown, name)
	case pod.PinnedTo == node:
		h = hold(node, HoldPinned, name)
	default:
		return nil
	}
	return &h
}

func cordonAction(node NodeView, protective bool) Action {
	action := Action{
		Kind:       KindCordon,
		Node:       node.Name,
		Target:     node.Target,
		Protective: protective,
	}
	if node.Prior == nil {
		action.NodePrior = &NodePrior{Unschedulable: false}
	}
	return action
}

// orphanUnmark clears an orphan mark on a Longhorn node whose Node is present.
func orphanUnmark(node NodeView, longhorn *LonghornNodeView) []Action {
	if longhorn == nil || longhorn.OrphanSince == nil {
		return nil
	}
	return []Action{{Kind: KindUnmarkOrphan, Node: node.Name, Target: longhorn.Target}}
}

func safeOnly(actions []Action) []Action {
	out := []Action{}
	for _, action := range actions {
		if action.SafeBelowMin() {
			out = append(out, action)
		}
	}
	return out
}

func memberActions(node NodeView, longhorn *LonghornNodeView, belowMinNodes bool) []Action {
	restorative := orphanUnmark(node, longhorn)
	if longhorn != nil && longhorn.Prior != nil {
		restorative = append(restorative, Action{
			Kind:          KindLonghornRestore,
			Node:          node.Name,
			Target:        longhorn.Target,
			LonghornPrior: longhorn.Prior,
		})
	}
	if node.Prior != nil {
		restorative = append(restorative, Action{
			Kind:      KindUncordon,
			Node:      node.Name,
			Target:    node.Target,
			NodePrior: node.Prior,
		})
	}
	if belowMinNodes {
		return safeOnly(restorative)
	}
	return restorative
}

func evictablePods(node NodeView, pods []PodView) []Action {
	evictions := []Action{}
	for _, pod := range pods {
		if pod.DaemonSet || pod.Mirror || pod.LonghornOwned || podHold(pod, node.Name) != nil {
			continue
		}
		evictions = append(evictions, Action{
			Kind:      KindEvictPod,
			Node:      node.Name,
			Namespace: pod.Namespace,
			Name:      pod.Name,
			Target:    pod.Target,
		})
	}
	return evictions
}

// allExempt reports whether every pod is a DaemonSet, mirror or Longhorn-owned pod.
func allExempt(pods []PodView) bool {
	for _, pod := range pods {
		if !pod.DaemonSet && !pod.Mirror && !pod.LonghornOwned {
			return false
		}
	}
	return true
}

func replicaCount(longhorn *LonghornNodeView) int {
	if longhorn == nil {
		return 0
	}
	return longhorn.ReplicaCount
}

func retiringActions(node NodeView, longhorn *LonghornNodeView, ctx planCtx) []Action {
	if ctx.belowMinNodes {
		return nil
	}
	pods := []PodView{}
	for _, pod := range ctx.pods {
		if pod.Node == node.Name {
			pods = append(pods, pod)
		}
	}
	blockers := []Action{}
	for _, pod := range pods {
		if h := podHold(pod, node.Name); h != nil {
			blockers = append(blockers, *h)
		}
	}
	actions := orphanUnmark(node, longhorn)
	if !node.Unschedulable {
		actions = append(actions, cordonAction(node, false))
	}
	if longhorn != nil && (longhorn.AllowScheduling || !longhorn.EvictionRequested) {
		evict := Action{Kind: KindLonghornEvict, Node: node.Name, Target: longhorn.Target}
		if longhorn.Prior == nil {
			evict.LonghornPrior = &LonghornPrior{
				AllowScheduling:   longhorn.AllowScheduling,
				EvictionRequested: longhorn.EvictionRequested,
			}
		}
		actions = append(actions, evict)
	}
	actions = append(actions, blockers...)
	if node.Unschedulable {
		actions = append(actions, evictablePods(node, pods)...)
	}
	if node.Unschedulable && len(blockers) == 0 && allExempt(pods) && replicaCount(longhorn) == 0 && !node.Drained {
		actions = append(actions, Action{Kind: KindMarkDrained, Node: node.Name, Target: node.Target})
	}
	return actions
}

func readyActions(node NodeView, longhorn *LonghornNodeView, belowMinNodes bool) []Action {
	restorative := orphanUnmark(node, longhorn)
	if !node.Unschedulable {
		restorative = append(restorative, cordonAction(node, true))
	}
	restorative = append(restorative, hold(node.Name, HoldReady, ""))
	if belowMinNodes {
		return safeOnly(restorative)
	}
	return restorative
}

func notReadyStep(node NodeView, longhorn *LonghornNodeView, ctx planCtx) step {
	if ctx.belowMinNodes {
		return step{}
	}
	if !graceElapsed(node.ReadySince, ctx.now, ctx.desired.NotReadyGraceSeconds) {
		return step{actions: []Action{hold(node.Name, HoldGrace, "")}}
	}
	if replicaCount(longhorn) > 0 {
		return step{actions: []Action{hold(node.Name, HoldLonghornReplicas, "")}}
	}
	if longhorn != nil && longhorn.DeletionTimestamp != nil {
		return step{actions: []Action{hold(node.Name, HoldLonghornDeleting, "")}}
	}
	if longhorn == nil {
		return step{deletion: &deletion{node: node.Name, target: node.Target, kind: KindDeleteNode, rank: 0}}
	}
	return step{deletion: &deletion{node: node.Name, target: longhorn.Target, kind: KindDeleteLonghornNode, rank: 1}}
}

func classifyNode(node NodeView, longhorn *LonghornNodeView, ctx planCtx) step {
	if node.ControlPlane && (node.Ready == readyTrue || !slices.Contains(ctx.desired.Renamed, node.Name)) {
		return step{actions: []Action{hold(node.Name, HoldControlPlane, "")}}
	}
	if slices.Contains(ctx.desired.Members, node.Name) {
		return step{actions: memberActions(node, longhorn, ctx.belowMinNodes)}
	}
	if slices.Contains(ctx.desired.Retiring, node.Name) {
		return step{actions: retiringActions(node, longhorn, ctx)}
	}
	if node.Ready == readyTrue {
		return step{actions: readyActions(node, longhorn, ctx.belowMinNodes)}
	}
	return notReadyStep(node, longhorn, ctx)
}

// classifyOrphan handles a Longhorn node whose Node object is gone.
func classifyOrphan(longhorn LonghornNodeView, ctx planCtx) step {
	if longhorn.OrphanSince == nil {
		if ctx.belowMinNodes {
			return step{}
		}
		return step{actions: []Action{{
			Kind:   KindMarkOrphan,
			Node:   longhorn.Name,
			Target: longhorn.Target,
			At:     ctx.now.UTC().Format(isoMillis),
		}}}
	}
	if ctx.belowMinNodes {
		return step{}
	}
	if !graceElapsed(*longhorn.OrphanSince, ctx.now, ctx.desired.NotReadyGraceSeconds) {
		return step{actions: []Action{hold(longhorn.Name, HoldGrace, "")}}
	}
	if longhorn.ReplicaCount > 0 {
		return step{actions: []Action{hold(longhorn.Name, HoldLonghornReplicas, "")}}
	}
	if longhorn.DeletionTimestamp != nil {
		return step{actions: []Action{hold(longhorn.Name, HoldLonghornDeleting, "")}}
	}
	return step{deletion: &deletion{node: longhorn.Name, target: longhorn.Target, kind: KindDeleteLonghornNode, rank: 2}}
}

// deletionActions runs only the first deletion in rank order; the rest hold on the budget.
func deletionActions(deletions []deletion) []Action {
	slices.SortStableFunc(deletions, func(a, b deletion) int {
		if a.rank != b.rank {
			return a.rank - b.rank
		}
		return strings.Compare(a.node, b.node)
	})
	actions := make([]Action, 0, len(deletions))
	for i, d := range deletions {
		if i == 0 {
			actions = append(actions, Action{Kind: d.kind, Node: d.node, Target: d.target})
			continue
		}
		actions = append(actions, hold(d.node, HoldDeleteBudget, ""))
	}
	return actions
}

// Classify is the pure classification from the node-removal safety table.
func Classify(desired DesiredSet, snap Snapshot, now time.Time) Plan {
	nodes := map[string]NodeView{}
	for _, node := range snap.Nodes {
		nodes[node.Name] = node
	}
	longhornByName := map[string]LonghornNodeView{}
	for _, longhorn := range snap.Longhorn {
		longhornByName[longhorn.Name] = longhorn
	}
	memberCount := 0
	for _, name := range desired.Members {
		if _, ok := nodes[name]; ok {
			memberCount++
		}
	}
	if memberCount == 0 {
		return Plan{Actions: []Action{}, BelowMinNodes: true}
	}
	belowMinNodes := memberCount < desired.MinNodes
	ctx := planCtx{desired: desired, pods: snap.Pods, belowMinNodes: belowMinNodes, now: now}
	actions := []Action{}
	deletions := []deletion{}
	for _, node := range snap.Nodes {
		var lh *LonghornNodeView
		if view, ok := longhornByName[node.Name]; ok {
			lh = &view
		}
		s := classifyNode(node, lh, ctx)
		actions = append(actions, s.actions...)
		if s.deletion != nil {
			deletions = append(deletions, *s.deletion)
		}
	}
	for _, longhorn := range snap.Longhorn {
		if _, ok := nodes[longhorn.Name]; ok {
			continue
		}
		s := classifyOrphan(longhorn, ctx)
		actions = append(actions, s.actions...)
		if s.deletion != nil {
			deletions = append(deletions, *s.deletion)
		}
	}
	actions = append(actions, deletionActions(deletions)...)
	return Plan{Actions: actions, BelowMinNodes: belowMinNodes}
}
