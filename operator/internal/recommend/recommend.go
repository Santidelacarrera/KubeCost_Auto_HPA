// Package recommend computes explainable, reproducible CPU/memory requests recommendations.
//
// It is pure: no I/O, no clock (the caller passes the window), no cluster access, no write path of any kind.
// See docs/adr/0001-read-only-rightsizing-recommender.md and docs/RECOMMENDER.md.
//
// The algorithm is a starting point that has NOT been validated against real workloads; every number it
// uses is configuration and is echoed in the output so a recommendation can be reproduced by hand.
package recommend

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"time"
)

// Action is what the recommender proposes for one resource.
type Action string

const (
	NoChange Action = "NoChange"
	Increase Action = "Increase"
	Decrease Action = "Decrease"
	// Abstain means the data (or the situation) does not allow any recommendation, in either direction.
	Abstain Action = "Abstain"
)

// Confidence is a coarse, documented grade (see grade).
type Confidence string

const (
	ConfHigh Confidence = "High"
	ConfMed  Confidence = "Medium"
	ConfLow  Confidence = "Low"
)

// Resources are CPU in cores and memory in bytes. Zero means "not set".
type Resources struct {
	CPURequest, CPULimit float64
	MemRequest, MemLimit float64
}

// Input is everything the algorithm may look at.
type Input struct {
	Cluster, Namespace, Workload, Kind, Container string

	WindowStart, WindowEnd time.Time
	// CPU usage in cores and memory working set in bytes, one value per (pod, step).
	CPUSamples, MemSamples []float64
	// CPUSeries optionally keeps the CPU samples as one time-ordered slice per pod. It enables the non-stationarity check;
	// without it only the window-length caveat applies.
	CPUSeries [][]float64

	// ThrottledRatio is throttled CFS periods / total periods over the window; ThrottleKnown says whether it was measured.
	ThrottledRatio float64
	ThrottleKnown  bool
	OOMKills       int
	Restarts       int
	// SLOBreached is only meaningful when SLOKnown.
	SLOBreached, SLOKnown bool

	Excluded bool // opted out by label/annotation
	// HPAOnCPU: an HPA scales this workload on CPU utilisation, so a CPU request change shifts its target.
	HPAOnCPU bool
	// VPAAuto: a VPA in Auto/Recreate mode owns these requests.
	VPAAuto bool

	Current Resources
}

// Config holds every tunable. Zero values are replaced by Defaults().
type Config struct {
	MinSamples int
	MinWindow  time.Duration
	// MaxStale is how old WindowEnd may be relative to Now.
	MaxStale time.Duration
	Now      time.Time

	CPUPercentile, MemPercentile float64 // in (0,1]
	CPUHeadroom, MemHeadroom     float64 // fraction added on top of the percentile

	MinCPU, MaxCPU float64
	MinMem, MaxMem float64

	// ThrottleVeto: throttled ratio at or above this blocks CPU decreases.
	ThrottleVeto float64
	// MaxDecreaseFraction caps one step down (0.5 = never recommend less than half of the current request).
	MaxDecreaseFraction float64
	// Tolerance: relative differences below this are reported as NoChange.
	Tolerance float64

	// Prices for the potential-saving estimate; zero disables it. These are inputs, never looked up.
	CPUPricePerCoreHour, MemPricePerGiBHour float64
}

// Defaults returns the documented defaults. They are untuned starting values.
func Defaults() Config {
	return Config{
		MinSamples: 200, MinWindow: 24 * time.Hour, MaxStale: 15 * time.Minute,
		CPUPercentile: 0.95, MemPercentile: 0.99, CPUHeadroom: 0.15, MemHeadroom: 0.20,
		MinCPU: 0.010, MaxCPU: 64, MinMem: 32 << 20, MaxMem: 256 << 30,
		ThrottleVeto: 0.05, MaxDecreaseFraction: 0.5, Tolerance: 0.10,
	}
}

