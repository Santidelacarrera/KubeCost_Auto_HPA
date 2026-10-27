package sim

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func scenario(t *testing.T, name string) Scenario {
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
			if b := Run(w, sc, scaler(t, w, s.Name())); !reflect.DeepEqual(a, b) {
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

// Every scaler faces the same trace from the same initial condition: two instances of the same
// scaler give the same answer, and the starting replica count does not depend on the scaler.
func TestScalersShareTheSameTraceAndInitialConditions(t *testing.T) {
	w := DefaultWorkload()
	sc := scenario(t, "steady")
	a, b := Run(w, sc, scaler(t, w, Baseline)), Run(w, sc, scaler(t, w, Baseline))
	if !reflect.DeepEqual(a, b) {
		t.Fatal("same scaler, same trace must give the same result")
	}
	for _, s := range Scalers(w) {
		if r := Run(w, sc, s); r.MaxReplicas < 22 { // warm start for 1500 rps at 70% of 100 rps/pod
			t.Errorf("%s did not start from the shared warm start: max %d", s.Name(), r.MaxReplicas)
		}
	}
}

// With constant demand a controller must converge and stay put, whatever the cost signal does.
func TestConstantDemandNeverReversesDirection(t *testing.T) {
	w := DefaultWorkload()
	sc := scenario(t, "steady")
	for _, s := range Scalers(w) {
		if strings.Contains(s.Name(), "rejected") {
			continue // see TestExperimentalOptionStillOscillates
		}
		if r := Run(w, sc, s); r.Reversals != 0 {
			t.Errorf("%s: %d scale reversals under constant demand (%d actions)", s.Name(), r.Reversals, r.ScaleActions)
		}
	}
}

// Evidence for NOT enabling DemandMovesWhenCostUnstable: demand-only moves while the cost signal
// is unstable fight the cost-pressure target once it is stable again, and the replica count cycles.
// If this ever stops being true the option has changed and docs/BENCHMARKS.md must be re-read.
func TestExperimentalOptionStillOscillates(t *testing.T) {
	w := DefaultWorkload()
	r := Run(w, scenario(t, "steady"), scaler(t, w, "KCA unstable: demand-only moves (experimental, rejected)"))
	if r.Reversals < 3 {
		t.Fatalf("expected the experimental option to oscillate under constant demand, got %d reversals", r.Reversals)
	}
}

// The native HPA arithmetic of the baseline: ceil(ready*util/target), capped at +max(4 pods, 100%) per period.
func TestHPABaselineArithmetic(t *testing.T) {
	h := &HPA{Label: "h", Target: 0.7, Tolerance: 0.1, DownWindow: 5 * time.Minute, Min: 2, Max: 100}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := h.Decide(Obs{Now: now, Current: 10, Ready: 10, Util: 1.4}); got != 20 {
		t.Fatalf("10 pods at 140%% of request, target 70%%: want 20, got %d", got)
	}
	if got := h.Decide(Obs{Now: now.Add(15 * time.Second), Current: 3, Ready: 3, Util: 3.0}); got != 7 {
		t.Fatalf("scale-up must be limited to +max(4, 100%%)=+4: want 7, got %d", got)
	}
	if got := h.Decide(Obs{Now: now.Add(30 * time.Second), Current: 10, Ready: 10, Util: 0.72}); got != 10 {
		t.Fatalf("within 10%% tolerance must hold, got %d", got)
	}
	// scale-down is held by the highest recommendation inside the window
	if got := h.Decide(Obs{Now: now.Add(45 * time.Second), Current: 10, Ready: 10, Util: 0.1}); got != 10 {
		t.Fatalf("a higher recommendation inside the window must block removal, got %d", got)
	}
	if got := h.Decide(Obs{Now: now.Add(10 * time.Minute), Current: 10, Ready: 10, Util: 0.1}); got != 2 {
		t.Fatalf("after the window the HPA scales down to the recommendation, got %d", got)
	}
}

func TestPausedControllerDecidesButNeverWrites(t *testing.T) {
	w := DefaultWorkload()
	sc := scenario(t, "sudden-increase")
	sc.Pause = []Interval{{0, sc.Duration}}
	for _, name := range []string{"KCA shipped (cost window 15m)", Baseline} {
		r := Run(w, sc, scaler(t, w, name))
		if r.ScaleActions != 0 {
			t.Errorf("%s: %d writes while paused", name, r.ScaleActions)
		}
		if r.UnmetDemandPct == 0 {
			t.Errorf("%s: with no writes the step in demand must go unserved; the scenario is not exercising the pause", name)
		}
	}
}

func TestControllerRestartKeepsKCAHistoryButNotHPAMemory(t *testing.T) {
	h := hpa(DefaultWorkload(), "h", 10*time.Minute)
	h.hist = []rec{{time.Now(), 9}}
	h.Restart()
	if len(h.hist) != 0 {
		t.Fatal("the native HPA keeps its recommendations in memory: a restart must clear them")
	}
	if _, ok := Scaler(&KCA{}).(Restarter); ok {
		t.Fatal("KCA persists its history in the resource status: it must not model a memory loss")
	}
}

func TestUnmetDemandIsZeroWhenCapacityAlwaysCoversDemand(t *testing.T) {
	w := DefaultWorkload()
	sc := Scenario{Name: "tiny", Duration: time.Hour, Demand: constant(100)}
	if r := Run(w, sc, scaler(t, w, "KCA demand-only")); r.UnmetDemandPct != 0 || r.SaturatedSeconds != 0 || r.MissingReplicaSeconds != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestCostOutageHidesTheCostSignalOnlyFromCostScalers(t *testing.T) {
	w := DefaultWorkload()
	sc := scenario(t, "kubecost-outage")
	noOutage := sc
	noOutage.CostOutage = nil
	if reflect.DeepEqual(Run(w, sc, scaler(t, w, "KCA shipped (cost window 15m)")), Run(w, noOutage, scaler(t, w, "KCA shipped (cost window 15m)"))) {
		t.Fatal("a KubeCost outage must change what a cost-driven scaler does")
	}
	for _, name := range []string{Baseline, "KCA demand-only"} {
		if !reflect.DeepEqual(Run(w, sc, scaler(t, w, name)), Run(w, noOutage, scaler(t, w, name))) {
			t.Fatalf("%s does not use cost and must be unaffected by the outage", name)
		}
	}
}

// docs/BENCHMARKS.md embeds the simulator's own output between markers; a stale document fails here.
func TestBenchmarksDocMatchesTheSimulator(t *testing.T) {
	b, err := os.ReadFile("../../../docs/BENCHMARKS.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	i, j := strings.Index(doc, "<!-- simbench:begin -->\n"), strings.Index(doc, "<!-- simbench:end -->")
	if i < 0 || j < 0 {
		t.Fatal("docs/BENCHMARKS.md has no simbench markers")
	}
	got := doc[i+len("<!-- simbench:begin -->\n") : j]
	if want := Markdown(); got != want {
		t.Fatalf("docs/BENCHMARKS.md is stale: regenerate the block between the markers with `go run ./cmd/simbench`")
	}
}
