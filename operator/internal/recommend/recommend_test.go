package recommend

import (
	"math"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

func series(n int, v float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = v
	}
	return out
}

// good is an over-provisioned workload with plenty of clean data: asks 2 cores / 4 GiB, uses 0.2 / 1 GiB.
func good() Input {
	return Input{
		Cluster: "c", Namespace: "shop", Workload: "api", Kind: "Deployment", Container: "app",
		WindowStart: t0.Add(-48 * time.Hour), WindowEnd: t0,
		CPUSamples: series(600, 0.2), MemSamples: series(600, 1<<30),
		ThrottleKnown: true, SLOKnown: true,
		Current: Resources{CPURequest: 2, CPULimit: 4, MemRequest: 4 << 30, MemLimit: 4 << 30},
	}
}

func cfg() Config { c := Defaults(); c.Now = t0; return c }

func mustRec(t *testing.T, in Input, c Config) Recommendation {
	t.Helper()
	r, err := Recommend(in, c)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestPercentileInterpolatesAndIgnoresGarbage(t *testing.T) {
	xs := []float64{1, 2, 3, 4, 5, math.NaN(), math.Inf(1), -3}
	if got := Percentile(xs, .5); got != 3 {
		t.Fatalf("p50=%v", got)
	}
	if got := Percentile(xs, .9); math.Abs(got-4.6) > 1e-9 {
		t.Fatalf("p90=%v", got)
	}
	if Percentile(nil, .5) != 0 || Percentile(xs, 0) != 1 || Percentile(xs, 1) != 5 {
		t.Fatal("edges")
	}
}

func TestOverprovisionedWorkloadGetsADocumentedReduction(t *testing.T) {
	c := cfg()
	c.CPUPricePerCoreHour, c.MemPricePerGiBHour = 0.03, 0.004
	r := mustRec(t, good(), c)
	if r.Abstained {
		t.Fatalf("%+v", r.CPU.Reasons)
	}
	// cpu target 0.23 is far below half of 2 cores, so the step is capped at 1.0
	if r.CPU.Action != Decrease || r.CPU.Recommended != 1.0 || len(r.CPU.Reasons) == 0 {
		t.Fatalf("cpu %+v", r.CPU)
	}
	// memory target 1.2 GiB is below half of 4 GiB, so the step is capped at 2 GiB
	if r.Memory.Action != Decrease || r.Memory.Recommended != 2<<30 {
		t.Fatalf("mem %+v", r.Memory)
	}
	if r.CPU.PotentialMonthlyUSD <= 0 || r.Memory.PotentialMonthlyUSD <= 0 {
		t.Fatal("potential saving not estimated")
	}
	if r.Confidence != ConfMed || r.Samples != 600 || r.CPU.Formula == "" || r.ID == "" {
		t.Fatalf("%+v", r)
	}
	if r.CPU.Limit != 4 || r.Memory.Limit != 4<<30 {
		t.Fatal("limits must not be lowered")
	}
}

func TestSmallReductionIsNotCapped(t *testing.T) {
	in := good()
	in.Current.CPURequest = 0.4 // target 0.23 -> 0.23 rounded up to 10m
	r := mustRec(t, in, cfg())
	if r.CPU.Action != Decrease || r.CPU.Recommended < 0.2 || r.CPU.Recommended > 0.3 {
		t.Fatalf("%+v", r.CPU)
	}
}

func TestToleranceBandMeansNoChange(t *testing.T) {
	in := good()
	in.Current.CPURequest = 0.25 // target ~0.23, inside 10%
	r := mustRec(t, in, cfg())
	if r.CPU.Action != NoChange || r.CPU.Recommended != 0.25 {
		t.Fatalf("%+v", r.CPU)
	}
}

func TestUnderprovisionedGetsAnIncreaseEvenWithVetoes(t *testing.T) {
	in := good()
	in.CPUSamples = series(600, 1.9)
	in.Current.CPURequest = 1
	in.OOMKills, in.SLOBreached = 2, true
	r := mustRec(t, in, cfg())
	if r.CPU.Action != Increase || r.CPU.Recommended <= 1 {
		t.Fatalf("%+v", r.CPU)
	}
}

func TestAbstainsOnBadData(t *testing.T) {
	cases := map[string]func(*Input, *Config){
		"samples":  func(i *Input, _ *Config) { i.CPUSamples = series(10, .2) },
		"memory":   func(i *Input, _ *Config) { i.MemSamples = nil },
		"window":   func(i *Input, _ *Config) { i.WindowStart = t0.Add(-time.Hour) },
		"stale":    func(i *Input, c *Config) { c.Now = t0.Add(time.Hour) },
		"excluded": func(i *Input, _ *Config) { i.Excluded = true },
		"vpa":      func(i *Input, _ *Config) { i.VPAAuto = true },
		"zeros":    func(i *Input, _ *Config) { i.CPUSamples, i.MemSamples = series(600, 0), series(600, 0) },
		"inverted": func(i *Input, _ *Config) { i.WindowEnd = i.WindowStart.Add(-time.Hour) },
	}
	for name, mut := range cases {
		in, c := good(), cfg()
		mut(&in, &c)
		r := mustRec(t, in, c)
		if !r.Abstained || r.CPU.Action != Abstain || r.Memory.Action != Abstain || r.CPU.Recommended != in.Current.CPURequest || len(r.CPU.Reasons) == 0 || r.Confidence != ConfLow {
			t.Errorf("%s: %+v", name, r)
		}
	}
}

func TestVetoesBlockOnlyTheRightDecreases(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		mut                    func(*Input)
		cpuBlocked, memBlocked bool
		reason                 string
	}{
		{"oom", func(i *Input) { i.OOMKills = 1 }, false, true, "OOMKilled"},
		{"throttle", func(i *Input) { i.ThrottledRatio = 0.2 }, true, false, "throttling"},
		{"throttle unknown", func(i *Input) { i.ThrottleKnown = false }, true, false, "not measured"},
		{"slo", func(i *Input) { i.SLOBreached = true }, true, true, "SLO"},
		{"restarts", func(i *Input) { i.Restarts = 3 }, true, true, "restart"},
	} {
		in := good()
		tc.mut(&in)
		r := mustRec(t, in, cfg())
		if r.Abstained {
			t.Fatalf("%s abstained", tc.name)
		}
		if (r.CPU.Action == NoChange) != tc.cpuBlocked || (r.Memory.Action == NoChange) != tc.memBlocked {
			t.Errorf("%s: cpu=%s mem=%s", tc.name, r.CPU.Action, r.Memory.Action)
		}
		blocked := r.CPU
		if tc.memBlocked && !tc.cpuBlocked {
			blocked = r.Memory
		}
		if !strings.Contains(strings.Join(blocked.Reasons, " "), tc.reason) {
			t.Errorf("%s: reasons %v", tc.name, blocked.Reasons)
		}
		if tc.cpuBlocked && r.CPU.Recommended != in.Current.CPURequest {
			t.Errorf("%s: a blocked recommendation changed the value", tc.name)
		}
	}
}

