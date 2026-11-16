package capacity

import (
	"strings"
	"testing"
)

// two light nodes plus one receiver: without constraints n1 is removable (the baseline every case below changes one fact of).
func base() ([]Node, []Pod) {
	return []Node{n("n1"), n("n2")}, []Pod{p("a", "n1", 0.5, 1), p("b", "n2", 1, 2)}
}

func why(t *testing.T, r Report, node string) string {
	t.Helper()
	for _, s := range r.Nodes {
		if s.Name == node {
			return s.Why
		}
	}
	t.Fatalf("node %s not in the report", node)
	return ""
}

func TestBaselineWithoutConstraintsIsRemovable(t *testing.T) {
	nodes, pods := base()
	r, err := Analyze(nodes, pods, 0)
	if err != nil || len(r.Removable) != 1 || r.Removable[0] != "n1" {
		t.Fatalf("%v %+v", err, r.Removable)
	}
}

func TestTaintWithoutTolerationBlocksTheMove(t *testing.T) {
	nodes, pods := base()
	nodes[1].Taints = []Taint{{Key: "dedicated", Value: "db", Effect: "NoSchedule"}}
	r, _ := Analyze(nodes, pods, 0)
	// n1's pod cannot go to the tainted n2; n2's pod (already there) can go to n1, so n2 becomes the candidate instead.
	for _, name := range r.Removable {
		if name == "n1" {
			t.Fatalf("n1 must not be removable: its pod does not tolerate n2's taint: %+v", r.Nodes)
		}
	}
	if !strings.Contains(why(t, r, "n1"), "taint dedicated=db:NoSchedule") {
		t.Fatalf("the reason must name the taint: %q", why(t, r, "n1"))
	}
}

func TestTolerationAllowsTheMove(t *testing.T) {
	for _, tol := range []Toleration{
		{Key: "dedicated", Operator: "Equal", Value: "db", Effect: "NoSchedule"},
		{Key: "dedicated", Operator: "Exists"},
		{Operator: "Exists"},
	} {
		nodes, pods := base()
		nodes[1].Taints = []Taint{{Key: "dedicated", Value: "db", Effect: "NoSchedule"}}
		pods[0].Tolerations = []Toleration{tol}
		r, _ := Analyze(nodes, pods, 0)
		if len(r.Removable) != 1 || r.Removable[0] != "n1" {
			t.Fatalf("toleration %+v must allow the move: %+v", tol, r.Nodes)
		}
	}
	// a toleration for another value or effect does not
	for _, tol := range []Toleration{
		{Key: "dedicated", Operator: "Equal", Value: "cache", Effect: "NoSchedule"},
		{Key: "dedicated", Operator: "Exists", Effect: "NoExecute"},
	} {
		nodes, pods := base()
		nodes[1].Taints = []Taint{{Key: "dedicated", Value: "db", Effect: "NoSchedule"}}
		pods[0].Tolerations = []Toleration{tol}
		r, _ := Analyze(nodes, pods, 0)
		for _, name := range r.Removable {
			if name == "n1" {
				t.Fatalf("toleration %+v must not allow the move", tol)
			}
		}
	}
}

func TestPreferNoScheduleIsOnlyAPreference(t *testing.T) {
	nodes, pods := base()
	nodes[1].Taints = []Taint{{Key: "x", Effect: "PreferNoSchedule"}}
	if r, _ := Analyze(nodes, pods, 0); len(r.Removable) != 1 || r.Removable[0] != "n1" {
		t.Fatalf("%+v", r.Nodes)
	}
}

func TestCordonedNodeReceivesNothing(t *testing.T) {
	nodes, pods := base()
	nodes[1].Unschedulable = true
	r, _ := Analyze(nodes, pods, 0)
	if !strings.Contains(why(t, r, "n1"), "cordoned") {
		t.Fatalf("%q", why(t, r, "n1"))
	}
}

