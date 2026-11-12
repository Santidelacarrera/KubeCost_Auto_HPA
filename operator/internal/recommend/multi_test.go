package recommend

import (
	"encoding/json"
	"strings"
	"testing"
)

func one(container, id string, conf Confidence, abst bool) Recommendation {
	return Recommendation{ID: id, Cluster: "c", Namespace: "shop", Kind: "Deployment", Workload: "api", Container: container,
		Confidence: conf, Abstained: abst, Risks: []string{"r-" + container},
		CPU: ResourceResult{Action: Decrease, Current: 1, Recommended: 0.5, PotentialMonthlyUSD: 2}}
}

func TestCombineKeepsThePrimaryAndTheWeakestConfidence(t *testing.T) {
	r, err := Combine(one("app", "a", ConfHigh, false), one("proxy", "b", ConfLow, true))
	if err != nil {
		t.Fatal(err)
	}
	if r.Container != "app" || len(r.Additional) != 1 || r.Additional[0].Container != "proxy" || r.Confidence != ConfLow || r.Abstained {
		t.Fatalf("%+v", r)
	}
	if r.ID == "a" || r.ID == "" {
		t.Fatal("a combined record needs its own ID")
	}
	if c, _ := r.PotentialMonthlyUSD(); c != 4 {
		t.Fatalf("estimates sum over containers: %v", c)
	}
	if !strings.Contains(strings.Join(r.Risks, " "), "container proxy: r-proxy") {
		t.Fatalf("%v", r.Risks)
	}
	v := r.Views()
	if len(v) != 2 || v[1].Container != "proxy" || v[1].ID != r.ID || !v[1].Abstained || len(v[0].Additional) != 0 {
		t.Fatalf("%+v", v)
	}
	all, _ := Combine(one("app", "a", ConfHigh, true), one("proxy", "b", ConfHigh, true))
	if !all.Abstained {
		t.Fatal("abstains when every container abstains")
	}
	for name, bad := range map[string][]Recommendation{
		"other workload": {one("app", "a", ConfHigh, false), func() Recommendation { x := one("p", "b", ConfHigh, false); x.Workload = "web"; return x }()},
		"duplicate":      {one("app", "a", ConfHigh, false), one("app", "b", ConfHigh, false)},
		"none":           nil,
	} {
		if _, err := Combine(bad...); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// Records written before multi-container support have no Additional field and must read as single-container records.
func TestSingleContainerRecordsAreUnchangedOnTheWire(t *testing.T) {
	r, _ := Combine(one("app", "a", ConfHigh, false))
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "Additional") {
		t.Fatal("a single-container record must not carry an Additional field")
	}
	var back Recommendation
	if err := json.Unmarshal([]byte(`{"ID":"x","Container":"app"}`), &back); err != nil || len(back.Views()) != 1 {
		t.Fatalf("%v %+v", err, back)
	}
}