func (c Config) withDefaults() Config {
	d := Defaults()
	if c.MinSamples == 0 {
		c.MinSamples = d.MinSamples
	}
	if c.MinWindow == 0 {
		c.MinWindow = d.MinWindow
	}
	if c.MaxStale == 0 {
		c.MaxStale = d.MaxStale
	}
	if c.CPUPercentile == 0 {
		c.CPUPercentile = d.CPUPercentile
	}
	if c.MemPercentile == 0 {
		c.MemPercentile = d.MemPercentile
	}
	if c.CPUHeadroom == 0 {
		c.CPUHeadroom = d.CPUHeadroom
	}
	if c.MemHeadroom == 0 {
		c.MemHeadroom = d.MemHeadroom
	}
	if c.MinCPU == 0 {
		c.MinCPU = d.MinCPU
	}
	if c.MaxCPU == 0 {
		c.MaxCPU = d.MaxCPU
	}
	if c.MinMem == 0 {
		c.MinMem = d.MinMem
	}
	if c.MaxMem == 0 {
		c.MaxMem = d.MaxMem
	}
	if c.ThrottleVeto == 0 {
		c.ThrottleVeto = d.ThrottleVeto
	}
	if c.MaxDecreaseFraction == 0 {
		c.MaxDecreaseFraction = d.MaxDecreaseFraction
	}
	if c.Tolerance == 0 {
		c.Tolerance = d.Tolerance
	}
	return c
}

// Validate rejects configurations that would silently produce nonsense.
func (c Config) Validate() error {
	c = c.withDefaults()
	switch {
	case c.CPUPercentile <= 0 || c.CPUPercentile > 1 || c.MemPercentile <= 0 || c.MemPercentile > 1:
		return fmt.Errorf("percentiles must be in (0,1]")
	case c.CPUHeadroom < 0 || c.MemHeadroom < 0:
		return fmt.Errorf("headroom must not be negative")
	case c.MinCPU > c.MaxCPU || c.MinMem > c.MaxMem:
		return fmt.Errorf("min bound exceeds max bound")
	case c.MaxDecreaseFraction <= 0 || c.MaxDecreaseFraction >= 1:
		return fmt.Errorf("maxDecreaseFraction must be in (0,1)")
	case c.MinSamples < 1:
		return fmt.Errorf("minSamples must be positive")
	case c.Tolerance < 0 || c.Tolerance >= 1:
		return fmt.Errorf("tolerance must be in [0,1)")
	case c.CPUPricePerCoreHour < 0 || c.MemPricePerGiBHour < 0:
		return fmt.Errorf("prices must not be negative")
	}
	return nil
}

// Stats describes one sample series.
type Stats struct {
	N                  int
	P50, P90, P95, P99 float64
	Max, Mean, StdDev  float64
	// CV is StdDev/Mean (0 when Mean is 0).
	CV float64
}

// ResourceResult is the outcome for one resource (CPU or memory).
type ResourceResult struct {
	Action      Action
	Current     float64
	Recommended float64 // equals Current unless Action is Increase or Decrease
	Limit       float64 // recommended limit; limits are never lowered in v1
	Stats       Stats
	Formula     string
	Reasons     []string // why a change was blocked, capped or refused
	// PotentialMonthlyUSD is an ESTIMATE (requests delta x the supplied unit price x 730 h); 0 unless Decrease and priced.
	PotentialMonthlyUSD float64
}

// Recommendation is the full, self-describing record.
type Recommendation struct {
	ID                                            string
	Cluster, Namespace, Workload, Kind, Container string
	WindowStart, WindowEnd                        time.Time
	Samples                                       int
	Confidence                                    Confidence
	CPU, Memory                                   ResourceResult
	Risks                                         []string
	// Conditions to meet before any application (informational; applying is out of scope for this package).
	Conditions  []string
	Config      Config
	GeneratedAt time.Time
	// Abstained is true when neither resource may be recommended; Reasons explains.
	Abstained bool
	// Additional holds the other containers of the SAME pod template (sidecars), changed in the same rollout and rolled back
	// together. Empty for a single-container record, so records written before this field existed read unchanged. See Combine.
	Additional []ContainerResult `json:",omitempty"`
}

// ContainerResult is the recommendation for one more container of the record's pod template.
type ContainerResult struct {
	Container   string
	Samples     int
	Confidence  Confidence
	Abstained   bool
	CPU, Memory ResourceResult
	Risks       []string `json:",omitempty"`
}

