// Package policy is the deterministic, side-effect-free scaling decision engine.
//
// It performs no I/O and reads no clock: everything it needs is in Input, so a
// decision can be reproduced exactly from the persisted status. See
// docs/KUBECOST_AUTOSCALER.md for the derivation of each step.
package policy

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/infra"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/money"
)

// Type is the kind of decision.
type Type string

const (
	ScaleUp        Type = "ScaleUp"
	ScaleDown      Type = "ScaleDown"
	Hold           Type = "Hold"
	Fallback       Type = "Fallback"
	InvalidMetrics Type = "InvalidMetrics"
)

// Reason is a stable, machine-readable cause (used in conditions and events).
type Reason string

const (
	ReasonWithinTolerance      Reason = "WithinTolerance"
	ReasonDemandScaleUp        Reason = "DemandScaleUp"
	ReasonSafetyScaleUp        Reason = "SafetyScaleUp"
	ReasonDemandScaleDown      Reason = "DemandScaleDown"
	ReasonCostScaleDown        Reason = "CostScaleDown"
	ReasonCostScaleUp          Reason = "CostPressureScaleUp"
	ReasonBoundsEnforced       Reason = "BoundsEnforced"
	ReasonScaleUpStabilizing   Reason = "ScaleUpStabilizing"
	ReasonDownStabilizing      Reason = "ScaleDownStabilizing"
	ReasonDownWarmup           Reason = "ScaleDownWarmup"
	ReasonCooldown             Reason = "Cooldown"
	ReasonPendingPods          Reason = "PendingPods"
	ReasonNotAllReady          Reason = "ReplicasNotReady"
	ReasonNoReadyReplicas      Reason = "NoReadyReplicas"
	ReasonDemandUnavailable    Reason = "DemandMetricsUnavailable"
	ReasonCostUnavailable      Reason = "CostMetricsUnavailable"
	ReasonCostEstimateUnstable Reason = "CostEstimateUnstable"
)

// Config is the resolved (defaulted and validated) policy.
type Config struct {
	MinReplicas, MaxReplicas, MinHealthy int32

	// Utilisation fractions of requested resources, e.g. 0.70.
	TargetUtil, Ceiling, CostUtil, Tolerance float64

	DownWindow, UpWindow, MinInterval time.Duration
	// MaxGap is the longest silence between recommendations before the
	// stabilisation history is considered discontinuous and restarted.
	MaxGap time.Duration

	// MaxHourly/TargetHourly are zero when unset.
	MaxHourly, TargetHourly money.USD
	Guardrail               bool

	SpotMult, OnDemandMult money.Ratio
	SpotBuffer             float64

	AllowSafetyScaleUp bool
}

// CostRequired reports whether any cost bound is configured. Without one the
// engine is purely demand-driven and does not need KubeCost.
func (c Config) CostRequired() bool { return c.MaxHourly > 0 || c.TargetHourly > 0 }

// Validate checks internal consistency.
func (c Config) Validate() error {
	switch {
	case c.MinReplicas < 1:
		return errors.New("minReplicas must be >= 1")
	case c.MaxReplicas < c.MinReplicas:
		return errors.New("maxReplicas must be >= minReplicas")
	case c.MinHealthy < 0 || c.MinHealthy > c.MaxReplicas:
		return errors.New("minHealthyReplicas must be within [0, maxReplicas]")
	case !(c.TargetUtil > 0 && c.TargetUtil <= c.CostUtil && c.CostUtil <= c.Ceiling && c.Ceiling <= 1):
		return errors.New("require 0 < targetUtilization <= costPressureUtilization <= ceilingUtilization <= 100")
	case c.Tolerance < 0 || c.Tolerance >= 0.5:
		return errors.New("tolerance must be in [0, 50)")
	case c.DownWindow < 0 || c.UpWindow < 0 || c.MinInterval < 0:
		return errors.New("windows and intervals must not be negative")
	case c.TargetHourly > 0 && c.MaxHourly > 0 && c.TargetHourly > c.MaxHourly:
		return errors.New("targetHourlyCostUSD must be <= maxHourlyCostUSD")
	case c.SpotMult.IsZero() || c.OnDemandMult.IsZero():
		return errors.New("price multipliers must be > 0")
	}
	return nil
}

// Recommendation is one entry of the persisted stabilisation history.
type Recommendation struct {
	Time     time.Time
	Replicas int32
}

