package cost

// All figures here are SYNTHETIC fixtures chosen so the arithmetic can be checked by hand. They are not real prices or bills.

import (
	"strings"
	"testing"
	"time"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/money"
)

var (
	t0 = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	t1 = t0.Add(24 * time.Hour)
)

func usd(s string) money.USD {
	v, err := money.Parse(s)
	if err != nil {
		panic(err)
	}
	return v
}

func nodes() []Node {
	return []Node{{Name: "n1", CPUAllocatable: 4, MemAllocatable: 16 << 30}, {Name: "n2", CPUAllocatable: 4, MemAllocatable: 16 << 30}}
}

func pods() []Pod {
	return []Pod{
		{Namespace: "shop", Node: "n1", CPURequest: 2, MemRequest: 8 << 30},        // half of n1 on both axes
		{Namespace: "kube-system", Node: "n1", CPURequest: 1, MemRequest: 4 << 30}, // a quarter
		{Namespace: "shop", Node: "n2", CPURequest: 1, MemRequest: 4 << 30},        // a quarter of n2
	}
}

func TestAllocationSplitsByRequestShareAndAlwaysSumsToTheInput(t *testing.T) {
	r, err := Allocate(BasisBilled, "synthetic", t0, t1, nodes(), pods(), map[string]money.USD{"n1": usd("100"), "n2": usd("100")}, nil, Config{})
	if err != nil {
		t.Fatal(err)
	}
	// n1: shop 50, kube-system 25, idle 25. n2: shop 25, idle 75.
	want := map[string]string{"shop": "75.000000", "kube-system": "25.000000", Idle: "100.000000"}
	for ns, w := range want {
		if got := r.ByNamespace[ns].String(); !strings.HasPrefix(got, strings.TrimRight(w, "0")[:3]) && got != w {
			t.Errorf("%s: %s want %s", ns, got, w)
		}
	}
	if r.Total != usd("200") || r.Accounted != r.Total || r.Difference != 0 || !r.Reconciled {
		t.Fatalf("%+v", r)
	}
	if r.ByNamespace["shop"] != usd("75") || r.ByNamespace["kube-system"] != usd("25") || r.ByNamespace[Idle] != usd("100") {
		t.Fatalf("%+v", r.ByNamespace)
	}
}

func TestSharedNamespacesAreRedistributedWithoutChangingTheTotal(t *testing.T) {
	r, _ := Allocate(BasisBilled, "synthetic", t0, t1, nodes(), pods(), map[string]money.USD{"n1": usd("100"), "n2": usd("100")}, nil, Config{SharedNamespaces: []string{"kube-system"}})
	if _, ok := r.ByNamespace["kube-system"]; ok {
		t.Fatal("shared namespace must not remain")
	}
	if r.Shared != usd("25") || r.ByNamespace["shop"] != usd("100") || r.Accounted != r.Total || !r.Reconciled {
		t.Fatalf("%+v", r)
	}
}

func TestSharedCostWithNobodyToTakeItIsKeptApart(t *testing.T) {
	p := []Pod{{Namespace: "kube-system", Node: "n1", CPURequest: 1, MemRequest: 4 << 30}}
	r, _ := Allocate(BasisBilled, "synthetic", t0, t1, nodes(), p, map[string]money.USD{"n1": usd("100")}, nil, Config{SharedNamespaces: []string{"kube-system"}})
	if r.ByNamespace["__shared__"] != usd("25") || !r.Reconciled {
		t.Fatalf("%+v", r)
	}
}

