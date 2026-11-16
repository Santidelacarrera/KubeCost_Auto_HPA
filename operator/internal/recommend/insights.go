package recommend

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// Signals a record can carry. They are stable strings so reports and dashboards can count them.
const (
	SignalCPUGrowing   = "cpu-growing"   // daily p95 rises week over week; the target includes a forecast
	SignalCPUSpikes    = "cpu-spikes"    // rare samples far above the bulk of the data
	SignalMemSpikes    = "memory-spikes" // same for the working set
	SignalOOMRisk      = "oom-risk"      // working set peaked close to the memory limit
	SignalOOMKilled    = "oomkilled"     // the kernel already killed the container in the window
	SignalCPUThrottled = "cpu-throttled" // CFS throttling at or above the veto threshold
	SignalRestarts     = "restarts"      // container restarts in the window
	SignalNoMemLimit   = "no-memory-limit"
)

// Thresholds of the insights. They are constants, not Config fields: Config is stored in every record and must not grow for
// values nobody has had a reason to tune yet.
const (
	trendMinDays       = 4    // full days needed before a trend is fitted
	trendMinGrowthWeek = 0.10 // a rise of 10% of the median daily p95 per week counts as growth
	trendMaxProjection = 2.0  // the forecast never exceeds this multiple of the highest observed daily p95
	spikeMADs          = 6.0  // a sample this many scaled MADs above the median is a spike
	oomRiskLimitUse    = 0.90 // working-set peak at or above this share of the limit
)

// TrendHorizon is how far ahead the growth forecast looks.
const TrendHorizon = 14 * 24 * time.Hour

// Trend is the evidence behind a growth statement: one p95 per full day and a robust line through them.
type Trend struct {
	DailyP95 []float64
	// SlopePerDay is the Theil-Sen slope (median of the pairwise slopes), in the unit of the samples per day.
	SlopePerDay float64
	// GrowthPerWeek is SlopePerDay x 7 divided by the median daily p95: 0.10 means 10% a week.
	GrowthPerWeek float64
	// Projected is the fitted daily p95 at Horizon after the last day, capped (see trendMaxProjection). It is a forecast from a
	// straight line, not a seasonal model.
	Projected float64
	Horizon   time.Duration
}

// Spikes describes the samples that stand far above the rest.
type Spikes struct {
	Count     int
	Share     float64 // Count / number of samples
	Threshold float64 // median + spikeMADs x 1.4826 x MAD
	Max       float64
}

// Insights is what the recommender noticed besides the percentile: evidence for the reader, and the reason for any forecast in
// the target. Nil when there is nothing to say.
type Insights struct {
	Signals   []string `json:",omitempty"`
	CPUTrend  *Trend   `json:",omitempty"`
	CPUSpikes *Spikes  `json:",omitempty"`
	MemSpikes *Spikes  `json:",omitempty"`
	// MemLimitUse is the highest working set divided by the memory limit (0 when there is no limit).
	MemLimitUse float64 `json:",omitempty"`
}

func (i *Insights) add(signal string) {
	for _, s := range i.Signals {
		if s == signal {
			return
		}
	}
	i.Signals = append(i.Signals, signal)
}

func (i *Insights) empty() bool {
	// MemLimitUse alone is not worth a field in every record: it is kept as context when something else is reported
	return len(i.Signals) == 0 && i.CPUTrend == nil && i.CPUSpikes == nil && i.MemSpikes == nil
}

func median(sorted []float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// DailyTrend fits a robust line through the p95 of each full day of the window. times and values are parallel (one timestamp
// per sample). It returns nil when they are not, when fewer than trendMinDays full days have data, or when the data is flat
// at zero. A day counts as full when it has at least half as many samples as the best-covered day, so a day the workload was
// scaled to zero or the scrape was down is left out instead of being read as a fall.
//
// Days are 24 h slices from start, so a daily cycle lands whole in every slice and does not look like a trend. A weekly cycle
// can: with fewer than 14 days, a Monday-to-Friday rise is indistinguishable from growth, which is why the result is reported
// as a risk and only ever raises a target.
func DailyTrend(start time.Time, times []time.Time, values []float64, horizon time.Duration) *Trend {
	if len(times) == 0 || len(times) != len(values) {
		return nil
	}
	byDay := map[int][]float64{}
	most := 0
	for i, at := range times {
		v := values[i]
		if at.Before(start) || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			continue
		}
		d := int(at.Sub(start) / (24 * time.Hour))
		byDay[d] = append(byDay[d], v)
		most = max(most, len(byDay[d]))
	}
	var days []int
	for d, xs := range byDay {
		if 2*len(xs) >= most {
			days = append(days, d)
		}
	}
	if len(days) < trendMinDays {
		return nil
	}
	sort.Ints(days)
	t := &Trend{Horizon: horizon}
	for _, d := range days {
		t.DailyP95 = append(t.DailyP95, Percentile(byDay[d], 0.95))
	}
	var slopes []float64
	for i := range days {
		for j := i + 1; j < len(days); j++ {
			slopes = append(slopes, (t.DailyP95[j]-t.DailyP95[i])/float64(days[j]-days[i]))
		}
	}
	sort.Float64s(slopes)
	t.SlopePerDay = median(slopes)
	level := append([]float64(nil), t.DailyP95...)
	sort.Float64s(level)
	mid := median(level)
	if mid <= 0 {
		return nil
	}
	t.GrowthPerWeek = t.SlopePerDay * 7 / mid
	// intercept: median of the residuals, so one odd day does not move the line
	var res []float64
	for i, d := range days {
		res = append(res, t.DailyP95[i]-t.SlopePerDay*float64(d))
	}
	sort.Float64s(res)
	last := float64(days[len(days)-1])
	t.Projected = median(res) + t.SlopePerDay*(last+horizon.Hours()/24)
	t.Projected = math.Max(0, math.Min(t.Projected, trendMaxProjection*level[len(level)-1]))
	return t
}