// Demand is the validated resource-utilisation signal.
type Demand struct {
	// Utilization is the average usage / request across ready replicas
	// (1.0 == 100%), for the most constrained configured resource.
	Utilization float64
}

// Cost is a validated KubeCost observation.
type Cost struct {
	// Hourly is the allocated run-rate of the whole workload in USD/hour.
	Hourly money.USD
	// Stable is false when the replica count changed inside the sample window,
	// so Hourly / replicas is not a clean per-replica figure.
	Stable bool
}

// Input is everything one evaluation needs.
type Input struct {
	Now       time.Time
	Current   int32 // spec.replicas of the target
	Ready     int32 // status.readyReplicas of the target
	Demand    *Demand
	Cost      *Cost
	Infra     infra.Summary
	History   []Recommendation
	LastScale time.Time // zero if the operator never scaled this target
}

// Details exposes the intermediate values for status, logs and tests.
type Details struct {
	Utilization      float64
	CapacityFactor   float64
	DemandReplicas   int32 // comfortable target (TargetUtil)
	SafetyReplicas   int32 // minimum to stay at or below Ceiling
	CostReplicas     int32 // cost-pressure target (CostUtil); 0 if not applicable
	BudgetReplicas   int32 // largest count within MaxHourly; -1 if not applicable
	Observed         money.USD
	MarginalAdd      money.USD
	MarginalRemove   money.USD
	ProjectedHourly  money.USD // cost estimate at Candidate
	CostPressure     bool
	BudgetLimited    bool
	Emergency        bool
	HistoryDiscarded bool
}

// Decision is the engine output.
type Decision struct {
	Type     Type
	Reason   Reason
	Message  string
	Current  int32
	Proposed int32
	// Candidate is the recommendation before stabilisation and gating.
	Candidate int32
	// CostDriven is true when the cost policy lowered the recommendation.
	CostDriven bool
	Details    Details
	// History is the updated history to persist.
	History []Recommendation
}

