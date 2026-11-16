package recommend

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func has(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func signals(r Recommendation) []string {
	if r.Insights == nil {
		return nil
	}
	return r.Insights.Signals
}

// dailyCPU builds `days` days of hourly samples ending at t0. level(day) is that day's base load; every day has the same
// day/night shape (x0.5 at night, x1 by day) so a daily cycle is present in all of them.
func dailyCPU(days int, level func(day int) float64) (start time.Time, times []time.Time, values []float64) {
	start = t0.Add(-time.Duration(days) * 24 * time.Hour)
	for h := 0; h < days*24; h++ {
		at := start.Add(time.Duration(h) * time.Hour)
		shape := 0.5
		if hr := h % 24; hr >= 8 && hr < 20 {
			shape = 1
		}
		times = append(times, at)
		values = append(values, level(h/24)*shape)
	}
	return start, times, values
}

func trendInput(days int, level func(int) float64) Input {
	in := good()
	in.WindowStart, in.CPUTimes, in.CPUSamples = dailyCPU(days, level)
	in.MemSamples = series(len(in.CPUSamples), 1<<30)
	in.Current.CPURequest = 1.2 // close enough that the step cap (half of the current request) does not hide the target
	return in
}

func TestDailyCycleAloneIsNotATrend(t *testing.T) {
	in := trendInput(10, func(int) float64 { return 0.4 })
	tr := DailyTrend(in.WindowStart, in.CPUTimes, in.CPUSamples, TrendHorizon)
	if tr == nil || math.Abs(tr.GrowthPerWeek) > 1e-9 {
		t.Fatalf("a repeating day/night cycle has no trend: %+v", tr)
	}
	r := mustRec(t, in, cfg())
	if has(signals(r), SignalCPUGrowing) {
		t.Fatalf("flat load reported as growing: %v", r.Risks)
	}
	if !strings.HasPrefix(r.CPU.Formula, "p95 x") {
		t.Fatalf("the percentile, not a forecast, must drive the target: %s", r.CPU.Formula)
	}
}

func TestGrowthRaisesTheTargetToTheForecast(t *testing.T) {
	// +5% of the first day per day: 0.40 -> 0.58 over 10 days, about 25% a week
	in := trendInput(10, func(d int) float64 { return 0.4 * (1 + 0.05*float64(d)) })
	flat := trendInput(10, func(int) float64 { return 0.4 * (1 + 0.05*9) }) // the last day's level, with no growth

	r, base := mustRec(t, in, cfg()), mustRec(t, flat, cfg())
	if !has(signals(r), SignalCPUGrowing) || r.Insights.CPUTrend == nil {
		t.Fatalf("growth not detected: %+v", r.Insights)
	}
	tr := r.Insights.CPUTrend
	if len(tr.DailyP95) != 10 || tr.GrowthPerWeek < 0.2 || tr.GrowthPerWeek > 0.35 {
		t.Fatalf("trend evidence: %+v", tr)
	}
	// 14 days ahead of the last day (0.58): 0.58 + 14 x 0.02 = 0.86
	if math.Abs(tr.Projected-0.86) > 0.02 {
		t.Fatalf("projection %.3f, want about 0.86", tr.Projected)
	}
	if r.CPU.Recommended <= base.CPU.Recommended {
		t.Fatalf("a growing workload must get more than the same load without growth: %.3f vs %.3f", r.CPU.Recommended, base.CPU.Recommended)
	}
	if !strings.Contains(r.CPU.Formula, "forecast 14 days ahead") {
		t.Fatalf("the formula must say the forecast drove the target: %s", r.CPU.Formula)
	}
	found := false
	for _, x := range r.Risks {
		if strings.Contains(x, "growing about") && strings.Contains(x, "weekly cycle") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the risk must explain the growth and its limit: %v", r.Risks)
	}
}

func TestFallingLoadNeverLowersTheTarget(t *testing.T) {
	in := trendInput(10, func(d int) float64 { return 0.8 * (1 - 0.05*float64(d)) })
	r := mustRec(t, in, cfg())
	if has(signals(r), SignalCPUGrowing) || !strings.HasPrefix(r.CPU.Formula, "p95 x") {
		t.Fatalf("a falling trend must not be forecast: %s", r.CPU.Formula)
	}
	if r.Insights == nil || r.Insights.CPUTrend == nil || r.Insights.CPUTrend.GrowthPerWeek >= 0 {
		t.Fatalf("the falling trend is still evidence: %+v", r.Insights)
	}
}

func TestTrendNeedsFullDaysAndParallelTimestamps(t *testing.T) {
	in := trendInput(3, func(d int) float64 { return 0.4 * (1 + 0.5*float64(d)) })
	if DailyTrend(in.WindowStart, in.CPUTimes, in.CPUSamples, TrendHorizon) != nil {
		t.Fatal("three days are not enough for a trend")
	}
	in = trendInput(10, func(d int) float64 { return 0.4 * (1 + 0.05*float64(d)) })
	if DailyTrend(in.WindowStart, in.CPUTimes[:50], in.CPUSamples, TrendHorizon) != nil {
		t.Fatal("timestamps that do not match the samples must be refused")
	}
	if DailyTrend(in.WindowStart, nil, in.CPUSamples, TrendHorizon) != nil {
		t.Fatal("no timestamps, no trend")
	}
}

// A day with almost no samples (scaled to zero, scrape outage) is left out instead of being read as a collapse followed by growth.
func TestSparseDayIsNotReadAsGrowth(t *testing.T) {
	start, times, values := dailyCPU(8, func(int) float64 { return 0.4 })
	var tt []time.Time
	var vv []float64
	for i := range times {
		day := int(times[i].Sub(start) / (24 * time.Hour))
		if day == 0 && i%24 > 1 { // keep 2 of 24 samples of the first day, both at night level
			continue
		}
		tt, vv = append(tt, times[i]), append(vv, values[i])
	}
	tr := DailyTrend(start, tt, vv, TrendHorizon)
	if tr == nil || len(tr.DailyP95) != 7 || math.Abs(tr.GrowthPerWeek) > 1e-9 {
		t.Fatalf("%+v", tr)
	}
}

func TestOneOddDayDoesNotMakeATrend(t *testing.T) {
	in := trendInput(9, func(d int) float64 {
		if d == 8 {
			return 2.0 // one incident day at five times the load
		}
		return 0.4
	})
	tr := DailyTrend(in.WindowStart, in.CPUTimes, in.CPUSamples, TrendHorizon)
	if tr == nil || tr.GrowthPerWeek > 0.01 {
		t.Fatalf("the median slope must ignore one outlier day: %+v", tr)
	}
}

func TestProjectionIsCapped(t *testing.T) {
	// doubling every day would extrapolate absurdly; the forecast stops at twice the highest observed day
	in := trendInput(6, func(d int) float64 { return 0.1 * math.Pow(2, float64(d)) })
	tr := DailyTrend(in.WindowStart, in.CPUTimes, in.CPUSamples, TrendHorizon)
	top := 0.1 * math.Pow(2, 5)
	if tr == nil || tr.Projected > 2*top+1e-9 {
		t.Fatalf("projection %.2f exceeds the cap %.2f", tr.Projected, 2*top)
	}
}

func TestSpikesAreReportedWithEvidence(t *testing.T) {
	in := good()
	in.CPUSamples = series(600, 0.2)
	for i := 0; i < len(in.CPUSamples); i++ { // a little noise so the deviation is not zero
		in.CPUSamples[i] += 0.01 * float64(i%5)
	}
	in.CPUSamples[100], in.CPUSamples[400] = 1.6, 1.9
	in.Current.CPURequest = 0.6
	r := mustRec(t, in, cfg())
	if !has(signals(r), SignalCPUSpikes) {
		t.Fatalf("spikes not reported: %+v", r.Insights)
	}
	sp := r.Insights.CPUSpikes
	if sp.Count != 2 || sp.Max != 1.9 || sp.Threshold <= 0.24 || sp.Threshold >= 1.6 {
		t.Fatalf("%+v", sp)
	}
	// the percentile target is unchanged by two samples out of 600: the report says so instead of sizing for them
	if r.CPU.Recommended > 0.4 {
		t.Fatalf("two spikes must not drive the request: %.3f", r.CPU.Recommended)
	}
}

func TestASecondOperatingLevelIsNotAnAnomaly(t *testing.T) {
	xs := series(600, 0.2)
	for i := 0; i < 120; i++ { // 20% of the time at a higher level: a batch phase, which the percentile sees
		xs[i] = 1.5
	}
	if sp := FindSpikes(xs); sp != nil {
		t.Fatalf("%+v", sp)
	}
	if FindSpikes(series(600, 0.2)) != nil || FindSpikes(series(10, 0.2)) != nil || FindSpikes(series(600, 0)) != nil {
		t.Fatal("flat, short or all-zero data has no spikes")
	}
}

func TestWorkingSetNearTheLimitIsAnOOMRisk(t *testing.T) {
	in := good()
	in.MemSamples = series(600, 1<<30)
	in.MemSamples[10] = 3.8 * (1 << 30) // the limit is 4 GiB
	r := mustRec(t, in, cfg())
	if !has(signals(r), SignalOOMRisk) || r.Insights.MemLimitUse < 0.94 || r.Insights.MemLimitUse > 0.96 {
		t.Fatalf("%+v", r.Insights)
	}
	ok := false
	for _, x := range r.Risks {
		if strings.HasPrefix(x, "OOM risk:") {
			ok = true
		}
	}
	if !ok {
		t.Fatalf("%v", r.Risks)
	}
	// well below the limit: no signal
	if r := mustRec(t, good(), cfg()); has(signals(r), SignalOOMRisk) {
		t.Fatalf("1 GiB of 4 GiB is not a risk: %+v", r.Insights)
	}
}

func TestOperationalSignalsAreCarried(t *testing.T) {
	in := good()
	in.OOMKills, in.Restarts, in.ThrottledRatio = 1, 2, 0.30
	in.Current.MemLimit = 0
	r := mustRec(t, in, cfg())
	for _, s := range []string{SignalOOMKilled, SignalRestarts, SignalCPUThrottled, SignalNoMemLimit} {
		if !has(signals(r), s) {
			t.Errorf("missing %s in %v", s, signals(r))
		}
	}
}

// Records written before Insights existed must read unchanged, and a record with nothing to say must not grow a field.
func TestInsightsAreOmittedWhenEmptyAndOldRecordsStillParse(t *testing.T) {
	in := good()
	in.Current.MemLimit = 8 << 30
	r := mustRec(t, in, cfg())
	if r.Insights != nil {
		t.Fatalf("nothing to say, yet: %+v", r.Insights)
	}
	b, err := json.Marshal(r)
	if err != nil || strings.Contains(string(b), "Insights") {
		t.Fatalf("%v %s", err, b)
	}
	var back Recommendation
	if err := json.Unmarshal(b, &back); err != nil || back.ID != r.ID {
		t.Fatal(err)
	}
}
