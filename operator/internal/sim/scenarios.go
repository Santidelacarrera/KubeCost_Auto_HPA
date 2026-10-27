package sim

import (
	"math"
	"time"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/money"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/policy"
)

// DefaultWorkload is the single assumption set behind every published result.
func DefaultWorkload() Workload {
	return Workload{PerPodRPS: 100, StartupDelay: 30 * time.Second, PricePerHour: 0.05, MinReplicas: 2, MaxReplicas: 60}
}

const (
	min1 = time.Minute
	hour = time.Hour
)

func constant(v float64) func(time.Duration) float64 { return func(time.Duration) float64 { return v } }

func spikes(base, peak float64, width time.Duration, at ...time.Duration) func(time.Duration) float64 {
	return func(t time.Duration) float64 {
		for _, a := range at {
			if t >= a && t < a+width {
				return peak
			}
		}
		return base
	}
}

func decline(t time.Duration) float64 {
	if t < 20*min1 {
		return 3000
	}
	return math.Max(500, 3000-2500*float64(t-20*min1)/float64(60*min1))
}

func ramp(t time.Duration) float64 {
	switch {
	case t < 40*min1:
		return 500 + 2500*float64(t)/float64(40*min1)
	case t < 80*min1:
		return 3000
	default:
		return 3000 - 2500*math.Min(1, float64(t-80*min1)/float64(40*min1))
	}
}

func stepUp(t time.Duration) float64 {
	if t < 30*min1 {
		return 800
	}
	return 3000
}

// Scenarios are the demand traces and disturbances. Every scaler sees exactly the same one.
func Scenarios() []Scenario {
	return []Scenario{
		{Name: "steady", Note: "constant demand", Duration: 2 * hour, Demand: constant(1500)},
		{Name: "sudden-increase", Note: "demand 800 to 3000 rps at 30 min and stays", Duration: 2 * hour, Demand: stepUp, LastChange: 30 * min1},
		{Name: "ramp-up-and-down", Note: "500 to 3000 over 40 min, hold, back down over 40 min", Duration: 2 * hour, Demand: ramp, LastChange: 120 * min1},
		{Name: "short-spikes", Note: "three 2-minute spikes to 3000 rps over a 800 rps base", Duration: 2 * hour,
			Demand: spikes(800, 3000, 2*min1, 20*min1, 50*min1, 80*min1), LastChange: 82 * min1},
		{Name: "burst", Note: "two 5-minute spikes to 3200 rps over a 800 rps base", Duration: 2 * hour,
			Demand: spikes(800, 3200, 5*min1, 30*min1, 75*min1), LastChange: 80 * min1},
		{Name: "gradual-decline", Note: "3000 down to 500 rps over an hour", Duration: 2 * hour, Demand: decline, LastChange: 80 * min1},
		{Name: "intermittent", Note: "square wave 400/2000 rps, 10 min each", Duration: 2 * hour, LastChange: 110 * min1,
			Demand: func(t time.Duration) float64 {
				if (t/(10*min1))%2 == 0 {
					return 400
				}
				return 2000
			}},
		{Name: "price-shock", Note: "constant demand; replica price doubles from 40 to 80 min (capacity reclaimed, on-demand fallback)", Duration: 2 * hour,
			Demand: constant(1500), LastChange: 80 * min1,
			Price: func(t time.Duration) float64 {
				if t >= 40*min1 && t < 80*min1 {
					return 0.10
				}
				return 0.05
			}},
		{Name: "metrics-lag", Note: "the 5-minute spikes again, but the controller sees demand 90 s late", Duration: 2 * hour,
			Demand: spikes(800, 3200, 5*min1, 30*min1, 75*min1), MetricLag: 90 * time.Second, LastChange: 80 * min1},
		{Name: "kubecost-outage", Note: "ramp; KubeCost unreachable from 40 to 70 min (cost-free scalers are unaffected)", Duration: 2 * hour,
			Demand: ramp, CostOutage: []Interval{{40 * min1, 70 * min1}}, LastChange: 120 * min1},
		{Name: "controller-restart", Note: "gradual decline; the controller is down from 40 to 45 min and restarts", Duration: 2 * hour,
			Demand: decline, Down: []Interval{{40 * min1, 45 * min1}}, LastChange: 80 * min1},
		{Name: "pause-then-resume", Note: "sudden increase at 30 min while the emergency pause is on from 25 to 55 min", Duration: 2 * hour,
			Demand: stepUp, Pause: []Interval{{25 * min1, 55 * min1}}, LastChange: 55 * min1},
		{Name: "external-scale-down", Note: "constant demand; another actor sets replicas to 2 at 40 min", Duration: 2 * hour,
			Demand: constant(1500), External: []ExternalWrite{{40 * min1, 2}}, LastChange: 40 * min1},
		{Name: "external-scale-up", Note: "constant demand; another actor sets replicas to 60 at 40 min", Duration: 2 * hour,
			Demand: constant(1500), External: []ExternalWrite{{40 * min1, 60}}, LastChange: 40 * min1},
	}
}