// Evaluate computes the decision. It never returns a Proposed outside
// [MinReplicas, MaxReplicas] (except Hold at an out-of-bounds Current it
// immediately corrects).
func Evaluate(cfg Config, in Input) Decision {
	d := Decision{Type: Hold, Current: in.Current, Proposed: in.Current, Candidate: in.Current, Details: Details{BudgetReplicas: -1}}
	d.History = pruneHistory(cfg, in.Now, in.History)

	// 0. Hard bounds are enforced immediately, independent of every signal.
	if in.Current > cfg.MaxReplicas {
		return bounded(d, ScaleDown, cfg.MaxReplicas, "replicas exceed maxReplicas")
	}
	if in.Current < cfg.MinReplicas {
		return bounded(d, ScaleUp, cfg.MinReplicas, "replicas below minReplicas")
	}

	// 1. Without a demand signal nothing can be justified.
	if in.Demand == nil || !(in.Demand.Utilization >= 0) || math.IsInf(in.Demand.Utilization, 0) {
		d.Type, d.Reason, d.Message = InvalidMetrics, ReasonDemandUnavailable, "resource utilisation is unavailable; holding the current replica count"
		return d
	}
	if in.Ready <= 0 {
		d.Reason, d.Message = ReasonNoReadyReplicas, "no ready replicas; nothing to measure"
		return d
	}

	// 2. Capacity model.
	cf := infra.CapacityFactor(in.Infra, cfg.SpotBuffer)
	util := in.Demand.Utilization
	load := util * float64(in.Ready) // replica-equivalents at 100% of requests
	need := func(t float64) int32 { return ceilReplicas(load / (t * cf)) }
	rd, rs := need(cfg.TargetUtil), need(cfg.Ceiling)
	if math.Abs(util/cfg.TargetUtil-1) <= cfg.Tolerance {
		rd = in.Current // hysteresis around the target
	}
	d.Details.Utilization, d.Details.CapacityFactor, d.Details.DemandReplicas, d.Details.SafetyReplicas = util, cf, rd, rs
	emergency := rs > in.Current
	d.Details.Emergency = emergency

	// 3. Cost model.
	costOK := in.Cost != nil && in.Cost.Stable
	if cfg.CostRequired() && in.Cost == nil {
		return failClosed(cfg, d, in, rs, ReasonCostUnavailable, "cost data is unavailable or invalid")
	}
	if cfg.CostRequired() && !in.Cost.Stable {
		return failClosed(cfg, d, in, rs, ReasonCostEstimateUnstable, "replicas changed inside the cost window; per-replica cost is not reliable yet")
	}

	cand := rd
	if costOK && cfg.CostRequired() {
		est, ok := estimate(cfg, in)
		if !ok {
			return failClosed(cfg, d, in, rs, ReasonCostUnavailable, "per-replica cost cannot be estimated")
		}
		d.Details.Observed, d.Details.MarginalAdd, d.Details.MarginalRemove = in.Cost.Hourly, est.add, est.remove
		// Pressure is judged on what the comfortable replica count (rd) would cost, not on
		// today's spend. Today's spend depends on the replica count this controller picks, so
		// using it would be a feedback loop: running hotter lowers the cost below the target,
		// the pressure disappears, demand scales back up, the cost rises again, and so on.
		// Under constant demand rd is constant, so the decision is too.
		comfort := project(in, est, rd)
		pressure := (cfg.TargetHourly > 0 && comfort > cfg.TargetHourly) || (cfg.MaxHourly > 0 && comfort > cfg.MaxHourly)
		d.Details.CostPressure = pressure
		if pressure {
			rc := need(cfg.CostUtil)
			if math.Abs(util/cfg.CostUtil-1) <= cfg.Tolerance {
				rc = in.Current
			}
			d.Details.CostReplicas = rc
			if rc < cand {
				cand = rc
				d.CostDriven = true
			}
		}
		if cfg.Guardrail && cfg.MaxHourly > 0 {
			rb := budgetReplicas(cfg, in, est)
			d.Details.BudgetReplicas = rb
			// The budget may cap comfort headroom but never the replicas needed
			// to stay at or below the utilisation ceiling.
			if allowed := maxI32(rb, rs); cand > allowed {
				cand = allowed
				d.Details.BudgetLimited = true
			}
		}
	}
	// The safety replicas are a floor for every recommendation: no cost or
	// hysteresis rule may leave utilisation above the ceiling.
	cand = maxI32(cand, rs)
	cand = clamp(cand, maxI32(cfg.MinReplicas, cfg.MinHealthy), cfg.MaxReplicas)
	d.Candidate = cand
	if costOK && cfg.CostRequired() {
		est, _ := estimate(cfg, in)
		d.Details.ProjectedHourly = project(in, est, cand)
	}

	// 4. Record the recommendation (history is the stabilisation memory).
	d.History, d.Details.HistoryDiscarded = appendHistory(cfg, in.Now, d.History, cand)

	// 5. Direction, stabilisation and gates.
	switch {
	case cand == in.Current:
		d.Reason = ReasonWithinTolerance
		d.Message = fmt.Sprintf("utilisation %.0f%% needs %d replicas (current %d)", util*100, cand, in.Current)
		return d
	case cand > in.Current:
		return scaleUp(cfg, in, d, emergency)
	default:
		return scaleDown(cfg, in, d)
	}
}

func scaleUp(cfg Config, in Input, d Decision, emergency bool) Decision {
	safety := emergency && cfg.AllowSafetyScaleUp
	if in.Infra.Pending > 0 {
		d.Reason, d.Message = ReasonPendingPods, fmt.Sprintf("%d pod(s) are pending; adding replicas would not help until they are scheduled", in.Infra.Pending)
		return d
	}
	proposed := d.Candidate
	if !safety {
		low := minInWindow(d.History, in.Now, cfg.UpWindow)
		proposed = maxI32(in.Current, low)
		if proposed <= in.Current {
			d.Reason, d.Message = ReasonScaleUpStabilizing, "scale-up recommendation has not been stable for the scale-up window"
			return d
		}
	}
	if !safety && cooling(cfg, in) {
		d.Reason, d.Message = ReasonCooldown, "minimum scale action interval has not elapsed"
		return d
	}
	d.Type, d.Proposed = ScaleUp, proposed
	switch {
	case safety:
		d.Reason, d.Message = ReasonSafetyScaleUp, fmt.Sprintf("utilisation %.0f%% exceeds the %.0f%% ceiling", d.Details.Utilization*100, cfg.Ceiling*100)
	case d.Details.CostPressure:
		d.Reason, d.Message = ReasonCostScaleUp, fmt.Sprintf("running at %.0f%% (cost-pressure target %.0f%%)", d.Details.Utilization*100, cfg.CostUtil*100)
	default:
		d.Reason, d.Message = ReasonDemandScaleUp, fmt.Sprintf("utilisation %.0f%% needs %d replicas", d.Details.Utilization*100, proposed)
	}
	return d
}

