// Package sim is a deterministic, synthetic simulator used to compare the
// KubeCostAutoscaler decision engine (internal/policy) with the documented
// behaviour of a native Kubernetes HorizontalPodAutoscaler on IDENTICAL demand
// traces, initial conditions and disturbances.
//
// It is NOT a measurement of a real cluster. There is no latency model, no error
// model, no scheduler, no node provisioning and no real KubeCost: demand is a
// prescribed request rate, capacity is ready replicas times a fixed per-pod rate,
// and cost is replica-hours times a price schedule. Results are only meaningful
// relative to each other, under the assumptions in docs/BENCHMARKS.md.
package sim

import (
	"math"
	"time"
)

// Workload is the simulated application and its fixed assumptions.
type Workload struct {
	PerPodRPS    float64       // requests/s one ready pod serves at 100% of its request
	StartupDelay time.Duration // from replica creation to ready
	PricePerHour float64       // USD per replica-hour (default price schedule)
	MinReplicas  int32
	MaxReplicas  int32
	Initial      int32 // 0 = warm start from the first demand sample (identical for every scaler)
}

// Interval is a half-open span of simulated time [From, To).
type Interval struct{ From, To time.Duration }

func (i Interval) contains(t time.Duration) bool { return t >= i.From && t < i.To }

func anyContains(is []Interval, t time.Duration) bool {
	for _, i := range is {
		if i.contains(t) {
			return true
		}
	}
	return false
}

// ExternalWrite is another actor (a person, a script) setting the replica count once.
type ExternalWrite struct {
	At       time.Duration
	Replicas int32
}

// Scenario is a demand trace plus the disturbances applied to every scaler alike.
type Scenario struct {
	Name     string
	Note     string
	Duration time.Duration
	Demand   func(t time.Duration) float64 // requests per second
	// LastChange is when the demand (or cost) last changed, to measure how long after it the
	// controller kept acting.
	LastChange time.Duration

	Price      func(t time.Duration) float64 // USD per replica-hour; nil = Workload.PricePerHour
	MetricLag  time.Duration                 // the controller sees demand from this long ago
	CostOutage []Interval                    // KubeCost unreachable: no cost sample is available
	Down       []Interval                    // the controller process is down: no evaluations; restarts after
	Pause      []Interval                    // emergency pause: decisions are computed but never applied
	External   []ExternalWrite
}

// CostObs is the cost signal handed to a scaler (nil when KubeCost is unavailable).
type CostObs struct {
	Hourly float64 // window-average run-rate
	Stable bool    // no replica change inside the cost window
}

// Obs is one evaluation's input.
type Obs struct {
	Now       time.Time
	Current   int32
	Ready     int32
	Util      float64 // demand seen by the controller / ready capacity (lagged if MetricLag)
	Cost      *CostObs
	LastScale time.Time
}

// Scaler decides the next replica count once per Period.
type Scaler interface {
	Name() string
	Period() time.Duration
	CostWindow() time.Duration // 0 when the scaler does not use cost
	Decide(Obs) int32
}

// Restarter is implemented by scalers whose state is lost when their process restarts.
type Restarter interface{ Restart() }

// Result is the outcome of one scenario/scaler run. Infrastructure cost, capacity shortfall and
// stability are reported separately on purpose; nothing is folded into a single score.
type Result struct {
	Scenario, Scaler string

	// Estimated infrastructure cost.
	CostUSD     float64
	AvgReplicas float64
	MaxReplicas int32

	// Performance / saturation (proxies: there is no latency or error model).
	UnmetDemandPct        float64 // demand above ready capacity, as % of all demand
	SaturatedSeconds      float64 // time with demand > ready capacity
	AboveCeilingSeconds   float64 // time with utilisation above 90%
	LongestSaturationSecs float64 // longest continuous saturation episode
	// MissingReplicaSeconds is the integral of max(0, replicas needed to stay at or below 90%
	// utilisation - ready replicas): the capacity penalty, in replica-seconds, with no price attached.
	MissingReplicaSeconds float64

	// Stability.
	ScaleActions    int
	Reversals       int     // direction changes between consecutive scale actions
	ActedAfterLastS float64 // seconds between the last demand/cost change and the last scale action (0 if none after)
}

const (
	step    = 5 * time.Second
	ceiling = 0.9
)

type sample struct {
	n     int32
	price float64
}

