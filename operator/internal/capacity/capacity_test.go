package capacity

import (
	"strings"
	"testing"
)

const gi = 1 << 30

func n(name string) Node {
	return Node{Name: name, CPUAllocatable: 4, MemAllocatable: 16 * gi, HourlyUSD: 0.2}
}

func p(name, node string, cpu, memGi float64) Pod {
	return Pod{Namespace: "shop", Name: name, Node: node, CPURequest: cpu, MemRequest: memGi * gi}
}

func TestEmptiableNodeIsACandidateNotASaving(t *testing.T) {
	// n1 carries 1 CPU, n2 and n3 are at 1 CPU each: n1's pod fits on n2
	r, err := Analyze([]Node{n("n1"), n("n2"), n("n3")}, []Pod{p("a", "n1", 1, 2), p("b", "n2", 1, 2), p("c", "n3", 1, 2)}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Removable) == 0 || r.CandidateHourlyUSD <= 0 || r.FreedCPU <= 0 {
		t.Fatalf("%+v", r)
	}
	if !strings.Contains(strings.Join(r.Caveats, " "), "CANDIDATE") {
		t.Fatal("the report must say a candidate is not a saving")
	}
}

func TestPlacementsNeverExceedTheTargetUtilisation(t *testing.T) {
	// two nodes at 60% requested each: neither can absorb the other at 85%
	r, _ := Analyze([]Node{n("n1"), n("n2")}, []Pod{p("a", "n1", 2.4, 8), p("b", "n2", 2.4, 8)}, 0.85)
	if len(r.Removable) != 0 {
		t.Fatalf("%+v", r.Removable)
	}
	for _, s := range r.Nodes {
		if !strings.Contains(s.Why, "does not fit") {
			t.Errorf("%s: %q", s.Name, s.Why)
		}
	}
	if r.StrandedCPU <= 0 {
		t.Fatal("fragmentation not reported")
	}
}

func TestPinnedAndDaemonSetPods(t *testing.T) {
	pinned := p("db", "n1", 0.5, 1)
	pinned.Pinned = true
	r, _ := Analyze([]Node{n("n1"), n("n2")}, []Pod{pinned, p("b", "n2", 0.5, 1)}, 0)
	// n1 cannot be emptied (pinned) but n2 can be moved to n1
	if len(r.Removable) != 1 || r.Removable[0] != "n2" {
		t.Fatalf("%+v", r.Removable)
	}
	ds := p("agent", "n1", 0.2, 0.2)
	ds.DaemonSet = true
	r, _ = Analyze([]Node{n("n1"), n("n2")}, []Pod{ds, {Namespace: "s", Name: "agent2", Node: "n2", CPURequest: 0.2, DaemonSet: true}}, 0)
	if len(r.Removable) != 1 {
		t.Fatalf("an idle node with only DaemonSet pods must be removable: %+v", r)
	}
}

func TestLastNodeIsNeverRemovable(t *testing.T) {
	r, _ := Analyze([]Node{n("n1")}, []Pod{p("a", "n1", 0.1, 0.1)}, 0)
	if len(r.Removable) != 0 || r.Nodes[0].Why != "last remaining node" {
		t.Fatalf("%+v", r)
	}
}

func TestPDBIsFlaggedOnRemoval(t *testing.T) {
	a := p("a", "n1", 0.5, 1)
	a.ProtectedByPDB = true
	r, _ := Analyze([]Node{n("n1"), n("n2")}, []Pod{a, p("b", "n2", 1, 2)}, 0)
	var why string
	for _, s := range r.Nodes {
		if s.Name == "n1" {
			why = s.Why
		}
	}
	if !strings.Contains(why, "PodDisruptionBudget") {
		t.Fatal(why)
	}
}

func TestSequentialRemovalAccountsForEarlierMoves(t *testing.T) {
	// three light nodes: removing two must not overfill the third
	pods := []Pod{p("a", "n1", 1.5, 4), p("b", "n2", 1.5, 4), p("c", "n3", 1.5, 4)}
	r, _ := Analyze([]Node{n("n1"), n("n2"), n("n3")}, pods, 0.85)
	if len(r.Removable) != 1 {
		t.Fatalf("only one node can go (3.4 CPU cap): %+v", r.Removable)
	}
}

func TestBadInputIsRejected(t *testing.T) {
	if _, err := Analyze([]Node{{Name: "x"}}, nil, 0); err == nil {
		t.Error("node without capacity accepted")
	}
	if _, err := Analyze([]Node{n("a"), n("a")}, nil, 0); err == nil {
		t.Error("duplicate node accepted")
	}
	if _, err := Analyze([]Node{n("a")}, []Pod{p("x", "ghost", 1, 1)}, 0); err == nil {
		t.Error("pod on unknown node accepted")
	}
}