func scaleDown(cfg Config, in Input, d Decision) Decision {
	if in.Infra.Pending > 0 {
		d.Reason, d.Message = ReasonPendingPods, "pending pods present; not removing capacity"
		return d
	}
	if in.Ready < in.Current || in.Infra.Terminating > 0 {
		d.Reason, d.Message = ReasonNotAllReady, fmt.Sprintf("only %d/%d replicas ready (or pods terminating); not removing capacity", in.Ready, in.Current)
		return d
	}
	if !historyCovers(cfg, in.Now, d.History) {
		d.Reason, d.Message = ReasonDownWarmup, "scale-down window is not yet covered by recorded recommendations"
		return d
	}
	high := maxInWindow(d.History, in.Now, cfg.DownWindow)
	proposed := minI32(in.Current, high)
	if proposed >= in.Current {
		d.Reason, d.Message = ReasonDownStabilizing, "a higher recommendation inside the scale-down window blocks removal"
		return d
	}
	if cooling(cfg, in) {
		d.Reason, d.Message = ReasonCooldown, "minimum scale action interval has not elapsed"
		return d
	}
	d.Type, d.Proposed = ScaleDown, proposed
	if d.CostDriven {
		d.Reason, d.Message = ReasonCostScaleDown, fmt.Sprintf("cost %s/h exceeds the target; running hotter (up to %.0f%%) is within safety limits", d.Details.Observed, cfg.CostUtil*100)
	} else {
		d.Reason, d.Message = ReasonDemandScaleDown, fmt.Sprintf("utilisation %.0f%% needs only %d replicas", d.Details.Utilization*100, proposed)
	}
	return d
}

// failClosed implements the Freeze semantics when cost data cannot be trusted:
// the replica count is preserved (no cost-driven or demand-driven scale-down)
// and only an explicitly authorised safety scale-up is allowed.
func failClosed(cfg Config, d Decision, in Input, safetyReplicas int32, reason Reason, msg string) Decision {
	d.Type, d.Reason, d.Message = InvalidMetrics, reason, msg+"; holding the current replica count"
	if d.Details.Emergency && cfg.AllowSafetyScaleUp && in.Infra.Pending == 0 {
		target := clamp(safetyReplicas, cfg.MinReplicas, cfg.MaxReplicas)
		if target > in.Current {
			d.Type, d.Reason = ScaleUp, ReasonSafetyScaleUp
			d.Proposed, d.Candidate = target, target
			d.Message = fmt.Sprintf("%s; safety scale-up to keep utilisation at or below %.0f%%", msg, cfg.Ceiling*100)
		}
	}
	return d
}

func bounded(d Decision, t Type, to int32, msg string) Decision {
	d.Type, d.Reason, d.Message, d.Proposed, d.Candidate = t, ReasonBoundsEnforced, msg, to, to
	return d
}

type estimateResult struct{ add, remove money.USD }

// estimate derives the marginal cost of adding/removing one replica from the
// observed (allocated, averaged) hourly cost. See the design doc: it is an
// estimate under a homogeneous-replica assumption, deliberately conservative
// (added replicas are priced On-Demand; removed ones at the cheapest class
// present).
func estimate(cfg Config, in Input) (estimateResult, bool) {
	if in.Ready <= 0 {
		return estimateResult{}, false
	}
	perReplica := in.Cost.Hourly.MulRat(1, int64(in.Ready))
	s := in.Infra
	placed := int64(s.Placed())
	var blended money.Ratio
	cheapest := cfg.OnDemandMult
	if placed == 0 {
		blended = cfg.OnDemandMult
	} else {
		sum := money.RatioFromInt(int64(s.Spot)).Mul(cfg.SpotMult).
			Add(money.RatioFromInt(int64(s.OnDemand) + int64(s.Unknown)).Mul(cfg.OnDemandMult))
		var ok bool
		blended, ok = sum.Quo(money.RatioFromInt(placed))
		if !ok {
			return estimateResult{}, false
		}
		if s.Spot > 0 && s.OnDemand+s.Unknown > 0 {
			cheapest = money.MinRatio(cfg.SpotMult, cfg.OnDemandMult)
		} else if s.Spot > 0 {
			cheapest = cfg.SpotMult
		}
	}
	addF, ok1 := cfg.OnDemandMult.Quo(blended)
	remF, ok2 := cheapest.Quo(blended)
	if !ok1 || !ok2 {
		return estimateResult{}, false
	}
	return estimateResult{add: addF.Apply(perReplica), remove: remF.Apply(perReplica)}, true
}