func TestSLONotKnownDoesNotBlock(t *testing.T) {
	in := good()
	in.SLOKnown, in.SLOBreached = false, true // a stale flag with SLOKnown=false must be ignored
	if r := mustRec(t, in, cfg()); r.CPU.Action != Decrease {
		t.Fatalf("%+v", r.CPU)
	}
}

func TestHPAOnCPUAddsRiskAndCondition(t *testing.T) {
	in := good()
	in.HPAOnCPU = true
	r := mustRec(t, in, cfg())
	if !strings.Contains(strings.Join(r.Risks, " "), "HPA") || !strings.Contains(strings.Join(r.Conditions, " "), "replicas") {
		t.Fatalf("%v %v", r.Risks, r.Conditions)
	}
}

func TestMissingRequestIsAnIncreaseNeverADecrease(t *testing.T) {
	in := good()
	in.Current = Resources{}
	r := mustRec(t, in, cfg())
	if r.CPU.Action != Increase || r.Memory.Action != Increase || r.CPU.Recommended <= 0 {
		t.Fatalf("%+v", r)
	}
	if r.CPU.Limit != 0 {
		t.Fatal("an unset limit must stay unset")
	}
}

func TestLimitIsRaisedWhenBelowNewRequest(t *testing.T) {
	in := good()
	in.CPUSamples = series(600, 1.9)
	in.Current.CPURequest, in.Current.CPULimit = 1, 1.2
	r := mustRec(t, in, cfg())
	if r.CPU.Limit < r.CPU.Recommended {
		t.Fatalf("limit %v below request %v", r.CPU.Limit, r.CPU.Recommended)
	}
}

func TestBoundsAreHonoured(t *testing.T) {
	in := good()
	in.CPUSamples = series(600, 0.001)
	in.Current.CPURequest = 0.05
	c := cfg()
	c.MinCPU = 0.04
	if r := mustRec(t, in, c); r.CPU.Recommended < 0.04 {
		t.Fatalf("below min: %+v", r.CPU)
	}
	in.CPUSamples = series(600, 100)
	c.MaxCPU = 8
	if r := mustRec(t, in, c); r.CPU.Recommended > 8 {
		t.Fatalf("above max: %+v", r.CPU)
	}
}

func TestConfidenceDropsWithVariability(t *testing.T) {
	in := good()
	for i := range in.CPUSamples {
		if i%2 == 0 {
			in.CPUSamples[i] = 0.01
		} else {
			in.CPUSamples[i] = 2
		}
	}
	if r := mustRec(t, in, cfg()); r.Confidence == ConfHigh {
		t.Fatalf("a bimodal series must not be high confidence: %s", r.Confidence)
	}
}

func TestDeterministicAndIDTracksWindow(t *testing.T) {
	a, b := mustRec(t, good(), cfg()), mustRec(t, good(), cfg())
	if a.ID != b.ID || a.CPU.Recommended != b.CPU.Recommended || a.Memory.Recommended != b.Memory.Recommended {
		t.Fatal("same input must give the same record")
	}
	in := good()
	in.WindowEnd = in.WindowEnd.Add(time.Minute)
	if mustRec(t, in, cfg()).ID == a.ID {
		t.Fatal("ID must change with the window")
	}
}