func TestNodeSelectorMustMatchTheTarget(t *testing.T) {
	nodes, pods := base()
	nodes[0].Labels = map[string]string{"pool": "gpu"}
	nodes[1].Labels = map[string]string{"pool": "general"}
	pods[0].NodeSelector = map[string]string{"pool": "gpu"}
	r, _ := Analyze(nodes, pods, 0)
	if !strings.Contains(why(t, r, "n1"), "pool=gpu") {
		t.Fatalf("%q", why(t, r, "n1"))
	}
	nodes[1].Labels["pool"] = "gpu"
	if r, _ := Analyze(nodes, pods, 0); len(r.Removable) != 1 {
		t.Fatalf("with a matching label the move is allowed: %+v", r.Nodes)
	}
}

func TestZonalVolumeKeepsThePodInItsZone(t *testing.T) {
	nodes := []Node{n("a1"), n("a2"), n("b1")}
	nodes[0].Zone, nodes[1].Zone, nodes[2].Zone = "a", "a", "b"
	pods := []Pod{p("db", "a1", 0.5, 1), p("x", "a2", 3.3, 2), p("y", "b1", 0.5, 1)}
	pods[0].VolumeZone = "a"
	// a2 is too full (3.3 + 0.5 > 3.4) and b1 is in another zone: a1 must stay.
	r, _ := Analyze(nodes, pods, 0.85)
	for _, name := range r.Removable {
		if name == "a1" {
			t.Fatalf("a1's pod has a volume in zone a and only b1 has room: %+v", r.Nodes)
		}
	}
	if !strings.Contains(why(t, r, "a1"), `volume is in "a"`) {
		t.Fatalf("%q", why(t, r, "a1"))
	}
}

func TestAntiAffinityCountsPodsAlreadyMovedInThisRun(t *testing.T) {
	// three replicas of one anti-affine group on three nodes: no node can be emptied, wherever the replica would land there is a sibling.
	nodes := []Node{n("n1"), n("n2"), n("n3")}
	pods := []Pod{p("r1", "n1", 0.2, 1), p("r2", "n2", 0.2, 1), p("r3", "n3", 0.2, 1)}
	for i := range pods {
		pods[i].AntiAffinityGroup = "app=web"
	}
	r, _ := Analyze(nodes, pods, 0)
	if len(r.Removable) != 0 {
		t.Fatalf("anti-affinity must keep one replica per node: %+v", r.Removable)
	}
	if !strings.Contains(why(t, r, "n1"), "anti-affinity") {
		t.Fatalf("%q", why(t, r, "n1"))
	}
	// the same group in another namespace is a different group
	pods[1].Namespace, pods[2].Namespace = "other", "other2"
	if r, _ := Analyze(nodes, pods, 0); len(r.Removable) == 0 {
		t.Fatalf("groups are per namespace: %+v", r.Nodes)
	}
}

func TestPDBWithNoDisruptionAllowedBlocksTheNode(t *testing.T) {
	nodes, pods := base()
	zero, one := int32(0), int32(1)
	pods[0].ProtectedByPDB, pods[0].PDBDisruptionsAllowed = true, &zero
	pods[1].ProtectedByPDB, pods[1].PDBDisruptionsAllowed = true, &zero
	r, _ := Analyze(nodes, pods, 0)
	if len(r.Removable) != 0 || !strings.Contains(why(t, r, "n1"), "allows no disruption") {
		t.Fatalf("%+v", r.Nodes)
	}
	pods[0].PDBDisruptionsAllowed = &one
	r, _ = Analyze(nodes, pods, 0)
	if len(r.Removable) != 1 || !strings.Contains(why(t, r, "n1"), "PodDisruptionBudget") {
		t.Fatalf("a budget with room allows the candidate and is still flagged: %+v", r.Nodes)
	}
}