// Percentile uses linear interpolation between closest ranks on a sorted copy. NaN/Inf/negative values are ignored.
func Percentile(xs []float64, p float64) float64 {
	s := clean(xs)
	if len(s) == 0 {
		return 0
	}
	if p <= 0 {
		return s[0]
	}
	if p >= 1 {
		return s[len(s)-1]
	}
	pos := p * float64(len(s)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	return s[lo] + (s[hi]-s[lo])*(pos-float64(lo))
}

func clean(xs []float64) []float64 {
	s := make([]float64, 0, len(xs))
	for _, x := range xs {
		if math.IsNaN(x) || math.IsInf(x, 0) || x < 0 {
			continue
		}
		s = append(s, x)
	}
	sort.Float64s(s)
	return s
}

// Describe computes Stats over the valid values of xs.
func Describe(xs []float64) Stats {
	s := clean(xs)
	st := Stats{N: len(s)}
	if st.N == 0 {
		return st
	}
	var sum float64
	for _, x := range s {
		sum += x
	}
	st.Mean = sum / float64(st.N)
	var sq float64
	for _, x := range s {
		sq += (x - st.Mean) * (x - st.Mean)
	}
	st.StdDev = math.Sqrt(sq / float64(st.N))
	if st.Mean > 0 {
		st.CV = st.StdDev / st.Mean
	}
	st.P50, st.P90, st.P95, st.P99 = Percentile(s, .5), Percentile(s, .9), Percentile(s, .95), Percentile(s, .99)
	st.Max = s[len(s)-1]
	return st
}

// Recommend produces one record. It never returns an error for bad data: it abstains and says why.
func Recommend(in Input, cfg Config) (Recommendation, error) {
	if err := cfg.Validate(); err != nil {
		return Recommendation{}, err
	}
	cfg = cfg.withDefaults()
	rec := Recommendation{
		Cluster: in.Cluster, Namespace: in.Namespace, Workload: in.Workload, Kind: in.Kind, Container: in.Container,
		WindowStart: in.WindowStart, WindowEnd: in.WindowEnd, Config: cfg, GeneratedAt: cfg.Now,
	}
	cpuS, memS := Describe(in.CPUSamples), Describe(in.MemSamples)
	rec.Samples = min(cpuS.N, memS.N)
	rec.ID = id(in)
	rec.CPU = ResourceResult{Action: Abstain, Current: in.Current.CPURequest, Recommended: in.Current.CPURequest, Limit: in.Current.CPULimit, Stats: cpuS}
	rec.Memory = ResourceResult{Action: Abstain, Current: in.Current.MemRequest, Recommended: in.Current.MemRequest, Limit: in.Current.MemLimit, Stats: memS}

	// ---- global data-quality gates: any failure abstains on both resources ----
	var gate []string
	switch {
	case in.Excluded:
		gate = append(gate, "workload is excluded (opt-out label/annotation)")
	case in.VPAAuto:
		gate = append(gate, "a VPA in Auto/Recreate mode owns these requests; two controllers must not own the same field")
	}
	if in.WindowEnd.Before(in.WindowStart) {
		gate = append(gate, "invalid window (end before start)")
	} else if w := in.WindowEnd.Sub(in.WindowStart); w < cfg.MinWindow {
		gate = append(gate, fmt.Sprintf("window %s is shorter than the minimum %s", w.Round(time.Minute), cfg.MinWindow))
	}
	if !cfg.Now.IsZero() && cfg.Now.Sub(in.WindowEnd) > cfg.MaxStale {
		gate = append(gate, fmt.Sprintf("metrics are stale: window ended %s before now (max %s)", cfg.Now.Sub(in.WindowEnd).Round(time.Second), cfg.MaxStale))
	}
	if cpuS.N < cfg.MinSamples || memS.N < cfg.MinSamples {
		gate = append(gate, fmt.Sprintf("insufficient samples: cpu=%d memory=%d, need %d", cpuS.N, memS.N, cfg.MinSamples))
	}
	if cpuS.N > 0 && cpuS.Max == 0 && memS.Max == 0 {
		gate = append(gate, "all samples are zero: the workload looks idle or the metrics are not being scraped")
	}
	rec.Confidence = grade(rec.Samples, cfg, cpuS, memS)
	if len(gate) > 0 {
		rec.Abstained = true
		rec.Confidence = ConfLow
		rec.CPU.Reasons, rec.Memory.Reasons = gate, gate
		rec.Risks = append(rec.Risks, "no recommendation: "+gate[0])
		return rec, nil
	}

	// ---- seasonality caveats: what the window cannot show ----
	if w := in.WindowEnd.Sub(in.WindowStart); w < 7*24*time.Hour {
		shown := w.Round(time.Hour)
		if w < 2*time.Hour {
			shown = w.Round(time.Minute)
		}
		rec.Risks = append(rec.Risks, fmt.Sprintf("the window (%s) is shorter than 7 days: weekly seasonality is not observed", shown))
		if rec.Confidence == ConfHigh {
			rec.Confidence = ConfMed
		}
	}
	// A ratio between tiny numbers is noise (3 millicores vs 9 millicores): only loads above five times the minimum CPU can be "not stationary".
	if lo, hi := QuarterSpread(in.CPUSeries); lo > 0 && hi/lo > 2 && hi >= 5*cfg.MinCPU {
		rec.Risks = append(rec.Risks, fmt.Sprintf("CPU load is not stationary: the p95 of the four quarters of the window ranges from %.3g to %.3g cores", lo, hi))
		rec.Confidence = ConfLow
	}

	// ---- shared vetoes that only block decreases ----
	var noDown []string
	if in.SLOKnown && in.SLOBreached {
		noDown = append(noDown, "SLO is breached: no reduction is allowed")
	}
	if in.Restarts > 0 {
		noDown = append(noDown, fmt.Sprintf("%d container restart(s) in the window", in.Restarts))
	}

	// ---- CPU ----
	cpuBase := Percentile(in.CPUSamples, cfg.CPUPercentile)
	cpuTarget := clamp(roundUp(cpuBase*(1+cfg.CPUHeadroom), 0.010), cfg.MinCPU, cfg.MaxCPU)
	cpuDown := append([]string{}, noDown...)
	if in.ThrottleKnown && in.ThrottledRatio >= cfg.ThrottleVeto {
		cpuDown = append(cpuDown, fmt.Sprintf("CPU throttling %.1f%% >= %.1f%%", in.ThrottledRatio*100, cfg.ThrottleVeto*100))
	}
	if !in.ThrottleKnown {
		cpuDown = append(cpuDown, "CPU throttling was not measured")
	}
	rec.CPU = decide(rec.CPU, cpuTarget, cpuDown, cfg, fmt.Sprintf("p%.0f x (1+%.2f) = %.4f x %.2f = %.4f cores (rounded up to 10m), clamped to [%.3f,%.3f]",
		cfg.CPUPercentile*100, cfg.CPUHeadroom, cpuBase, 1+cfg.CPUHeadroom, cpuTarget, cfg.MinCPU, cfg.MaxCPU), in.Current.CPULimit)
	if rec.CPU.Action == Decrease && cfg.CPUPricePerCoreHour > 0 {
		rec.CPU.PotentialMonthlyUSD = (rec.CPU.Current - rec.CPU.Recommended) * cfg.CPUPricePerCoreHour * 730
	}

	// ---- memory ----
	memBase := Percentile(in.MemSamples, cfg.MemPercentile)
	memTarget := clamp(roundUp(memBase*(1+cfg.MemHeadroom), 1<<20), cfg.MinMem, cfg.MaxMem)
	memDown := append([]string{}, noDown...)
	if in.OOMKills > 0 {
		memDown = append(memDown, fmt.Sprintf("%d OOMKilled event(s) in the window: the working set understates the need", in.OOMKills))
	}
	rec.Memory = decide(rec.Memory, memTarget, memDown, cfg, fmt.Sprintf("p%.0f x (1+%.2f) = %.0f x %.2f = %.0f bytes, clamped to [%.0f,%.0f]",
		cfg.MemPercentile*100, cfg.MemHeadroom, memBase, 1+cfg.MemHeadroom, memTarget, cfg.MinMem, cfg.MaxMem), in.Current.MemLimit)
	if rec.Memory.Action == Decrease && cfg.MemPricePerGiBHour > 0 {
		rec.Memory.PotentialMonthlyUSD = (rec.Memory.Current - rec.Memory.Recommended) / (1 << 30) * cfg.MemPricePerGiBHour * 730
	}

	// ---- risks and conditions ----
	if in.HPAOnCPU && (rec.CPU.Action == Decrease || rec.CPU.Action == Increase) {
		rec.Risks = append(rec.Risks, "an HPA scales this workload on CPU utilisation: changing the CPU request shifts its utilisation target and can trigger scale-out or scale-in")
		rec.Conditions = append(rec.Conditions, "do not change replicas and CPU requests in the same step; re-measure the HPA after the change")
	}
	if rec.Memory.Action == Decrease {
		rec.Risks = append(rec.Risks, "memory reductions can cause OOMKilled; apply behind a canary and watch restarts")
	}
	if rec.Confidence == ConfLow {
		rec.Risks = append(rec.Risks, "low confidence: highly variable load or a short history")
	}
	if rec.CPU.Action == Decrease || rec.Memory.Action == Decrease || rec.CPU.Action == Increase || rec.Memory.Action == Increase {
		rec.Conditions = append(rec.Conditions, "explicit policy authorisation, previous state recorded, rollback condition defined (not provided by this package)")
	}
	return rec, nil
}

// QuarterSpread splits every series in four time quarters, pools each quarter across series and returns the lowest and highest
// quarter p95. It returns 0, 0 when there is not enough data (fewer than 8 points in a series, or no series).
func QuarterSpread(series [][]float64) (lo, hi float64) {
	var q [4][]float64
	for _, s := range series {
		if len(s) < 8 {
			continue
		}
		n := len(s) / 4
		for i := 0; i < 4; i++ {
			end := (i + 1) * n
			if i == 3 {
				end = len(s)
			}
			q[i] = append(q[i], s[i*n:end]...)
		}
	}
	if len(q[0]) == 0 {
		return 0, 0
	}
	lo = math.Inf(1)
	for i := range q {
		p := Percentile(q[i], 0.95)
		lo, hi = math.Min(lo, p), math.Max(hi, p)
	}
	return lo, hi
}

// decide turns a target into an action, honouring vetoes, the step cap and the tolerance band.
func decide(r ResourceResult, target float64, downVetoes []string, cfg Config, formula string, curLimit float64) ResourceResult {
	r.Formula = formula
	cur := r.Current
	if cur <= 0 { // no request set: report what it should be, never a "decrease"
		r.Action, r.Recommended = Increase, target
		r.Reasons = append(r.Reasons, "no request is currently set")
		r.Limit = raiseLimit(curLimit, r.Recommended)
		return r
	}
	rel := (target - cur) / cur
	switch {
	case math.Abs(rel) < cfg.Tolerance:
		r.Action, r.Recommended = NoChange, cur
		r.Reasons = append(r.Reasons, fmt.Sprintf("difference %.1f%% is within the %.0f%% tolerance", rel*100, cfg.Tolerance*100))
	case rel > 0:
		r.Action, r.Recommended = Increase, target
	default:
		if len(downVetoes) > 0 {
			r.Action, r.Recommended = NoChange, cur
			r.Reasons = append(r.Reasons, "reduction refused: "+join(downVetoes))
			break
		}
		floor := cur * (1 - cfg.MaxDecreaseFraction)
		r.Action, r.Recommended = Decrease, target
		if target < floor {
			r.Recommended = floor
			r.Reasons = append(r.Reasons, fmt.Sprintf("reduction capped at %.0f%% of the current request per step", cfg.MaxDecreaseFraction*100))
		}
	}
	r.Limit = raiseLimit(curLimit, r.Recommended)
	return r
}

// raiseLimit never lowers a limit; it only lifts it when it would fall below the request. 0 stays 0 (unset).
func raiseLimit(limit, req float64) float64 {
	if limit > 0 && limit < req {
		return req
	}
	return limit
}

func grade(n int, cfg Config, cpu, mem Stats) Confidence {
	// The coefficient of variation of a quantity far below its configured floor is noise (a 1 MB working set against a 32 MiB minimum):
	// the recommendation is the floor either way, so the variability says nothing about how safe it is.
	cv := 0.0
	if cpu.P95 >= 5*cfg.MinCPU {
		cv = math.Max(cv, cpu.CV)
	}
	if mem.P99 >= cfg.MinMem {
		cv = math.Max(cv, mem.CV)
	}
	switch {
	case n >= 3*cfg.MinSamples && cv < 0.5:
		return ConfHigh
	case n >= cfg.MinSamples && cv < 1.0:
		return ConfMed
	}
	return ConfLow
}

func clamp(v, lo, hi float64) float64 { return math.Min(math.Max(v, lo), hi) }

func roundUp(v, unit float64) float64 { return math.Ceil(v/unit-1e-9) * unit }

func join(xs []string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += "; "
		}
		out += x
	}
	return out
}

func id(in Input) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%s|%s|%d", in.Cluster, in.Namespace, in.Kind, in.Workload, in.Container, in.WindowEnd.Unix())))
	return hex.EncodeToString(h[:8])
}