func TestUnknownNodesAndExtraItemsAreUnallocatedNotHidden(t *testing.T) {
	extra := []BillingItem{{Resource: "lb-1", Cost: usd("7")}}
	r, err := Allocate(BasisBilled, "synthetic", t0, t1, nodes(), pods(), map[string]money.USD{"n1": usd("100"), "ghost": usd("40")}, extra, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if r.ByNamespace[Unallocated] != usd("47") || r.Total != usd("147") || !r.Reconciled || len(r.Notes) == 0 {
		t.Fatalf("%+v", r)
	}
}

func TestRequestsAboveCapacityNeverOverAllocate(t *testing.T) {
	p := []Pod{{Namespace: "a", Node: "n1", CPURequest: 8, MemRequest: 32 << 30}}
	r, _ := Allocate(BasisEstimated, "price-flag", t0, t1, nodes(), p, map[string]money.USD{"n1": usd("10")}, nil, Config{})
	if r.Accounted != r.Total || r.ByNamespace["a"] > usd("10") {
		t.Fatalf("%+v", r)
	}
}

func TestInvalidInputs(t *testing.T) {
	if _, err := Allocate("guess", "x", t0, t1, nil, nil, nil, nil, Config{}); err == nil {
		t.Error("unknown basis accepted")
	}
	if _, err := Allocate(BasisBilled, "x", t1, t0, nil, nil, nil, nil, Config{}); err == nil {
		t.Error("inverted window accepted")
	}
	if _, err := Allocate(BasisBilled, "x", t0, t1, nodes(), nil, map[string]money.USD{"n1": -1}, nil, Config{}); err == nil {
		t.Error("negative node cost accepted")
	}
}

func TestEstimatedNodeCost(t *testing.T) {
	if got := EstimateNodeCost(usd("0.5"), t0, t0.Add(36*time.Hour)); got != usd("18") {
		t.Fatal(got)
	}
	if EstimateNodeCost(usd("1"), t1, t0) != 0 {
		t.Fatal("negative window")
	}
}

const csvOK = "resource,cost_usd,start,end\nn1,48.00,2026-11-01T00:00:00Z,2026-11-03T00:00:00Z\nlb-1,5,2026-11-01T00:00:00Z,2026-11-02T00:00:00Z\nold,9,2026-10-01T00:00:00Z,2026-10-02T00:00:00Z\n"

func TestBillingFileIsProratedToTheWindow(t *testing.T) {
	items, err := ParseBilling(strings.NewReader(csvOK), t0, t1)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Cost != usd("24") || items[1].Cost != usd("5") {
		t.Fatalf("%+v", items)
	}
	nc, extra := SplitByNode(items, nodes())
	if nc["n1"] != usd("24") || len(extra) != 1 || extra[0].Resource != "lb-1" {
		t.Fatalf("%v %v", nc, extra)
	}
}

func TestBillingFileRejectsBadData(t *testing.T) {
	for name, in := range map[string]string{
		"empty":     "",
		"no column": "resource,cost\nn1,1\n",
		"bad cost":  "resource,cost_usd,start,end\nn1,abc,2026-11-01T00:00:00Z,2026-11-02T00:00:00Z\n",
		"negative":  "resource,cost_usd,start,end\nn1,-5,2026-11-01T00:00:00Z,2026-11-02T00:00:00Z\n",
		"bad time":  "resource,cost_usd,start,end\nn1,1,yesterday,2026-11-02T00:00:00Z\n",
		"inverted":  "resource,cost_usd,start,end\nn1,1,2026-11-02T00:00:00Z,2026-11-01T00:00:00Z\n",
	} {
		if _, err := ParseBilling(strings.NewReader(in), t0, t1); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func rep(basis Basis, ns string, cost string, hours int) Report {
	return Report{Basis: basis, Start: t0, End: t0.Add(time.Duration(hours) * time.Hour), ByNamespace: map[string]money.USD{ns: usd(cost)}}
}

func TestObservedSavingsNormalisesWindowsAndKeepsKindsApart(t *testing.T) {
	s, err := ObservedSavings(rep(BasisEstimated, "shop", "48", 24), rep(BasisEstimated, "shop", "12", 12), "shop", usd("3"), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	// before 2 USD/h, after 1 USD/h
	if s.Observed == nil || *s.Observed != usd("1") || s.Billed != nil || s.Potential != usd("3") {
		t.Fatalf("%+v", s)
	}
	s, _ = ObservedSavings(rep(BasisBilled, "shop", "48", 24), rep(BasisBilled, "shop", "12", 12), "shop", 0, 0, 0)
	if s.Billed == nil || *s.Billed != usd("1") {
		t.Fatalf("%+v", s)
	}
	s, _ = ObservedSavings(rep(BasisBilled, "shop", "48", 24), rep(BasisEstimated, "shop", "12", 12), "shop", 0, 0, 0)
	if s.Observed != nil || s.Billed != nil {
		t.Fatalf("mixed bases must not compare: %+v", s)
	}
}

func TestTrafficDropIsFlagged(t *testing.T) {
	s, _ := ObservedSavings(rep(BasisEstimated, "shop", "24", 24), rep(BasisEstimated, "shop", "12", 24), "shop", 0, 24000, 6000)
	if !strings.Contains(strings.Join(s.Notes, " "), "traffic fell") {
		t.Fatalf("%v", s.Notes)
	}
	if _, err := ObservedSavings(Report{}, Report{}, "x", 0, 0, 0); err == nil {
		t.Fatal("empty windows accepted")
	}
}