func TestLastNodeOfAZoneIsKept(t *testing.T) {
	nodes := []Node{n("a1"), n("b1"), n("b2")}
	nodes[0].Zone, nodes[1].Zone, nodes[2].Zone = "a", "b", "b"
	pods := []Pod{p("x", "a1", 0.1, 1), p("y", "b1", 1, 2), p("z", "b2", 1.2, 2)}
	r, _ := Analyze(nodes, pods, 0)
	for _, name := range r.Removable {
		if name == "a1" {
			t.Fatalf("a1 is the only node in zone a: %+v", r.Nodes)
		}
	}
	if !strings.Contains(why(t, r, "a1"), "zone availability") {
		t.Fatalf("%q", why(t, r, "a1"))
	}
	// with a single zone in the cluster there is no zone to lose
	for i := range nodes {
		nodes[i].Zone = "a"
	}
	if r, _ := Analyze(nodes, pods, 0); len(r.Removable) == 0 {
		t.Fatalf("%+v", r.Nodes)
	}
}

func TestPinnedReasonIsReported(t *testing.T) {
	nodes, pods := base()
	pods[0].Pinned, pods[0].PinnedReason = true, "required node affinity"
	r, _ := Analyze(nodes, pods, 0)
	if !strings.Contains(why(t, r, "n1"), "required node affinity") {
		t.Fatalf("%q", why(t, r, "n1"))
	}
}

func TestPlanIsOrderedManualAndReversible(t *testing.T) {
	nodes := []Node{n("n1"), n("n2"), n("n3")}
	pods := []Pod{p("a", "n1", 1, 2), p("b", "n2", 1, 2), p("c", "n3", 1, 2)}
	pods[0].ProtectedByPDB = true
	rep, _ := Analyze(nodes, pods, 0)
	plan := Plan(rep, nodes, pods)
	if len(plan.Blocked) != 0 || len(plan.Steps) != len(rep.Removable) || len(plan.Steps) == 0 {
		t.Fatalf("%+v", plan)
	}
	for i, st := range plan.Steps {
		if st.Node != rep.Removable[i] {
			t.Fatalf("the plan must keep the order of the analysis: %v vs %v", st.Node, rep.Removable)
		}
		if len(st.Commands) != 2 || !strings.HasPrefix(st.Commands[0], "kubectl cordon ") || !strings.Contains(st.Commands[1], "--delete-emptydir-data=false") {
			t.Fatalf("commands: %v", st.Commands)
		}
		if len(st.Undo) == 0 || len(st.Verify) == 0 || len(st.Moves) == 0 || st.CandidateHourlyUSD != 0.2 {
			t.Fatalf("%+v", st)
		}
	}
	if !strings.Contains(strings.Join(plan.Notes, " "), "nothing in this product executes it") {
		t.Fatal("the plan must say it is not executed")
	}
	// the PDB of pod a is carried into the step of its node, wherever that node is in the plan
	for _, st := range plan.Steps {
		if st.Node == "n1" && (st.PDBProtected != 1 || !strings.Contains(strings.Join(st.Preconditions, " "), "PodDisruptionBudget")) {
			t.Fatalf("%+v", st)
		}
	}
}

func TestPlanRefusesOnAClusterShortOfCapacity(t *testing.T) {
	nodes := []Node{n("n1"), n("n2"), n("n3")}
	pods := []Pod{p("a", "n1", 1, 2), p("b", "n2", 1, 2), p("c", "n3", 1, 2)}
	rep, _ := Analyze(nodes, pods, 0)
	rep.Signals.PendingUnschedulable = []string{"shop/x"}
	if plan := Plan(rep, nodes, pods); len(plan.Steps) != 0 || !strings.Contains(strings.Join(plan.Blocked, " "), "pending") {
		t.Fatalf("%+v", plan)
	}
	rep.Signals = ClusterSignals{NodesUnderPressure: []string{"n2: MemoryPressure"}}
	if plan := Plan(rep, nodes, pods); len(plan.Steps) != 0 || !strings.Contains(strings.Join(plan.Blocked, " "), "pressure") {
		t.Fatalf("%+v", plan)
	}
	// nothing removable: an empty plan with the reason
	one := []Node{n("only")}
	rep, _ = Analyze(one, nil, 0)
	if plan := Plan(rep, one, nil); len(plan.Steps) != 0 || len(plan.Blocked) == 0 {
		t.Fatalf("%+v", plan)
	}
}
