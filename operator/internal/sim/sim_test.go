package sim

import (
	"math"
	"reflect"
	"testing"
	"time"
)

func find(t *testing.T, name string) Scenario {
	t.Helper()
	for _, s := range Scenarios() {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no scenario %q", name)
	return Scenario{}
}

func scaler(t *testing.T, w Workload, label string) Scaler {
	t.Helper()
	for _, s := range Scalers(w) {
		if s.Name() == label {
			return s
		}
	}
	t.Fatalf("no scaler %q", label)
	return nil
}

func TestRunIsDeterministic(t *testing.T) {
	w := DefaultWorkload()
	for _, sc := range Scenarios() {
		for _, s := range Scalers(w) {
			a := Run(w, sc, s)
			var again Scaler
			for _, c := range Scalers(w) {
				if c.Name() == s.Name() {
					again = c
				}
			}
			if b := Run(w, sc, again); !reflect.DeepEqual(a, b) {
				t.Fatalf("%s/%s not deterministic:\n%+v\n%+v", sc.Name, s.Name(), a, b)
			}
		}
	}
}

func TestEveryScalerRespectsReplicaBounds(t *testing.T) {
	w := DefaultWorkload()
	for _, sc := range Scenarios() {
		for _, s := range Scalers(w) {
			if r := Run(w, sc, s); r.MaxReplicas > w.MaxReplicas || r.AvgReplicas < float64(w.MinReplicas)-1e-9 {
				t.Errorf("%s/%s: max %d avg %.2f outside [%d,%d]", sc.Name, s.Name(), r.MaxReplicas, r.AvgReplicas, w.MinReplicas, w.MaxReplicas)
			}
		}
	}
}

// With constant demand a controller must converge and stay put. Cost data that
// depends on the replica count (as real spend does) must not create a limit
// cycle between "cost pressure on" and "cost pressure off".
func TestConstantDemandNeverReversesDirection(t *testing.T) {
	w := DefaultWorkload()
	sc := find(t, "steady")
	for _, s := range Scalers(w) {
		if r := Run(w, sc, s); r.Reversals != 0 {
			t.Errorf("%s: %d scale reversals under constant demand (%d actions)", s.Name(), r.Reversals, r.ScaleActions)
		}
	}
}

// The native HPA arithmetic of the baseline: ceil(ready*util/target), capped at
// +max(4 pods, 100%) per period.
func TestHPABaselineArithmetic(t *testing.T) {
	h := &HPA{Label: "h", Target: 0.7, Tolerance: 0.1, DownWindow: 5 * time.Minute, Min: 2, Max: 100}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := h.Decide(now, 10, 10, 1.4, 0, true, time.Time{}); got != 20 {
		t.Fatalf("10 pods at 140%% of request, target 70%%: want 20, got %d", got)
	}
	if got := h.Decide(now.Add(15*time.Second), 3, 3, 3.0, 0, true, time.Time{}); got != 7 {
		t.Fatalf("scale-up must be limited to +max(4, 100%%)=+4: want 7, got %d", got)
	}
	if got := h.Decide(now.Add(30*time.Second), 10, 10, 0.72, 0, true, time.Time{}); got != 10 {
		t.Fatalf("within 10%% tolerance must hold, got %d", got)
	}
	if math.IsNaN(float64(h.Decide(now.Add(45*time.Second), 10, 10, 0.1, 0, true, time.Time{}))) {
		t.Fatal("unreachable")
	}
}

func TestUnmetDemandIsZeroWhenCapacityAlwaysCoversDemand(t *testing.T) {
	w := DefaultWorkload()
	sc := Scenario{Name: "tiny", Duration: time.Hour, Demand: func(time.Duration) float64 { return 100 }}
	if r := Run(w, sc, scaler(t, w, "KCA demand-only")); r.UnmetDemandPct != 0 || r.SaturatedSeconds != 0 {
		t.Fatalf("%+v", r)
	}
}
