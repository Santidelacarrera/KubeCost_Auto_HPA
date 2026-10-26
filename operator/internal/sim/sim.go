// Package sim is a deterministic, synthetic simulator used to compare the
// KubeCostAutoscaler decision engine (internal/policy) with the documented
// behaviour of a native Kubernetes HorizontalPodAutoscaler on identical demand.
//
// It is NOT a measurement of a real cluster. There is no latency model, no
// scheduler, no node provisioning and no real KubeCost: demand is a prescribed
// request rate, capacity is ready replicas times a fixed per-pod rate, and cost
// is replica-hours times a fixed price. Results are only meaningful relative to
// each other and under these assumptions (see docs/BENCHMARKS.md).
package sim

import (
	"math"
	"time"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/infra"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/money"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/policy"
)

// Workload is the simulated application.
type Workload struct {
	PerPodRPS    float64       // requests/s one ready pod serves at 100% of its request
	StartupDelay time.Duration // from replica creation to ready
	PricePerHour float64       // USD per replica-hour (fixed assumption)
	MinReplicas  int32
	MaxReplicas  int32
	Initial      int32 // 0 = warm start from the first demand sample
}

// Scenario is a named demand pattern (requests per second over time).
type Scenario struct {
	Name     string
	Duration time.Duration
	Demand   func(t time.Duration) float64
}

// Scaler decides the next replica count once per control period.
type Scaler interface {
	Name() string
	// Decide returns the desired replica count (equal to current for no change).
	Decide(now time.Time, current, ready int32, util float64, costWindowHourly float64, costStable bool, lastScale time.Time) int32
}

// Result is the outcome of one scenario/scaler run.
type Result struct {
	Scenario, Scaler string
	CostUSD          float64
	AvgReplicas      float64
	MaxReplicas      int32
	ScaleActions     int
	Reversals        int     // direction changes between consecutive scale actions
	UnmetDemandPct   float64 // demand above ready capacity, as % of all demand
	SaturatedSeconds float64 // time with demand > ready capacity
	AboveCeilingSecs float64 // time with utilisation above 90%
	LongestSatSecs   float64 // longest continuous saturation episode
}

const (
	step    = 5 * time.Second
	period  = 15 * time.Second // control period for every scaler
	costWin = 15 * time.Minute // KubeCost averaging window the controller would use
)

// Run simulates one scaler on one scenario.
func Run(w Workload, sc Scenario, s Scaler) Result {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	readyAt := make([]time.Time, 0, int(w.MaxReplicas))
	if w.Initial == 0 { // warm start: the replicas a native HPA would settle on for the first demand sample
		w.Initial = clampI(int32(math.Ceil(sc.Demand(0)/(0.7*w.PerPodRPS))), w.MinReplicas, w.MaxReplicas)
	}
	for i := int32(0); i < w.Initial; i++ {
		readyAt = append(readyAt, start.Add(-time.Hour)) // already running
	}
	res := Result{Scenario: sc.Name, Scaler: s.Name()}
	var (
		lastScale      time.Time
		lastDir        int
		sumReplicas    float64
		demandTotal    float64
		unmet          float64
		sat, ceil, cur float64
		longest        float64
		nextCtl        = start
		history        []sample // replica history for the cost window
		lastChange     = start.Add(-24 * time.Hour)
	)
	for el := time.Duration(0); el < sc.Duration; el += step {
		now := start.Add(el)
		d := math.Max(0, sc.Demand(el))
		ready := int32(0)
		for _, r := range readyAt {
			if !r.After(now) {
				ready++
			}
		}
		capRPS := float64(ready) * w.PerPodRPS
		dt := step.Seconds()
		res.CostUSD += float64(len(readyAt)) * w.PricePerHour * dt / 3600
		sumReplicas += float64(len(readyAt)) * dt
		demandTotal += d * dt
		if d > capRPS {
			unmet += (d - capRPS) * dt
			sat += dt
			cur += dt
			if cur > longest {
				longest = cur
			}
		} else {
			cur = 0
		}
		if capRPS > 0 && d/capRPS > 0.9 || capRPS == 0 && d > 0 {
			ceil += dt
		}
		if int32(len(readyAt)) > res.MaxReplicas {
			res.MaxReplicas = int32(len(readyAt))
		}
		history = append(history, sample{now, int32(len(readyAt))})

		if now.Before(nextCtl) {
			continue
		}
		nextCtl = now.Add(period)
		current := int32(len(readyAt))
		if ready == 0 {
			continue
		}
		util := math.Min(d/(float64(ready)*w.PerPodRPS), 2.0)
		hourly, stable := costWindow(history, now, lastChange, w.PricePerHour)
		want := s.Decide(now, current, ready, util, hourly, stable, lastScale)
		want = clampI(want, w.MinReplicas, w.MaxReplicas)
		if want == current {
			continue
		}
		dir := 1
		if want < current {
			dir = -1
		}
		if lastDir != 0 && dir != lastDir {
			res.Reversals++
		}
		lastDir = dir
		res.ScaleActions++
		lastScale, lastChange = now, now
		if want > current {
			for i := current; i < want; i++ {
				readyAt = append(readyAt, now.Add(w.StartupDelay))
			}
		} else {
			// remove not-yet-ready pods first, then the newest
			for i := current; i > want; i-- {
				readyAt = readyAt[:len(readyAt)-1]
			}
		}
	}
	secs := sc.Duration.Seconds()
	res.AvgReplicas = sumReplicas / secs
	if demandTotal > 0 {
		res.UnmetDemandPct = 100 * unmet / demandTotal
	}
	res.SaturatedSeconds, res.AboveCeilingSecs, res.LongestSatSecs = sat, ceil, longest
	return res
}