func project(in Input, e estimateResult, replicas int32) money.USD {
	obs := in.Cost.Hourly
	switch {
	case replicas > in.Current:
		return addUSD(obs, e.add.MulInt(int64(replicas-in.Current)))
	case replicas < in.Current:
		sub := e.remove.MulInt(int64(in.Current - replicas))
		if sub >= obs {
			return 0
		}
		return obs - sub
	}
	return obs
}

func addUSD(a, b money.USD) money.USD {
	if a > money.Max-b {
		return money.Max
	}
	return a + b
}

// budgetReplicas is the largest replica count whose projected hourly cost is
// within MaxHourly (may be below the current count when already over budget).
func budgetReplicas(cfg Config, in Input, e estimateResult) int32 {
	obs, max := in.Cost.Hourly, cfg.MaxHourly
	if obs <= max {
		if e.add == 0 {
			return cfg.MaxReplicas
		}
		extra := int64((max - obs) / e.add)
		return clamp32(int64(in.Current) + extra)
	}
	if e.remove == 0 {
		return in.Current
	}
	over := obs - max
	k := int64((over + e.remove - 1) / e.remove)
	return clamp32(int64(in.Current) - k)
}

func cooling(cfg Config, in Input) bool {
	return !in.LastScale.IsZero() && in.Now.Sub(in.LastScale) < cfg.MinInterval
}

func retention(cfg Config) time.Duration {
	w := cfg.DownWindow
	if cfg.UpWindow > w {
		w = cfg.UpWindow
	}
	return w + maxDur(cfg.MaxGap, time.Minute)
}

func pruneHistory(cfg Config, now time.Time, h []Recommendation) []Recommendation {
	out := make([]Recommendation, 0, len(h)+1)
	cut := now.Add(-retention(cfg))
	for _, r := range h {
		if !r.Time.Before(cut) && !r.Time.After(now) {
			out = append(out, r)
		}
	}
	return out
}

// appendHistory adds the recommendation, restarting the history when the
// controller was silent for longer than MaxGap (we cannot know what demand did
// in the gap, so stabilisation must warm up again).
func appendHistory(cfg Config, now time.Time, h []Recommendation, replicas int32) ([]Recommendation, bool) {
	discarded := false
	if n := len(h); n > 0 && cfg.MaxGap > 0 && now.Sub(h[n-1].Time) > cfg.MaxGap {
		h, discarded = h[:0], true
	}
	if n := len(h); n > 0 && h[n-1].Time.Equal(now) {
		h[n-1].Replicas = replicas
		return h, discarded
	}
	return append(h, Recommendation{Time: now, Replicas: replicas}), discarded
}

// historyCovers reports whether recommendations span the scale-down window.
func historyCovers(cfg Config, now time.Time, h []Recommendation) bool {
	if cfg.DownWindow == 0 {
		return true
	}
	return len(h) > 0 && !h[0].Time.After(now.Add(-cfg.DownWindow))
}

func maxInWindow(h []Recommendation, now time.Time, w time.Duration) int32 {
	var m int32 = -1
	for _, r := range h {
		if !r.Time.Before(now.Add(-w)) && r.Replicas > m {
			m = r.Replicas
		}
	}
	return m
}

func minInWindow(h []Recommendation, now time.Time, w time.Duration) int32 {
	m := int32(math.MaxInt32)
	for _, r := range h {
		if !r.Time.Before(now.Add(-w)) && r.Replicas < m {
			m = r.Replicas
		}
	}
	return m
}

func ceilReplicas(x float64) int32 {
	if math.IsNaN(x) || x < 0 {
		return 0
	}
	if x > 1e6 {
		return 1_000_000
	}
	return int32(math.Ceil(x - 1e-9))
}

func clamp(v, lo, hi int32) int32 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clamp32(v int64) int32 {
	if v < 0 {
		return 0
	}
	if v > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(v)
}

func maxI32(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}

func minI32(a, b int32) int32 {
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
