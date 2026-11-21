// Package reconcile plans and applies node removals from the desired set and cluster state.
package reconcile

// Mode is the desired run mode: dry-run logs the plan, enforce writes it.
type Mode string

const (
	ModeDryRun  Mode = "dry-run"
	ModeEnforce Mode = "enforce"
)

// DesiredSet is the parsed desired-nodes ConfigMap.
type DesiredSet struct {
	Members              []string
	Retiring             []string
	Renamed              []string
	Mode                 Mode
	MinNodes             int
	NotReadyGraceSeconds int
}

// Annotation keys the reconciler owns on Nodes and Longhorn Nodes.
const (
	nodePriorKey     = "node-removal.rigel.build/cordoned"
	nodeDrainedKey   = "node-removal.rigel.build/drained"
	longhornPriorKey = "node-removal.rigel.build/prior"
	orphanSinceKey   = "node-removal.rigel.build/orphan-since"
)

// NodePrior is the JSON in the Node's cordoned annotation.
type NodePrior struct {
	Unschedulable bool `json:"unschedulable"`
}

// LonghornPrior is the JSON in the Longhorn node's prior annotation.
type LonghornPrior struct {
	AllowScheduling   bool `json:"allowScheduling"`
	EvictionRequested bool `json:"evictionRequested"`
}

// readyTrue is the Ready condition status of a Ready Node; uidKey is the metadata and precondition UID key.
const (
	readyTrue = "True"
	uidKey    = "uid"
)

// Target pins a write to one object by UID and resourceVersion at read time.
type Target struct {
	UID             string
	ResourceVersion string
}

// NodeView is one Kubernetes Node as the planner sees it.
type NodeView struct {
	Name          string
	Target        Target
	Ready         string // "True", "False" or "Unknown"
	ReadySince    string
	ControlPlane  bool
	Unschedulable bool
	Prior         *NodePrior
	Drained       bool
}

// LonghornNodeView is one Longhorn node CR as the planner sees it.
type LonghornNodeView struct {
	Name              string
	Target            Target
	AllowScheduling   bool
	EvictionRequested bool
	ReplicaCount      int
	Prior             *LonghornPrior
	OrphanSince       *string
	DeletionTimestamp *string
}

// PodView is one pod on a retiring Node as the planner sees it.
type PodView struct {
	Namespace     string
	Name          string
	Target        Target
	Node          string
	DaemonSet     bool
	Mirror        bool
	LonghornOwned bool
	Controller    bool
	PinnedTo      string // empty when unpinned
	OwnerKnown    bool
}

// Snapshot is one read of the cluster.
type Snapshot struct {
	Nodes    []NodeView
	Longhorn []LonghornNodeView
	Pods     []PodView
}

// HoldReason explains why an action was held instead of written.
type HoldReason string

const (
	HoldReady            HoldReason = "ready"
	HoldGrace            HoldReason = "grace"
	HoldControlPlane     HoldReason = "control-plane"
	HoldLonghornReplicas HoldReason = "longhorn-replicas"
	HoldLonghornDeleting HoldReason = "longhorn-deleting"
	HoldDeleteBudget     HoldReason = "delete-budget"
	HoldPinned           HoldReason = "pinned"
	HoldPinnedUnknown    HoldReason = "pinned-unknown"
	HoldUnmanaged        HoldReason = "unmanaged"
	HoldStalePlan        HoldReason = "stale-plan"
	HoldInvalidDesired   HoldReason = "invalid-desired"
	HoldConflict         HoldReason = "conflict"
	HoldEvictionRefused  HoldReason = "eviction-refused"
	HoldWebhookRefused   HoldReason = "webhook-refused"
)

// ActionKind names one planned write, or a hold.
type ActionKind string

const (
	KindCordon             ActionKind = "cordon"
	KindLonghornEvict      ActionKind = "longhorn-evict"
	KindLonghornRestore    ActionKind = "longhorn-restore"
	KindUncordon           ActionKind = "uncordon"
	KindEvictPod           ActionKind = "evict-pod"
	KindMarkDrained        ActionKind = "mark-drained"
	KindDeleteLonghornNode ActionKind = "delete-longhorn-node"
	KindDeleteNode         ActionKind = "delete-node"
	KindMarkOrphan         ActionKind = "mark-orphan"
	KindUnmarkOrphan       ActionKind = "unmark-orphan"
	KindHold               ActionKind = "hold"
)

// Action is one planned write or hold. Fields not used by its Kind stay zero.
type Action struct {
	Kind          ActionKind
	Node          string
	Target        Target
	NodePrior     *NodePrior     // cordon: nil writes the annotation as null; uncordon: required
	LonghornPrior *LonghornPrior // longhorn-evict: nil writes no prior; longhorn-restore: required
	Protective    bool           // cordon only
	Namespace     string         // evict-pod only
	Name          string         // evict-pod only
	At            string         // mark-orphan only
	Reason        HoldReason     // hold only
	Pod           string         // hold only, optional; empty when absent
}

// Key is kind plus "/" plus target UID. Two actions are the same when their keys match.
func (a Action) Key() string {
	return string(a.Kind) + "/" + a.Target.UID
}

// SafeBelowMin reports whether an action may run while the member count is below minNodes.
// Restorations and protective cordons are the only below-minimum writes.
func (a Action) SafeBelowMin() bool {
	return a.Kind == KindLonghornRestore ||
		a.Kind == KindUncordon ||
		a.Kind == KindUnmarkOrphan ||
		(a.Kind == KindCordon && a.Protective)
}

// Plan is the ordered actions for one run.
type Plan struct {
	Actions       []Action
	BelowMinNodes bool
}
