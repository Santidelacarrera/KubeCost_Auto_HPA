package capacity

import (
	"fmt"
	"sort"
)

// DrainStep is one node of a consolidation plan: what a person would run, in order, and how to undo it.
type DrainStep struct {
	Node string
	// Moves is where the simulation placed each pod (pod -> receiving node). The scheduler decides for real.
	Moves map[string]string
	// PDBProtected counts the node's pods covered by a PodDisruptionBudget: the drain will wait for the budget.
	PDBProtected  int
	Preconditions []string
	Commands      []string
	Verify        []string
	Undo          []string
	// CandidateHourlyUSD is the supplied price of this node: an estimate of what releasing it could save.
	CandidateHourlyUSD float64
}

// ConsolidationPlan is a PROPOSAL for a human. Nothing in this package or in the CLI executes it.
type ConsolidationPlan struct {
	Steps []DrainStep
	// Blocked says why the plan is empty or must not be followed now.
	Blocked []string
	Notes   []string
}

// Plan turns an analysis into an ordered, manual drain plan: one node at a time, each with its preconditions, the commands, the
// check before going on, and the way back. It refuses (Blocked) while pods are pending as unschedulable or nodes report
// pressure: removing capacity from a cluster that is short of it is the wrong move, whatever the requests say.
func Plan(rep Report, nodes []Node, pods []Pod) ConsolidationPlan {
	p := ConsolidationPlan{Notes: []string{
		"this is a proposal: nothing here was executed, and nothing in this product executes it",
		"one node at a time: finish the verification of a step before starting the next, and re-run `rightsize nodes` between steps, because every drain changes where pods are",
		"the placement is a simulation on requests; the scheduler decides, and preferred affinities and topology spread constraints were not evaluated",
		"a drained node is released by the cluster autoscaler (GKE, AKS) or by you; until it is gone and the bill falls, no saving exists",
	}}
	if n := len(rep.Signals.PendingUnschedulable); n > 0 {
		p.Blocked = append(p.Blocked, fmt.Sprintf("%d pod(s) are pending as unschedulable: the cluster is short of capacity", n))
	}
	if n := len(rep.Signals.NodesUnderPressure); n > 0 {
		p.Blocked = append(p.Blocked, fmt.Sprintf("%d node condition(s) report pressure", n))
	}
	if len(rep.Removable) == 0 {
		p.Blocked = append(p.Blocked, "no node is a candidate: see the reason on each node of the analysis")
	}
	if len(p.Blocked) > 0 {
		return p
	}
	price := map[string]float64{}
	for _, n := range nodes {
		price[n.Name] = n.HourlyUSD
	}
	pdb := map[string]int{}
	for _, pod := range pods {
		if pod.ProtectedByPDB && !pod.DaemonSet {
			pdb[pod.Node]++
		}
	}
	moved := map[string]map[string]string{}
	for _, st := range rep.Nodes {
		if st.Removable {
			moved[st.Name] = st.MovedTo
		}
	}
	for _, name := range rep.Removable { // the order of the analysis: least requested first
		receivers := map[string]bool{}
		for _, to := range moved[name] {
			receivers[to] = true
		}
		var rs []string
		for r := range receivers {
			rs = append(rs, r)
		}
		sort.Strings(rs)
		step := DrainStep{Node: name, Moves: moved[name], PDBProtected: pdb[name], CandidateHourlyUSD: price[name],
			Preconditions: []string{
				"a maintenance window, and someone watching the workloads on this node",
				fmt.Sprintf("the receiving node(s) %v are Ready and not cordoned", rs),
				"no rollout is in progress for the workloads on this node",
			},
			Commands: []string{
				"kubectl cordon " + name,
				"kubectl drain " + name + " --ignore-daemonsets --delete-emptydir-data=false --timeout=10m",
			},
			Verify: []string{
				"kubectl get pods -A --field-selector=status.phase=Pending   # nothing may be left pending",
				"kubectl get pods -A -o wide --field-selector spec.nodeName=" + name + "   # only DaemonSet and static pods remain",
				"your latency and error dashboards for the moved workloads, for at least one scrape interval after they are Ready",
			},
			Undo: []string{
				"kubectl uncordon " + name + "   # at any point before the node is deleted; pods do not move back by themselves",
			},
		}
		if step.PDBProtected > 0 {
			step.Preconditions = append(step.Preconditions, fmt.Sprintf("%d pod(s) are covered by a PodDisruptionBudget: the drain waits for the budget and fails at the timeout instead of breaking it", step.PDBProtected))
		}
		p.Steps = append(p.Steps, step)
	}
	return p
}
