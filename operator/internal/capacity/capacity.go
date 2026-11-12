// Package capacity answers one question the recommender cannot: if requests shrink, does any node actually become removable?
//
// Lowering a container's requests is Resource Efficiency. Fitting the same pods on fewer nodes is Capacity Optimization.
// Only releasing a node is Infrastructure Cost Reduction. This package computes the second and states the third as a
// candidate, never as a saving.
//
// The simulation is a first-fit-decreasing check on CPU and memory REQUESTS. It deliberately ignores what it cannot see
// (pod affinity/anti-affinity, topology spread, taints beyond a simple flag, volumes' zones) and says so in the report.
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
}

// Pod is a scheduled pod.
type Pod struct {
	Namespace, Name, Node  string
	CPURequest, MemRequest float64
	// DaemonSet pods run on every node and are ignored when judging whether a node can be emptied.
	DaemonSet bool
	// Pinned pods cannot move: nodeSelector/affinity to this node, local storage, or no controller.
	Pinned bool
	// ProtectedByPDB: a PodDisruptionBudget covers the pod; draining it needs the budget to allow it.
	ProtectedByPDB bool
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
}

// Analyze checks which nodes could be emptied. targetUtil (default 0.85) caps how full the receiving nodes may get on requests.
func Analyze(nodes []Node, pods []Pod, targetUtil float64) (Report, error) {
	if targetUtil <= 0 || targetUtil > 1 {
		targetUtil = 0.85
	}
	var rep Report
	rep.Caveats = []string{
		"based on CPU/memory requests only; pod (anti-)affinity, topology spread, zones of volumes and taints are not evaluated",
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
				blocked = fmt.Sprintf("pod %s/%s is pinned (selector/affinity, local storage or no controller)", p.Namespace, p.Name)
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
			for j, o := range nodes {
				if j == ci || removed[j] {
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