type sample struct {
	t time.Time
	n int32
}

// costWindow mimics a KubeCost allocation over the last costWin: the run-rate
// is the window-average replica count times the price, and it is "stable" only
// if no scale action happened inside the window.
func costWindow(h []sample, now, lastChange time.Time, price float64) (float64, bool) {
	from := now.Add(-costWin)
	var sum float64
	var n int
	for i := len(h) - 1; i >= 0 && !h[i].t.Before(from); i-- {
		sum += float64(h[i].n)
		n++
	}
	if n == 0 {
		return 0, false
	}
	return sum / float64(n) * price, !lastChange.After(from)
}

func clampI(v, lo, hi int32) int32 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ---------------------------------------------------------------------------
// Native HorizontalPodAutoscaler (autoscaling/v2) baseline.

// HPA reproduces the documented algorithm: desired = ceil(current * util/target)
// unless within tolerance; scale-down uses the highest recommendation of the
// stabilisation window; scale-up is limited per period by selectPolicy Max of
// {+4 pods, +100%} (the Kubernetes defaults).
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

func (h *HPA) Name() string { return h.Label }

func (h *HPA) Decide(now time.Time, current, ready int32, util float64, _ float64, _ bool, _ time.Time) int32 {
	ratio := util / h.Target
	desired := current
	if math.Abs(ratio-1) > h.Tolerance {
		desired = int32(math.Ceil(float64(ready) * ratio))
	}
	desired = clampI(desired, h.Min, h.Max)
	h.hist = append(h.hist, rec{now, desired})
	// drop history older than the window
	cut := now.Add(-h.DownWindow)
	keep := h.hist[:0]
	for _, r := range h.hist {
		if !r.t.Before(cut) {
			keep = append(keep, r)
		}
	}
	h.hist = keep
	if desired < current {
		hi := desired
		for _, r := range h.hist {
			if r.n > hi {
				hi = r.n
			}
		}
		desired = minI(current, hi)
	}
	if desired > current {
		limit := current + maxI(4, current)
		desired = minI(desired, limit)
	}
	return clampI(desired, h.Min, h.Max)
}

// ---------------------------------------------------------------------------
// KubeCostAutoscaler: the real decision engine, driven by the simulator.

// KCA drives policy.Evaluate. It keeps its own persisted-history equivalent.
type KCA struct {
	Label string
	Cfg   policy.Config
	// UseCost passes the (windowed) cost observation to the engine; without it
	// the engine runs demand-only.
	UseCost bool
	hist    []policy.Recommendation
}

func (k *KCA) Name() string { return k.Label }

func (k *KCA) Decide(now time.Time, current, ready int32, util float64, hourly float64, stable bool, lastScale time.Time) int32 {
	in := policy.Input{
		Now: now, Current: current, Ready: ready,
		Demand:  &policy.Demand{Utilization: util},
		Infra:   infra.Summary{OnDemand: ready},
		History: k.hist, LastScale: lastScale,
	}
	if k.UseCost {
		v, err := money.FromFloat(hourly)
		if err == nil {
			in.Cost = &policy.Cost{Hourly: v, Stable: stable}
		}
	}
	d := policy.Evaluate(k.Cfg, in)
	k.hist = d.History
	if d.Type == policy.ScaleUp || d.Type == policy.ScaleDown {
		return d.Proposed
	}
	return current
}

func minI(a, b int32) int32 {
	if a < b {
		return a
	}
	return b
}
func maxI(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}
