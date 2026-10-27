package sim

import (
	"math"
	"time"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/infra"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/money"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/policy"
)

// HPA reproduces the documented autoscaling/v2 algorithm with the default behaviour: every 15 s,
// desired = ceil(ready * util/target) unless within tolerance; scale-down uses the highest
// recommendation of the stabilisation window; scale-up is capped per period at max(+4 pods, +100%).
// Its recommendations live in the kube-controller-manager's memory, so a restart loses them
// (assumption, see docs/BENCHMARKS.md).
type HPA struct {
	Label      string
	Target     float64
	Tolerance  float64
	DownWindow time.Duration
	Min, Max   int32
	hist       []rec
}

type rec struct {
	t time.Time
	n int32
}

func (h *HPA) Name() string              { return h.Label }
func (h *HPA) Period() time.Duration     { return 15 * time.Second }
func (h *HPA) CostWindow() time.Duration { return 0 }
func (h *HPA) Restart()                  { h.hist = nil }
func (h *HPA) Decide(o Obs) int32 {
	ratio := o.Util / h.Target
	desired := o.Current
	if math.Abs(ratio-1) > h.Tolerance {
		desired = int32(math.Ceil(float64(o.Ready) * ratio))
	}
	desired = clampI(desired, h.Min, h.Max)
	h.hist = append(h.hist, rec{o.Now, desired})
	cut := o.Now.Add(-h.DownWindow)
	keep := h.hist[:0]
	for _, r := range h.hist {
		if !r.t.Before(cut) {
			keep = append(keep, r)
		}
	}
	h.hist = keep
	if desired < o.Current {
		hi := desired
		for _, r := range h.hist {
			if r.n > hi {
				hi = r.n
			}
		}
		desired = minI(o.Current, hi)
	}
	if desired > o.Current {
		desired = minI(desired, o.Current+maxI(4, o.Current))
	}
	return clampI(desired, h.Min, h.Max)
}

// KCA drives the real policy.Evaluate. Its history is persisted in the resource status, so a
// process restart does not lose it (the engine itself discards history across a gap longer than MaxGap).
type KCA struct {
	Label  string
	Cfg    policy.Config
	Window time.Duration // cost window; 0 = demand-only (no cost policy)
	Every  time.Duration // evaluation interval (spec.evaluationInterval, default 30 s)
	hist   []policy.Recommendation
}

func (k *KCA) Name() string              { return k.Label }
func (k *KCA) Period() time.Duration     { return k.Every }
func (k *KCA) CostWindow() time.Duration { return k.Window }

func (k *KCA) Decide(o Obs) int32 {
	in := policy.Input{
		Now: o.Now, Current: o.Current, Ready: o.Ready,
		Demand:  &policy.Demand{Utilization: o.Util},
		Infra:   infra.Summary{OnDemand: o.Ready},
		History: k.hist, LastScale: o.LastScale,
	}
	if k.Window > 0 && o.Cost != nil {
		if v, err := money.FromFloat(o.Cost.Hourly); err == nil {
			in.Cost = &policy.Cost{Hourly: v, Stable: o.Cost.Stable}
		}
	}
	d := policy.Evaluate(k.Cfg, in)
	k.hist = d.History
	if d.Type == policy.ScaleUp || d.Type == policy.ScaleDown {
		return d.Proposed
	}
	return o.Current
}

// KCAFallback is KCA in failurePolicy.mode=ResourceMetricsFallback: when cost data stays unavailable
// for ActivationDelay the operator-owned native HPA takes the replicas; RecoveryDelay of valid cost
// data hands them back.
type KCAFallback struct {
	KCA
	HPA                            *HPA
	ActivationDelay, RecoveryDelay time.Duration
	outageSince, healthySince      time.Time
	active                         bool
}

func (f *KCAFallback) Decide(o Obs) int32 {
	if o.Cost == nil {
		f.healthySince = time.Time{}
		if f.outageSince.IsZero() {
			f.outageSince = o.Now
		}
		if !f.active && o.Now.Sub(f.outageSince) >= f.ActivationDelay {
			f.active = true
		}
	} else {
		f.outageSince = time.Time{}
		if f.healthySince.IsZero() {
			f.healthySince = o.Now
		}
		if f.active && o.Now.Sub(f.healthySince) >= f.RecoveryDelay {
			f.active = false
		}
	}
	if f.active {
		return f.HPA.Decide(o)
	}
	return f.KCA.Decide(o)
}