// Run simulates one scaler on one scenario.
func Run(w Workload, sc Scenario, s Scaler) Result {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	price := func(el time.Duration) float64 {
		if sc.Price != nil {
			return sc.Price(el)
		}
		return w.PricePerHour
	}
	if w.Initial == 0 {
		w.Initial = clampI(int32(math.Ceil(sc.Demand(0)/(0.7*w.PerPodRPS))), w.MinReplicas, w.MaxReplicas)
	}
	readyAt := make([]time.Time, 0, int(w.MaxReplicas)+8)
	for i := int32(0); i < w.Initial; i++ {
		readyAt = append(readyAt, start.Add(-time.Hour))
	}
	res := Result{Scenario: sc.Name, Scaler: s.Name()}
	var (
		lastScale, lastAction time.Time
		lastDir               int
		sumReplicas, demand   float64
		unmet, sat, over, cur float64
		longest, missing      float64
		nextCtl               = start
		hist                  []sample // one entry per step, for the cost window
		lastChange            = start.Add(-24 * time.Hour)
		wasDown               bool
		extDone               = make([]bool, len(sc.External))
	)
	setReplicas := func(now time.Time, want int32) {
		cur := int32(len(readyAt))
		if want == cur {
			return
		}
		lastChange = now
		if want > cur {
			for i := cur; i < want; i++ {
				readyAt = append(readyAt, now.Add(w.StartupDelay))
			}
		} else {
			for i := cur; i > want; i-- { // not-yet-ready pods first, then the newest
				readyAt = readyAt[:len(readyAt)-1]
			}
		}
	}
	for el := time.Duration(0); el < sc.Duration; el += step {
		now := start.Add(el)
		for i, ew := range sc.External {
			if !extDone[i] && el >= ew.At {
				extDone[i] = true
				setReplicas(now, maxI(1, ew.Replicas))
			}
		}
		d := math.Max(0, sc.Demand(el))
		ready := int32(0)
		for _, r := range readyAt {
			if !r.After(now) {
				ready++
			}
		}
		capRPS := float64(ready) * w.PerPodRPS
		dt := step.Seconds()
		res.CostUSD += float64(len(readyAt)) * price(el) * dt / 3600
		sumReplicas += float64(len(readyAt)) * dt
		demand += d * dt
		if d > capRPS {
			unmet += (d - capRPS) * dt
			sat += dt
			cur += dt
			longest = math.Max(longest, cur)
		} else {
			cur = 0
		}
		if (capRPS > 0 && d/capRPS > ceiling) || (capRPS == 0 && d > 0) {
			over += dt
		}
		needCeil := math.Ceil(d / (ceiling * w.PerPodRPS))
		if short := needCeil - float64(ready); short > 0 {
			missing += short * dt
		}
		if n := int32(len(readyAt)); n > res.MaxReplicas {
			res.MaxReplicas = n
		}
		hist = append(hist, sample{int32(len(readyAt)), price(el)})

		down := anyContains(sc.Down, el)
		if down {
			wasDown = true
			continue
		}
		if wasDown { // the process came back: it has lost whatever it kept only in memory
			wasDown = false
			if r, ok := s.(Restarter); ok {
				r.Restart()
			}
			nextCtl = now
		}
		if now.Before(nextCtl) || ready == 0 {
			continue
		}
		nextCtl = now.Add(s.Period())
		current := int32(len(readyAt))
		seen := math.Max(0, sc.Demand(maxDur(0, el-sc.MetricLag)))
		util := math.Min(seen/(float64(ready)*w.PerPodRPS), 2.0)
		var cost *CostObs
		if cw := s.CostWindow(); cw > 0 && !anyContains(sc.CostOutage, el) {
			cost = costObs(hist, now, lastChange, cw)
		}
		want := clampI(s.Decide(Obs{Now: now, Current: current, Ready: ready, Util: util, Cost: cost, LastScale: lastScale}), w.MinReplicas, w.MaxReplicas)
		if want == current || anyContains(sc.Pause, el) { // a paused controller still decides, but writes nothing
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
		lastScale, lastAction = now, now
		setReplicas(now, want)
	}
	secs := sc.Duration.Seconds()
	res.AvgReplicas = sumReplicas / secs
	if demand > 0 {
		res.UnmetDemandPct = 100 * unmet / demand
	}
	res.SaturatedSeconds, res.AboveCeilingSeconds, res.LongestSaturationSecs, res.MissingReplicaSeconds = sat, over, longest, missing
	if !lastAction.IsZero() {
		if after := lastAction.Sub(start) - sc.LastChange; after > 0 {
			res.ActedAfterLastS = after.Seconds()
		}
	}
	return res
}

// costObs mimics a KubeCost allocation over the last window: the run-rate is the window-average
// of replicas x price, and it is "stable" only if no replica change happened inside the window.
func costObs(h []sample, now, lastChange time.Time, window time.Duration) *CostObs {
	n := int(window / step)
	if n > len(h) {
		n = len(h)
	}
	if n == 0 {
		return nil
	}
	var sum float64
	for _, s := range h[len(h)-n:] {
		sum += float64(s.n) * s.price
	}
	return &CostObs{Hourly: sum / float64(n), Stable: !lastChange.After(now.Add(-window))}
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

func maxI(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}

func minI(a, b int32) int32 {
	if a < b {
		return a
	}
	return b
}

func maxDur(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
