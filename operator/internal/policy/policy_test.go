package policy

import (
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/infra"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/money"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func usd(s string) money.USD {
	v, err := money.Parse(s)
	if err != nil {
		panic(err)
	}
	return v
}

func baseCfg() Config {
	return Config{
		MinReplicas: 2, MaxReplicas: 20, MinHealthy: 2,
		TargetUtil: 0.70, Ceiling: 0.90, CostUtil: 0.80, Tolerance: 0.10,
		DownWindow: 10 * time.Minute, UpWindow: 30 * time.Second, MinInterval: time.Minute, MaxGap: 3 * time.Minute,
		MaxHourly: usd("5.00"), TargetHourly: usd("4.00"), Guardrail: true,
		SpotMult: money.One(), OnDemandMult: money.One(), SpotBuffer: 0.2, AllowSafetyScaleUp: true,
	}
}

// input builds a healthy all-ready, all-on-demand observation; perReplica is the
// hourly cost of one replica.
func input(now time.Time, cur int32, util float64, perReplica string) Input {
	return Input{
		Now: now, Current: cur, Ready: cur,
		Demand: &Demand{Utilization: util},
		Cost:   &Cost{Hourly: usd(perReplica).MulInt(int64(cur)), Stable: true},
		Infra:  infra.Summary{OnDemand: cur},
	}
}

func steady(now time.Time, replicas int32, span, step time.Duration) []Recommendation {
	var h []Recommendation
	for ts := now.Add(-span); !ts.After(now); ts = ts.Add(step) {
		h = append(h, Recommendation{Time: ts, Replicas: replicas})
	}
	return h
}

func TestConfigValidate(t *testing.T) {
	if err := baseCfg().Validate(); err != nil {
		t.Fatal(err)
	}
	mut := map[string]func(*Config){
		"min 0":              func(c *Config) { c.MinReplicas = 0 },
		"max<min":            func(c *Config) { c.MaxReplicas = 1 },
		"util order":         func(c *Config) { c.CostUtil = 0.6 },
		"ceiling>1":          func(c *Config) { c.Ceiling = 1.2 },
		"tolerance":          func(c *Config) { c.Tolerance = 0.6 },
		"target>max budget":  func(c *Config) { c.TargetHourly = usd("6") },
		"zero multiplier":    func(c *Config) { c.SpotMult = money.Ratio{} },
		"negative window":    func(c *Config) { c.DownWindow = -1 },
		"minHealthy>maxRepl": func(c *Config) { c.MinHealthy = 99 },
	}
	for name, m := range mut {
		c := baseCfg()
		m(&c)
		if c.Validate() == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestHoldWithinTolerance(t *testing.T) {
	d := Evaluate(baseCfg(), input(t0, 4, 0.72, "0.5"))
	if d.Type != Hold || d.Proposed != 4 || d.Reason != ReasonWithinTolerance {
		t.Fatalf("%+v", d)
	}
}

func TestSafetyScaleUpIsCappedByBudgetOnlyAboveTheCeiling(t *testing.T) {
	// 4 replicas at $1/h = $4/h (budget $5). util 95% => L=3.8, safety=5, comfortable=6.
	d := Evaluate(baseCfg(), input(t0, 4, 0.95, "1"))
	if d.Type != ScaleUp || d.Proposed != 5 || d.Reason != ReasonSafetyScaleUp {
		t.Fatalf("%+v", d)
	}
	if !d.Details.BudgetLimited || d.Details.DemandReplicas != 6 || d.Details.SafetyReplicas != 5 || d.Details.BudgetReplicas != 5 {
		t.Fatalf("details %+v", d.Details)
	}
}

func TestScaleUpStillHappensWhenFarOverBudgetUnderHeavyDemand(t *testing.T) {
	// 12 replicas at $1/h = $12/h, 2.4x the budget, and overloaded.
	d := Evaluate(baseCfg(), input(t0, 12, 0.95, "1"))
	if d.Type != ScaleUp || d.Proposed != 13 {
		t.Fatalf("overloaded workload must scale to its safety size even over budget: %+v", d)
	}
	if d.Details.BudgetReplicas != 5 || !d.Details.BudgetLimited {
		t.Fatalf("budget bookkeeping: %+v", d.Details)
	}
	// ... and never above what the ceiling needs.
	if d.Proposed > d.Details.SafetyReplicas {
		t.Fatal("budget breach may only buy safety capacity, not comfort")
	}
}

func TestSafetyScaleUpDisabledFallsBackToStabilisedScaleUp(t *testing.T) {
	c := baseCfg()
	c.AllowSafetyScaleUp = false
	in := input(t0, 4, 0.95, "1")
	in.History = []Recommendation{{Time: t0.Add(-20 * time.Second), Replicas: 4}} // recently still fine at 4
	d := Evaluate(c, in)
	if d.Type != Hold || d.Reason != ReasonScaleUpStabilizing {
		t.Fatalf("%+v", d)
	}
}

func TestScaleUpWindowRequiresSustainedRecommendation(t *testing.T) {
	c := baseCfg()
	c.MaxHourly, c.TargetHourly = 0, 0 // demand only
	in := input(t0, 4, 0.8, "1")       // needs ceil(3.2/0.7)=5, not an emergency
	in.History = []Recommendation{{Time: t0.Add(-20 * time.Second), Replicas: 4}}
	if d := Evaluate(c, in); d.Type != Hold || d.Reason != ReasonScaleUpStabilizing {
		t.Fatalf("a recent lower recommendation blocks scale-up: %+v", d)
	}
	in.History = []Recommendation{{Time: t0.Add(-20 * time.Second), Replicas: 5}}
	if d := Evaluate(c, in); d.Type != ScaleUp || d.Proposed != 5 || d.Reason != ReasonDemandScaleUp {
		t.Fatalf("%+v", d)
	}
}

func TestCostPressureScaleDownStopsAtSafetyReplicas(t *testing.T) {
	c := baseCfg()
	// 10 replicas at $1/h => $10/h, over target and max budget. util 50%: L=5.
	in := input(t0, 10, 0.5, "1")
	in.History = steady(t0, 6, 11*time.Minute, 30*time.Second) // the cost-capped recommendation has been stable
	d := Evaluate(c, in)
	// comfortable=8, cost-pressure target=7, budget=5, safety=6 => 6.
	if d.Type != ScaleDown || d.Proposed != 6 || d.Reason != ReasonCostScaleDown || !d.CostDriven {
		t.Fatalf("%+v", d)
	}
	if got := 5.0 / 6; got > c.Ceiling {
		t.Fatalf("resulting utilisation %.2f exceeds the ceiling", got)
	}
	if d.Details.CostReplicas != 7 || d.Details.BudgetReplicas != 5 || d.Details.SafetyReplicas != 6 {
		t.Fatalf("%+v", d.Details)
	}
}

func TestRepeatedLowRecommendationsCannotBypassStabilisationWindow(t *testing.T) {
	c := baseCfg()
	c.MaxHourly, c.TargetHourly = 0, 0
	cur := int32(10)
	var hist []Recommendation
	var scaledAt time.Duration = -1
	for ts := t0; ts.Sub(t0) <= 15*time.Minute; ts = ts.Add(30 * time.Second) {
		in := input(ts, cur, 0.35, "1") // needs ceil(3.5/0.7)=5 every single time
		in.History = hist
		d := Evaluate(c, in)
		hist = d.History
		if d.Type == ScaleDown {
			scaledAt = ts.Sub(t0)
			break
		}
	}
	if scaledAt < c.DownWindow {
		t.Fatalf("scaled down after %v, before the %v window", scaledAt, c.DownWindow)
	}
	if scaledAt > c.DownWindow+time.Minute {
		t.Fatalf("scale-down took too long: %v", scaledAt)
	}
}

func TestHigherRecommendationInsideWindowBlocksScaleDown(t *testing.T) {
	c := baseCfg()
	c.MaxHourly, c.TargetHourly = 0, 0
	in := input(t0, 10, 0.35, "1")
	in.History = steady(t0, 5, 11*time.Minute, 30*time.Second)
	in.History[len(in.History)-6].Replicas = 9 // a spike 2.5 minutes ago
	d := Evaluate(c, in)
	if d.Type != ScaleDown || d.Proposed != 9 {
		t.Fatalf("must step down only to the highest recent recommendation: %+v", d)
	}
	in.History[len(in.History)-6].Replicas = 10
	if d := Evaluate(c, in); d.Type != Hold || d.Reason != ReasonDownStabilizing {
		t.Fatalf("%+v", d)
	}
}

func TestCostSpikeCannotImmediatelyRemoveReplicas(t *testing.T) {
	c := baseCfg()
	// Steady for 9 minutes at 10 replicas needing 8 (no cost pressure)...
	in := input(t0, 10, 0.5, "0.3") // $3/h: under the $4 target
	in.History = steady(t0.Add(-30*time.Second), 8, 9*time.Minute+30*time.Second, 30*time.Second)
	// ...then one sample reports $10/h (price spike): recommendation drops to the cost target.
	in.Cost = &Cost{Hourly: usd("10"), Stable: true}
	d := Evaluate(c, in)
	if d.Type == ScaleDown && time.Duration(len(in.History))*30*time.Second < c.DownWindow {
		t.Fatalf("a single cost spike must not shrink the workload: %+v", d)
	}
	// And a transient cost *dip* never shrinks anything: it can only relax pressure.
	in2 := input(t0, 10, 0.6, "1")
	in2.Cost = &Cost{Hourly: usd("0.10"), Stable: true}
	in2.History = steady(t0, 9, 11*time.Minute, 30*time.Second)
	if d := Evaluate(c, in2); d.Type == ScaleDown && d.CostDriven {
		t.Fatalf("a cost dip must not drive a scale-down: %+v", d)
	}
}

func TestEmptyHistoryWarmsUpInsteadOfScalingDownImmediately(t *testing.T) {
	c := baseCfg()
	c.MaxHourly, c.TargetHourly = 0, 0
	d := Evaluate(c, input(t0, 10, 0.35, "1")) // e.g. after losing the status
	if d.Type != Hold || d.Reason != ReasonDownWarmup {
		t.Fatalf("%+v", d)
	}
	if len(d.History) != 1 {
		t.Fatalf("recommendation must still be recorded: %+v", d.History)
	}
}

func TestRestartKeepsStabilisationState(t *testing.T) {
	c := baseCfg()
	c.MaxHourly, c.TargetHourly = 0, 0
	var hist []Recommendation
	run := func(from, to time.Duration) Decision {
		var d Decision
		for ts := t0.Add(from); ts.Sub(t0) <= to; ts = ts.Add(30 * time.Second) {
			in := input(ts, 10, 0.35, "1")
			in.History = hist
			d = Evaluate(c, in)
			hist = d.History
		}
		return d
	}
	run(0, 5*time.Minute)
	// "restart": history is round-tripped through the (persisted) status only.
	persisted := append([]Recommendation(nil), hist...)
	hist = persisted
	if d := run(5*time.Minute+30*time.Second, 9*time.Minute); d.Type == ScaleDown {
		t.Fatalf("restart must not shorten the window: %+v", d)
	}
	if d := run(9*time.Minute+30*time.Second, 11*time.Minute); d.Type != ScaleDown {
		t.Fatalf("expected scale-down once the window is covered: %+v", d)
	}
}

func TestHistoryGapRestartsWarmup(t *testing.T) {
	c := baseCfg()
	c.MaxHourly, c.TargetHourly = 0, 0
	in := input(t0, 10, 0.35, "1")
	in.History = steady(t0.Add(-6*time.Minute), 5, 6*time.Minute, 30*time.Second) // controller was silent for 6 minutes (> MaxGap)
	d := Evaluate(c, in)
	if d.Type != Hold || d.Reason != ReasonDownWarmup || !d.Details.HistoryDiscarded {
		t.Fatalf("%+v", d)
	}
}

func TestCooldown(t *testing.T) {
	c := baseCfg()
	c.MaxHourly, c.TargetHourly = 0, 0
	down := input(t0, 10, 0.35, "1")
	down.History = steady(t0, 5, 11*time.Minute, 30*time.Second)
	down.LastScale = t0.Add(-30 * time.Second)
	if d := Evaluate(c, down); d.Type != Hold || d.Reason != ReasonCooldown {
		t.Fatalf("%+v", d)
	}
	down.LastScale = t0.Add(-61 * time.Second)
	if d := Evaluate(c, down); d.Type != ScaleDown {
		t.Fatalf("%+v", d)
	}
	// non-emergency scale-up also waits; an emergency does not.
	up := input(t0, 4, 0.8, "1")
	up.History = []Recommendation{{Time: t0.Add(-20 * time.Second), Replicas: 5}}
	up.LastScale = t0.Add(-10 * time.Second)
	if d := Evaluate(c, up); d.Type != Hold || d.Reason != ReasonCooldown {
		t.Fatalf("%+v", d)
	}
	hot := input(t0, 4, 0.97, "1")
	hot.LastScale = t0.Add(-10 * time.Second)
	if d := Evaluate(c, hot); d.Type != ScaleUp || d.Reason != ReasonSafetyScaleUp {
		t.Fatalf("safety scale-up must bypass the cooldown: %+v", d)
	}
}

func TestBoundsAreEnforcedImmediately(t *testing.T) {
	c := baseCfg()
	in := input(t0, 25, 0.5, "1")
	in.Demand = nil // even without any signal
	in.LastScale = t0
	if d := Evaluate(c, in); d.Type != ScaleDown || d.Proposed != 20 || d.Reason != ReasonBoundsEnforced {
		t.Fatalf("%+v", d)
	}
	low := input(t0, 1, 0.5, "1")
	if d := Evaluate(c, low); d.Type != ScaleUp || d.Proposed != 2 || d.Reason != ReasonBoundsEnforced {
		t.Fatalf("%+v", d)
	}
}

func TestMinAndMaxClampRecommendations(t *testing.T) {
	c := baseCfg()
	c.MaxHourly, c.TargetHourly = 0, 0
	in := input(t0, 20, 0.99, "0.1")
	if d := Evaluate(c, in); d.Proposed > 20 {
		t.Fatalf("%+v", d)
	}
	low := input(t0, 3, 0.01, "0.1")
	low.History = steady(t0, 2, 11*time.Minute, 30*time.Second)
	if d := Evaluate(c, low); d.Type != ScaleDown || d.Proposed != 2 {
		t.Fatalf("never below minReplicas: %+v", d)
	}
	c.MinHealthy = 4
	if d := Evaluate(c, low); d.Candidate < 4 {
		t.Fatalf("never below minHealthyReplicas: %+v", d)
	}
}

func TestMissingDemandHolds(t *testing.T) {
	in := input(t0, 6, 0.5, "1")
	in.Demand = nil
	d := Evaluate(baseCfg(), in)
	if d.Type != InvalidMetrics || d.Proposed != 6 || d.Reason != ReasonDemandUnavailable {
		t.Fatalf("%+v", d)
	}
	in.Demand = &Demand{Utilization: math.NaN()}
	if d := Evaluate(baseCfg(), in); d.Type != InvalidMetrics {
		t.Fatalf("NaN utilisation must be invalid: %+v", d)
	}
	in.Demand = &Demand{Utilization: 0.5}
	in.Ready = 0
	if d := Evaluate(baseCfg(), in); d.Type != Hold || d.Reason != ReasonNoReadyReplicas {
		t.Fatalf("%+v", d)
	}
}

func TestFreezeWhenCostUnavailable(t *testing.T) {
	c := baseCfg()
	in := input(t0, 10, 0.2, "1") // would otherwise scale down hard
	in.History = steady(t0, 4, 11*time.Minute, 30*time.Second)
	in.Cost = nil
	d := Evaluate(c, in)
	if d.Type != InvalidMetrics || d.Proposed != 10 || d.Reason != ReasonCostUnavailable {
		t.Fatalf("cost failure must never authorise a scale-down: %+v", d)
	}
	in.Cost = &Cost{Hourly: usd("10"), Stable: false}
	if d := Evaluate(c, in); d.Type != InvalidMetrics || d.Proposed != 10 || d.Reason != ReasonCostEstimateUnstable {
		t.Fatalf("%+v", d)
	}
	if len(d.History) != len(Evaluate(c, in).History) {
		t.Fatal("deterministic")
	}
}

func TestFreezeStillAllowsAuthorisedSafetyScaleUp(t *testing.T) {
	c := baseCfg()
	in := input(t0, 4, 0.97, "1")
	in.Cost = nil
	d := Evaluate(c, in)
	if d.Type != ScaleUp || d.Reason != ReasonSafetyScaleUp || d.Proposed < 5 {
		t.Fatalf("%+v", d)
	}
	c.AllowSafetyScaleUp = false
	if d := Evaluate(c, in); d.Type != InvalidMetrics || d.Proposed != 4 {
		t.Fatalf("safety scale-up must be explicitly authorised: %+v", d)
	}
	// pending pods: adding replicas would not help
	c.AllowSafetyScaleUp = true
	in.Infra.Pending = 2
	if d := Evaluate(c, in); d.Type == ScaleUp {
		t.Fatalf("%+v", d)
	}
}

func TestDemandOnlyModeDoesNotNeedCost(t *testing.T) {
	c := baseCfg()
	c.MaxHourly, c.TargetHourly = 0, 0
	in := input(t0, 4, 0.95, "1")
	in.Cost = nil
	if d := Evaluate(c, in); d.Type != ScaleUp {
		t.Fatalf("%+v", d)
	}
}

func TestSafetyGates(t *testing.T) {
	c := baseCfg()
	c.MaxHourly, c.TargetHourly = 0, 0
	down := input(t0, 10, 0.35, "1")
	down.History = steady(t0, 5, 11*time.Minute, 30*time.Second)

	pending := down
	pending.Infra.Pending = 1
	if d := Evaluate(c, pending); d.Type != Hold || d.Reason != ReasonPendingPods {
		t.Fatalf("%+v", d)
	}
	notReady := down
	notReady.Ready = 9
	notReady.Infra.OnDemand = 9
	if d := Evaluate(c, notReady); d.Type != Hold || d.Reason != ReasonNotAllReady {
		t.Fatalf("%+v", d)
	}
	terminating := down
	terminating.Infra.Terminating = 1
	if d := Evaluate(c, terminating); d.Type != Hold || d.Reason != ReasonNotAllReady {
		t.Fatalf("%+v", d)
	}
	up := input(t0, 4, 0.95, "1")
	up.Infra.Pending = 3
	if d := Evaluate(c, up); d.Type != Hold || d.Reason != ReasonPendingPods {
		t.Fatalf("scale-up with unschedulable pods is pointless: %+v", d)
	}
}

func TestSpotCapacityIsNotGuaranteedCapacity(t *testing.T) {
	c := baseCfg()
	c.MaxHourly, c.TargetHourly = 0, 0
	od := input(t0, 10, 0.6, "1")
	spot := od
	spot.Infra = infra.Summary{Spot: 10}
	dOD, dSpot := Evaluate(c, od), Evaluate(c, spot)
	if dSpot.Details.DemandReplicas <= dOD.Details.DemandReplicas || dSpot.Details.CapacityFactor >= 1 {
		t.Fatalf("all-Spot workloads need more replicas for the same load: spot=%+v od=%+v", dSpot.Details, dOD.Details)
	}
	// 10 spot @ buffer .2 => factor .8: need = ceil(6/(.7*.8)) = 11
	if dSpot.Details.DemandReplicas != 11 {
		t.Fatalf("got %d", dSpot.Details.DemandReplicas)
	}
	unknown := od
	unknown.Infra = infra.Summary{Unknown: 10}
	if Evaluate(c, unknown).Details.DemandReplicas != dOD.Details.DemandReplicas {
		t.Fatal("unknown placement is treated as on-demand (not discounted)")
	}
}

func TestMarginalCostModelUsesCapacityClassMultipliers(t *testing.T) {
	c := baseCfg()
	c.SpotMult = money.MustRatio("0.35")
	c.OnDemandMult = money.MustRatio("1")
	in := input(t0, 4, 0.72, "1") // $4/h, no change needed; we only inspect the model
	in.Infra = infra.Summary{Spot: 2, OnDemand: 2}
	d := Evaluate(c, in)
	// blended = (2*.35+2*1)/4 = .675; per-replica $1 => add 1/.675, remove .35/.675
	if got := d.Details.MarginalAdd.Float(); math.Abs(got-1.481481) > 1e-6 {
		t.Fatalf("add %v", got)
	}
	if got := d.Details.MarginalRemove.Float(); math.Abs(got-0.518519) > 1e-6 {
		t.Fatalf("remove %v", got)
	}
	// adding is priced pessimistically (on-demand), removing optimistically-safe (cheapest present).
	if d.Details.MarginalAdd <= d.Details.MarginalRemove {
		t.Fatal("asymmetry expected")
	}
}

func TestStrictBudgetBoundary(t *testing.T) {
	c := baseCfg()
	c.MaxHourly = usd("0.3") // exactly 3 replicas at $0.10
	c.TargetHourly = 0
	in := input(t0, 3, 0.8, "0.10")
	in.Cost = &Cost{Hourly: usd("0.1").MulInt(3), Stable: true}
	d := Evaluate(c, in)
	if d.Details.BudgetReplicas != 3 {
		t.Fatalf("exactly-at-budget must not be treated as over budget: %+v", d.Details)
	}
	in.Cost = &Cost{Hourly: usd("0.300001"), Stable: true}
	if d := Evaluate(c, in); d.Details.BudgetReplicas != 2 && d.Details.BudgetReplicas != 3 {
		t.Fatalf("%+v", d.Details)
	}
	if d := Evaluate(c, in); !d.Details.CostPressure {
		t.Fatal("one micro-dollar over the budget is over the budget")
	}
}

func TestZeroCostAndHugeValuesDoNotPanicOrDivideByZero(t *testing.T) {
	c := baseCfg()
	free := input(t0, 4, 0.95, "0")
	free.Cost = &Cost{Hourly: 0, Stable: true}
	if d := Evaluate(c, free); d.Details.BudgetReplicas != c.MaxReplicas {
		t.Fatalf("free workload has an unbounded budget: %+v", d.Details)
	}
	huge := input(t0, 4, 0.95, "1")
	huge.Cost = &Cost{Hourly: money.Max, Stable: true}
	_ = Evaluate(c, huge)
	huge.Ready, huge.Current = 1<<30, 1<<30
	_ = Evaluate(c, huge)
}

func TestEvaluateIsDeterministic(t *testing.T) {
	in := input(t0, 10, 0.5, "1")
	in.History = steady(t0, 10, 11*time.Minute, 30*time.Second)
	a, b := Evaluate(baseCfg(), in), Evaluate(baseCfg(), in)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("%+v != %+v", a, b)
	}
}

// plant simulates a workload with constant demand: utilisation follows the
// replica count instantly, and cost is replicas * price.
func TestNoOscillationUnderConstantDemand(t *testing.T) {
	for name, sc := range map[string]struct {
		load        float64
		price       []string // price per replica over time (cycled per phase)
		spotFrac    float64
		expectFinal int32
	}{
		"cost pressure converges":       {load: 6, price: []string{"1"}, expectFinal: 7},
		"price doubles, demand same":    {load: 6, price: []string{"1", "2"}, expectFinal: 7},
		"cheap workload follows demand": {load: 6, price: []string{"0.1"}, expectFinal: 9},
	} {
		t.Run(name, func(t *testing.T) {
			c := baseCfg()
			cur, last := int32(9), int32(9)
			var hist []Recommendation
			var lastScale time.Time
			changes, lastChangeAt := 0, time.Duration(0)
			end := 120 * time.Minute
			for ts := t0; ts.Sub(t0) <= end; ts = ts.Add(30 * time.Second) {
				price := sc.price[0]
				if len(sc.price) > 1 && ts.Sub(t0) > end/2 {
					price = sc.price[1]
				}
				in := input(ts, cur, sc.load/float64(cur), price)
				in.History, in.LastScale = hist, lastScale
				d := Evaluate(c, in)
				hist = d.History
				if d.Proposed < c.MinReplicas || d.Proposed > c.MaxReplicas {
					t.Fatalf("out of bounds: %+v", d)
				}
				if d.Type == ScaleUp || d.Type == ScaleDown {
					if (d.Proposed > cur) != (cur > last) && changes > 0 && ts.Sub(t0) > 30*time.Minute {
						t.Fatalf("direction reversal at %v: %d -> %d", ts.Sub(t0), cur, d.Proposed)
					}
					last, cur, lastScale = cur, d.Proposed, ts
					changes++
					lastChangeAt = ts.Sub(t0)
				}
			}
			if cur != sc.expectFinal {
				t.Fatalf("final replicas %d want %d (changes=%d)", cur, sc.expectFinal, changes)
			}
			if lastChangeAt > 70*time.Minute {
				t.Fatalf("still scaling at %v: not converged", lastChangeAt)
			}
		})
	}
}

func TestRecommendationHistoryIsBoundedAndOrdered(t *testing.T) {
	c := baseCfg()
	var hist []Recommendation
	for ts := t0; ts.Sub(t0) < 6*time.Hour; ts = ts.Add(30 * time.Second) {
		in := input(ts, 6, 0.7, "0.5")
		in.History = hist
		hist = Evaluate(c, in).History
	}
	max := int((c.DownWindow + c.MaxGap) / (30 * time.Second))
	if len(hist) > max+2 {
		t.Fatalf("history grew unbounded: %d entries (max ~%d)", len(hist), max)
	}
	for i := 1; i < len(hist); i++ {
		if !hist[i].Time.After(hist[i-1].Time) {
			t.Fatal("history must be strictly ordered")
		}
	}
	// future-dated entries (clock skew / tampering) are dropped
	in := input(t0, 6, 0.7, "0.5")
	in.History = []Recommendation{{Time: t0.Add(time.Hour), Replicas: 20}}
	if got := Evaluate(c, in).History; len(got) != 1 || got[0].Replicas == 20 {
		t.Fatalf("%+v", got)
	}
}