// FindSpikes reports the samples more than spikeMADs scaled median absolute deviations above the median. It returns nil when
// there are none, or when more than 5% of the samples qualify: that is a second operating level (a batch phase, a daily peak),
// which the percentile already sees, not an anomaly.
func FindSpikes(xs []float64) *Spikes {
	c := clean(xs)
	if len(c) < 20 {
		return nil
	}
	sort.Float64s(c)
	med := median(c)
	dev := make([]float64, len(c))
	for i, v := range c {
		dev[i] = math.Abs(v - med)
	}
	sort.Float64s(dev)
	mad := median(dev)
	thr := med + spikeMADs*1.4826*mad
	if mad == 0 {
		thr = med * 2 // a perfectly flat series: anything at twice the level stands out
	}
	if thr <= 0 {
		return nil
	}
	n := len(c) - sort.SearchFloat64s(c, math.Nextafter(thr, math.Inf(1)))
	if n == 0 || float64(n)/float64(len(c)) > 0.05 {
		return nil
	}
	return &Spikes{Count: n, Share: float64(n) / float64(len(c)), Threshold: thr, Max: c[len(c)-1]}
}

// insights gathers the signals for one input. It returns them, the CPU forecast to fold into the target (0 when there is none)
// and the risks to print.
func insights(in Input, cfg Config, memS Stats) (*Insights, float64, []string) {
	out := &Insights{}
	var risks []string
	forecast := 0.0

	if tr := DailyTrend(in.WindowStart, in.CPUTimes, in.CPUSamples, TrendHorizon); tr != nil {
		out.CPUTrend = tr
		// growth in tiny absolute numbers is noise, as in the stationarity check
		if tr.GrowthPerWeek >= trendMinGrowthWeek && tr.Projected >= 5*cfg.MinCPU {
			out.add(SignalCPUGrowing)
			forecast = tr.Projected
			risks = append(risks, fmt.Sprintf("CPU demand is growing about %.0f%% per week (daily p95 over %d days: %.3g to %.3g cores): the forecast for %d days ahead is %.3g cores and the CPU target is sized for it; a straight line cannot tell growth from a weekly cycle on less than two weeks of data",
				tr.GrowthPerWeek*100, len(tr.DailyP95), tr.DailyP95[0], tr.DailyP95[len(tr.DailyP95)-1], int(TrendHorizon.Hours()/24), tr.Projected))
		}
	}
	if sp := FindSpikes(in.CPUSamples); sp != nil && sp.Max >= 5*cfg.MinCPU {
		out.CPUSpikes = sp
		out.add(SignalCPUSpikes)
		risks = append(risks, fmt.Sprintf("%d CPU sample(s) (%.2f%%) are spikes up to %.3g cores, far above the p%.0f of %.3g: bursts like these are served from the CPU limit, not the request, and are throttled without one",
			sp.Count, sp.Share*100, sp.Max, cfg.CPUPercentile*100, Percentile(in.CPUSamples, cfg.CPUPercentile)))
	}
	if sp := FindSpikes(in.MemSamples); sp != nil {
		out.MemSpikes = sp
		out.add(SignalMemSpikes)
		risks = append(risks, fmt.Sprintf("%d memory sample(s) (%.2f%%) are spikes up to %.0f MiB: memory is not compressible, so check they stay under the limit", sp.Count, sp.Share*100, sp.Max/(1<<20)))
	}
	switch {
	case in.Current.MemLimit > 0 && memS.Max > 0:
		out.MemLimitUse = memS.Max / in.Current.MemLimit
		if out.MemLimitUse >= oomRiskLimitUse {
			out.add(SignalOOMRisk)
			risks = append(risks, fmt.Sprintf("OOM risk: the working set peaked at %.0f%% of the memory limit (%.0f of %.0f MiB); raise the limit before anything else",
				out.MemLimitUse*100, memS.Max/(1<<20), in.Current.MemLimit/(1<<20)))
		}
	case in.Current.MemLimit == 0 && memS.Max > 0:
		out.add(SignalNoMemLimit)
	}
	if in.OOMKills > 0 {
		out.add(SignalOOMKilled)
	}
	if in.ThrottleKnown && in.ThrottledRatio >= cfg.ThrottleVeto {
		out.add(SignalCPUThrottled)
	}
	if in.Restarts > 0 {
		out.add(SignalRestarts)
	}
	if out.empty() {
		return nil, forecast, risks
	}
	return out, forecast, risks
}
