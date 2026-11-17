// Package capacity answers one question the recommender cannot: if requests shrink, does any node actually become removable?
//
// Lowering a container's requests is Resource Efficiency. Fitting the same pods on fewer nodes is Capacity Optimization.
// Only releasing a node is Infrastructure Cost Reduction. This package computes the second and states the third as a
// candidate, never as a saving.
//
// The simulation is a first-fit-decreasing check on CPU and memory REQUESTS. When the caller supplies them it also honours
// the scheduling constraints that most often make a "fits on paper" move impossible: cordoned nodes, NoSchedule/NoExecute
// taints and tolerations, nodeSelector, the zone of a zonal volume, required per-node anti-affinity, a PodDisruptionBudget
// that allows no disruption right now, and the last node of a zone. What it still cannot see (preferred affinities, topology
// spread constraints, inter-pod affinity) it says in the report. It never drains, cordons or deletes anything.
package capacity

import (
	"fmt"
	"sort"
)

// Node is a schedulable node.
type Node struct {
	ProviderID                     string
	Name                           string
	CPUAllocatable, MemAllocatable float64
	// HourlyUSD is an optional, caller-supplied price used only for the candidate estimate.
	HourlyUSD float64
	// Optional scheduling facts. Zero values mean "not supplied" and leave the result as a requests-only check.
	Labels        map[string]string
	Taints        []Taint
	Zone          string // topology.kubernetes.io/zone
	Unschedulable bool   // cordoned: it cannot receive pods
}

// Taint is a node taint. Only NoSchedule and NoExecute keep a pod away; PreferNoSchedule is a preference.
type Taint struct{ Key, Value, Effect string }

// Toleration mirrors the Kubernetes toleration fields that matter for placement.
type Toleration struct{ Key, Operator, Value, Effect string }

func (t Toleration) tolerates(x Taint) bool {
	if t.Effect != "" && t.Effect != x.Effect {
		return false
	}
	if t.Key == "" {
		return t.Operator == "Exists" // an empty key with Exists tolerates everything
	}
	if t.Key != x.Key {
		return false
	}
	return t.Operator == "Exists" || t.Value == x.Value
}

// Pod is a scheduled pod.
type Pod struct {
	Namespace, Name, Node  string
	CPURequest, MemRequest float64
	// DaemonSet pods run on every node and are ignored when judging whether a node can be emptied.
	DaemonSet bool
	// Pinned pods cannot move: nodeSelector/affinity to this node, local storage, or no controller.
	Pinned bool
	// PinnedReason says why, for the report ("required node affinity", "local storage"...). Optional.
	PinnedReason string
	// ProtectedByPDB: a PodDisruptionBudget covers the pod; draining it needs the budget to allow it.
	ProtectedByPDB bool
	// PDBDisruptionsAllowed is the budget's status.disruptionsAllowed when ProtectedByPDB and known. A pointer to 0 means an
	// eviction would be refused right now, so the node is not a candidate.
	PDBDisruptionsAllowed *int32
	// Optional scheduling facts (see Node).
	NodeSelector map[string]string
	Tolerations  []Toleration
	// VolumeZone is the zone of a zonal persistent volume the pod mounts: it can only move inside that zone.
	VolumeZone string
	// AntiAffinityGroup: pods of one namespace with the same non-empty group refuse to share a node (required pod
	// anti-affinity on the hostname).
	AntiAffinityGroup string
}