func TestInvalidConfigIsRejected(t *testing.T) {
	for _, c := range []Config{
		{CPUPercentile: 1.5}, {CPUHeadroom: -1}, {MinCPU: 10, MaxCPU: 1}, {MaxDecreaseFraction: 1}, {Tolerance: 1}, {CPUPricePerCoreHour: -1},
	} {
		if _, err := Recommend(good(), c); err == nil {
			t.Errorf("accepted %+v", c)
		}
	}
}

func TestNeverNegativeOrNaN(t *testing.T) {
	in := good()
	in.CPUSamples = append(series(600, 0.2), math.NaN(), math.Inf(1), -5)
	r := mustRec(t, in, cfg())
	for _, v := range []float64{r.CPU.Recommended, r.Memory.Recommended, r.CPU.Limit} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			t.Fatalf("bad value %v", v)
		}
	}
}

func TestShortWindowCapsConfidenceAndSaysWhy(t *testing.T) {
	r := mustRec(t, good(), cfg()) // 48 h
	if r.Confidence != ConfMed || !strings.Contains(strings.Join(r.Risks, " "), "weekly seasonality") {
		t.Fatalf("%s %v", r.Confidence, r.Risks)
	}
	in := good()
	in.WindowStart = in.WindowEnd.Add(-8 * 24 * time.Hour)
	if r := mustRec(t, in, cfg()); r.Confidence != ConfHigh || strings.Contains(strings.Join(r.Risks, " "), "weekly") {
		t.Fatalf("an 8-day clean window can be high confidence: %s %v", r.Confidence, r.Risks)
	}
}

func TestNonStationaryLoadLowersConfidence(t *testing.T) {
	in := good()
	in.WindowStart = in.WindowEnd.Add(-8 * 24 * time.Hour)
	quiet, busy := series(150, 0.1), series(150, 1.0)
	in.CPUSeries = [][]float64{append(append(append(append([]float64{}, quiet...), quiet...), busy...), busy...)}
	r := mustRec(t, in, cfg())
	if r.Confidence != ConfLow || !strings.Contains(strings.Join(r.Risks, " "), "not stationary") {
		t.Fatalf("%s %v", r.Confidence, r.Risks)
	}
	lo, hi := QuarterSpread(in.CPUSeries)
	if lo != 0.1 || hi != 1.0 {
		t.Fatalf("%v %v", lo, hi)
	}
	if lo, hi := QuarterSpread([][]float64{{1, 2}}); lo != 0 || hi != 0 {
		t.Fatal("too little data must give no spread")
	}
}

func TestTinyLoadsAreNeverCalledNonStationary(t *testing.T) {
	in := good()
	in.WindowStart = in.WindowEnd.Add(-8 * 24 * time.Hour)
	in.CPUSamples = series(600, 0.01)
	in.Current.CPURequest = 0.5
	quiet, busy := series(150, 0.003), series(150, 0.015) // 5x apart, but only 15 millicores at most
	in.CPUSeries = [][]float64{append(append(append(append([]float64{}, quiet...), quiet...), busy...), busy...)}
	r := mustRec(t, in, cfg())
	if strings.Contains(strings.Join(r.Risks, " "), "not stationary") || r.Confidence == ConfLow {
		t.Fatalf("noise on a 15 millicore load must not lower confidence: %s %v", r.Confidence, r.Risks)
	}
}

func TestShortWindowIsShownInMinutesNotZeroHours(t *testing.T) {
	in := good()
	in.WindowStart = in.WindowEnd.Add(-14 * time.Minute)
	c := cfg()
	c.MinWindow = 5 * time.Minute
	r := mustRec(t, in, c)
	if !strings.Contains(strings.Join(r.Risks, " "), "(14m0s)") {
		t.Fatalf("%v", r.Risks)
	}
}

func TestVariabilityBelowTheConfiguredFloorsDoesNotLowerConfidence(t *testing.T) {
	in := good()
	in.WindowStart = in.WindowEnd.Add(-8 * 24 * time.Hour)
	mem := make([]float64, 600)
	cpu := make([]float64, 600)
	for i := range mem {
		mem[i], cpu[i] = 400e3, 0.004 // 400 KB and 4 millicores: far below the 32 MiB and 50 mc floors
		if i%5 == 0 {
			mem[i], cpu[i] = 3e6, 0.02 // coefficient of variation well above 1
		}
	}
	in.MemSamples, in.CPUSamples = mem, cpu
	if r := mustRec(t, in, cfg()); r.Confidence != ConfHigh {
		t.Fatalf("noise below the floors must not lower confidence: %s", r.Confidence)
	}
	// the same variability on a quantity above its floor does lower it
	for i := range mem {
		mem[i] = 100e6
		if i%5 == 0 {
			mem[i] = 900e6
		}
	}
	in.MemSamples = mem
	if r := mustRec(t, in, cfg()); r.Confidence == ConfHigh {
		t.Fatal("real variability above the floor must still count")
	}
}