func usd(s string) money.USD {
	v, err := money.Parse(s)
	if err != nil {
		panic(err)
	}
	return v
}

// kcaConfig is the shipped defaults (spec defaults: target 70%, ceiling 90%, cost-pressure 80%,
// tolerance 10%, scale-down 10 min, scale-up 30 s, cooldown 60 s) with a $1.00/h target and
// $2.00/h budget (20 and 40 replicas at the base price) when cost is on.
func kcaConfig(w Workload, cost bool, down time.Duration, unstable policy.UnstableMode) policy.Config {
	c := policy.Config{
		MinReplicas: w.MinReplicas, MaxReplicas: w.MaxReplicas, MinHealthy: w.MinReplicas,
		TargetUtil: 0.70, Ceiling: 0.90, CostUtil: 0.80, Tolerance: 0.10,
		DownWindow: down, UpWindow: 30 * time.Second, MinInterval: time.Minute, MaxGap: 3 * time.Minute,
		SpotMult: money.One(), OnDemandMult: money.One(), SpotBuffer: 0.2, AllowSafetyScaleUp: true,
		WhenCostUnstable: unstable,
	}
	if cost {
		c.TargetHourly, c.MaxHourly, c.Guardrail = usd("1.00"), usd("2.00"), true
	}
	return c
}

// Baseline is the HPA every other scaler is judged against: same stabilisation window as the
// shipped KubeCostAutoscaler, so the comparison does not reward a longer window by itself.
const Baseline = "HPA (down 10m)"

func hpa(w Workload, label string, down time.Duration) *HPA {
	return &HPA{Label: label, Target: 0.70, Tolerance: 0.10, DownWindow: down, Min: w.MinReplicas, Max: w.MaxReplicas}
}

func kca(w Workload, label string, cost bool, window, down time.Duration, unstable policy.UnstableMode, tweak ...func(*policy.Config)) *KCA {
	k := &KCA{Label: label, Cfg: kcaConfig(w, cost, down, unstable), Every: 30 * time.Second}
	for _, f := range tweak {
		f(&k.Cfg)
	}
	if cost {
		k.Window = window
	}
	return k
}

// Scalers returns fresh (they are stateful) scalers for one run, in report order.
func Scalers(w Workload) []Scaler {
	shipped := kca(w, "KCA shipped (cost window 15m)", true, 15*min1, 10*min1, policy.UnstableFreeze)
	fb := &KCAFallback{
		KCA: *kca(w, "KCA fallback mode (15m)", true, 15*min1, 10*min1, policy.UnstableFreeze),
		HPA: hpa(w, "fallback", 5*min1), ActivationDelay: 2 * min1, RecoveryDelay: 2 * min1,
	}
	return []Scaler{
		hpa(w, "HPA default (down 5m)", 5*min1),
		hpa(w, Baseline, 10*min1),
		kca(w, "KCA demand-only", false, 0, 10*min1, policy.UnstableFreeze),
		shipped,
		kca(w, "KCA cost window 5m", true, 5*min1, 10*min1, policy.UnstableFreeze),
		kca(w, "KCA cost window 30m", true, 30*min1, 10*min1, policy.UnstableFreeze),
		kca(w, "KCA unstable: demand-only moves (experimental, rejected)", true, 15*min1, 10*min1, policy.UnstableDemandOnly),
		kca(w, "KCA unstable: scale up to cost target (experimental)", true, 15*min1, 10*min1, policy.UnstableScaleUpAtCostTarget),
		kca(w, "KCA conservative (ceiling 80%, cost target 75%)", true, 15*min1, 10*min1, policy.UnstableFreeze,
			func(c *policy.Config) { c.Ceiling, c.CostUtil = 0.80, 0.75 }),
		kca(w, "KCA conservative + scale up to cost target (experimental)", true, 15*min1, 10*min1, policy.UnstableScaleUpAtCostTarget,
			func(c *policy.Config) { c.Ceiling, c.CostUtil = 0.80, 0.75 }),
		kca(w, "KCA down window 5m", true, 15*min1, 5*min1, policy.UnstableFreeze),
		kca(w, "KCA down window 20m", true, 15*min1, 20*min1, policy.UnstableFreeze),
		fb,
	}
}