// fits reports why the pod may NOT be scheduled on the node, given the pods already (or about to be) there. Empty means it may.
func (p Pod) fits(n Node, there []Pod) string {
	if n.Unschedulable {
		return "is cordoned"
	}
	for _, t := range n.Taints {
		if t.Effect != "NoSchedule" && t.Effect != "NoExecute" {
			continue
		}
		ok := false
		for _, tol := range p.Tolerations {
			if tol.tolerates(t) {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Sprintf("has the taint %s=%s:%s, which the pod does not tolerate", t.Key, t.Value, t.Effect)
		}
	}
	for k, v := range p.NodeSelector {
		if n.Labels[k] != v {
			return fmt.Sprintf("lacks the label %s=%s of the pod's nodeSelector", k, v)
		}
	}
	if p.VolumeZone != "" && n.Zone != p.VolumeZone {
		return fmt.Sprintf("is in zone %q and the pod's volume is in %q", n.Zone, p.VolumeZone)
	}
	if p.AntiAffinityGroup != "" {
		for _, o := range there {
			if o.AntiAffinityGroup == p.AntiAffinityGroup && o.Namespace == p.Namespace {
				return fmt.Sprintf("already runs %s/%s, which the pod's anti-affinity excludes", o.Namespace, o.Name)
			}
		}
	}
	return ""
}

// ClusterSignals are saturation facts the caller may attach: they do not change the analysis, they qualify it.
type ClusterSignals struct {
	// PendingUnschedulable lists pods the scheduler could not place (PodScheduled=False, reason Unschedulable).
	PendingUnschedulable []string `json:",omitempty"`
	// NodesUnderPressure lists "node: condition" for MemoryPressure, DiskPressure and PIDPressure that are True.
	NodesUnderPressure []string `json:",omitempty"`
}

// NodeStat is the per-node summary.
type NodeStat struct {
	Name                       string
	CPURequested, MemRequested float64
	CPUPct, MemPct             float64 // requested / allocatable
	Pods                       int
	Removable                  bool
	Why                        string // why it cannot be removed, or what it needs
	MovedTo                    map[string]string
}

// Report is the result.
type Report struct {
	Nodes []NodeStat
	// Removable lists the nodes whose pods fit elsewhere (greedy, in the order given).
	Removable []string
	// FreedCPU / FreedMem are the allocatable capacity of the removable nodes.
	FreedCPU, FreedMem float64
	// CandidateHourlyUSD is the price of the removable nodes if prices were supplied: an ESTIMATE of a possible reduction, not a saving.
	CandidateHourlyUSD float64
	// StrandedCPU / StrandedMem: free capacity on nodes that cannot be removed (fragmentation).
	StrandedCPU, StrandedMem float64
	Caveats                  []string
	// Signals is filled by the caller (see ClusterSignals). With pending pods or node pressure, removing capacity is the wrong move.
	Signals ClusterSignals
}

// Analyze checks which nodes could be emptied. targetUtil (default 0.85) caps how full the receiving nodes may get on requests.
func Analyze(nodes []Node, pods []Pod, targetUtil float64) (Report, error) {
	if targetUtil <= 0 || targetUtil > 1 {
		targetUtil = 0.85
	}
	var rep Report
	rep.Caveats = []string{
		"based on CPU/memory requests; cordons, taints/tolerations, nodeSelector, zonal volumes, required per-node anti-affinity, PodDisruptionBudgets with no disruption allowed and the last node of a zone are honoured WHEN the caller supplies them; preferred affinities, topology spread constraints and inter-pod affinity are not evaluated",
		"a removable node is a CANDIDATE: no saving exists until the node is actually released by an autoscaler/provider and the bill falls",
	}
	idx := map[string]int{}
	for i, n := range nodes {
		if n.CPUAllocatable <= 0 || n.MemAllocatable <= 0 {
			return rep, fmt.Errorf("node %q has no allocatable capacity data", n.Name)
		}
		if _, dup := idx[n.Name]; dup {
			return rep, fmt.Errorf("duplicate node %q", n.Name)
		}
		idx[n.Name] = i
	}
	type load struct{ cpu, mem float64 }
	used := make([]load, len(nodes))
	byNode := map[string][]Pod{}
	for _, p := range pods {
		i, ok := idx[p.Node]
		if !ok {
			return rep, fmt.Errorf("pod %s/%s is on unknown node %q", p.Namespace, p.Name, p.Node)
		}
		used[i].cpu += p.CPURequest
		used[i].mem += p.MemRequest
		byNode[p.Node] = append(byNode[p.Node], p)
	}
	stats := make([]NodeStat, len(nodes))
	for i, n := range nodes {
		stats[i] = NodeStat{Name: n.Name, CPURequested: used[i].cpu, MemRequested: used[i].mem,
			CPUPct: used[i].cpu / n.CPUAllocatable, MemPct: used[i].mem / n.MemAllocatable, Pods: len(byNode[n.Name])}
	}

	// candidates: least requested first
	order := make([]int, len(nodes))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return stats[order[a]].CPUPct+stats[order[a]].MemPct < stats[order[b]].CPUPct+stats[order[b]].MemPct
	})
	removed := map[int]bool{}
	for _, ci := range order {
		n := nodes[ci]
		var movable []Pod
		blocked := ""
		for _, p := range byNode[n.Name] {
			switch {
			case p.DaemonSet:
			case p.Pinned:
				why := "selector/affinity, local storage or no controller"
				if p.PinnedReason != "" {
					why = p.PinnedReason
				}
				blocked = fmt.Sprintf("pod %s/%s is pinned (%s)", p.Namespace, p.Name, why)
			case p.PDBDisruptionsAllowed != nil && *p.PDBDisruptionsAllowed <= 0:
				blocked = fmt.Sprintf("pod %s/%s is covered by a PodDisruptionBudget that allows no disruption now", p.Namespace, p.Name)
			default:
				movable = append(movable, p)
			}
		}
		if blocked != "" {
			stats[ci].Why = blocked
			continue
		}
		if len(nodes)-len(removed) <= 1 {
			stats[ci].Why = "last remaining node"
			continue
		}
		if n.Zone != "" {
			same, zones := 0, map[string]bool{}
			for j, o := range nodes {
				if removed[j] || o.Zone == "" {
					continue
				}
				zones[o.Zone] = true
				if j != ci && o.Zone == n.Zone {
					same++
				}
			}
			if same == 0 && len(zones) > 1 {
				stats[ci].Why = fmt.Sprintf("last node in zone %q: removing it would reduce zone availability", n.Zone)
				continue
			}
		}
		sort.SliceStable(movable, func(a, b int) bool {
			return movable[a].CPURequest+movable[a].MemRequest/1e9 > movable[b].CPURequest+movable[b].MemRequest/1e9
		})
		// try to place on the other, not-removed nodes using a copy of the loads
		trial := append([]load(nil), used...)
		placed := map[string]string{}
		moves := map[string][]Pod{} // target node -> pods that would land there
		ok := true
		for _, p := range movable {
			done := false
			refused := ""
			for j, o := range nodes {
				if j == ci || removed[j] {
					continue
				}
				if why := p.fits(o, append(append([]Pod(nil), byNode[o.Name]...), moves[o.Name]...)); why != "" {
					refused = fmt.Sprintf("node %s %s", o.Name, why)
					continue
				}
				if trial[j].cpu+p.CPURequest <= o.CPUAllocatable*targetUtil && trial[j].mem+p.MemRequest <= o.MemAllocatable*targetUtil {
					trial[j].cpu += p.CPURequest
					trial[j].mem += p.MemRequest
					placed[p.Namespace+"/"+p.Name] = o.Name
					moves[o.Name] = append(moves[o.Name], p)
					done = true
					break
				}
			}
			if !done {
				ok = false
				stats[ci].Why = fmt.Sprintf("pod %s/%s does not fit on the other nodes below %.0f%% requested", p.Namespace, p.Name, targetUtil*100)
				if refused != "" {
					stats[ci].Why += " (scheduling constraint: " + refused + ")"
				}
				break
			}
		}
		if !ok {
			continue
		}
		var pdb int
		for _, p := range movable {
			if p.ProtectedByPDB {
				pdb++
			}
		}
		used = trial
		// later candidates must see the pods that were moved onto their node
		for target, ps := range moves {
			byNode[target] = append(byNode[target], ps...)
		}
		delete(byNode, n.Name)
		removed[ci] = true
		stats[ci].Removable, stats[ci].MovedTo = true, placed
		stats[ci].Why = "its pods fit elsewhere"
		if pdb > 0 {
			stats[ci].Why += fmt.Sprintf("; %d pod(s) are covered by a PodDisruptionBudget: draining must respect it", pdb)
		}
		rep.Removable = append(rep.Removable, n.Name)
		rep.FreedCPU += n.CPUAllocatable
		rep.FreedMem += n.MemAllocatable
		rep.CandidateHourlyUSD += n.HourlyUSD
	}
	for i, n := range nodes {
		if !removed[i] {
			rep.StrandedCPU += n.CPUAllocatable - used[i].cpu
			rep.StrandedMem += n.MemAllocatable - used[i].mem
		}
	}
	rep.Nodes = stats
	return rep, nil
}
