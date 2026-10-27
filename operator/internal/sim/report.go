package sim

import (
	"fmt"
	"strings"
)

// Acceptance criteria. They were fixed BEFORE the results were generated and are not tuned per
// result. A scaler is unacceptable if, on ANY scenario, it is worse than Baseline (the HPA with the
// same stabilisation window, on the same trace) by more than these margins. Cost is never allowed
// to excuse a breach: acceptability is decided first, cost is compared only among acceptable ones.
const (
	MaxExtraUnmetPct       = 0.25 // percentage points of demand left unserved
	MaxExtraSaturatedSecs  = 120.0
	MaxExtraLongestSatSecs = 60.0
	MaxExtraReversals      = 2
)

// Violation describes one breach of the criteria.
type Violation struct {
	Scenario, What string
}

// Judge compares one result with the baseline on the same scenario.
func Judge(r, base Result) []Violation {
	var v []Violation
	add := func(what string) { v = append(v, Violation{r.Scenario, what}) }
	if d := r.UnmetDemandPct - base.UnmetDemandPct; d > MaxExtraUnmetPct {
		add(fmt.Sprintf("unmet demand +%.2f pp", d))
	}
	if d := r.SaturatedSeconds - base.SaturatedSeconds; d > MaxExtraSaturatedSecs {
		add(fmt.Sprintf("saturated +%.0f s", d))
	}
	if d := r.LongestSaturationSecs - base.LongestSaturationSecs; d > MaxExtraLongestSatSecs {
		add(fmt.Sprintf("longest saturation +%.0f s", d))
	}
	if d := r.Reversals - base.Reversals; d > MaxExtraReversals {
		add(fmt.Sprintf("reversals +%d", d))
	}
	return v
}

// Report runs every scaler on every scenario.
func Report(w Workload) (map[string][]Result, []string, []string) {
	byScenario := map[string][]Result{}
	var scenarios, scalers []string
	for _, sc := range Scenarios() {
		scenarios = append(scenarios, sc.Name)
		for _, s := range Scalers(w) {
			if len(scenarios) == 1 {
				scalers = append(scalers, s.Name())
			}
			byScenario[sc.Name] = append(byScenario[sc.Name], Run(w, sc, s))
		}
	}
	return byScenario, scenarios, scalers
}

// Markdown renders the full, deterministic result set. It is embedded in docs/BENCHMARKS.md and a
// test fails if the document and the simulator disagree.
func Markdown() string {
	w := DefaultWorkload()
	by, scenarios, scalers := Report(w)
	var b strings.Builder
	notes := map[string]string{}
	for _, sc := range Scenarios() {
		notes[sc.Name] = sc.Note
	}
	fmt.Fprintf(&b, "Workload: %.0f rps per ready pod, %s startup, $%.2f per replica-hour, replicas %d-%d, warm start. SYNTHETIC.\n\n",
		w.PerPodRPS, w.StartupDelay, w.PricePerHour, w.MinReplicas, w.MaxReplicas)

	for _, name := range scenarios {
		fmt.Fprintf(&b, "#### %s\n\n%s.\n\n", name, notes[name])
		b.WriteString("| Scaler | Cost USD | vs baseline | Avg repl. | Max repl. | Actions | Reversals | Unmet % | Saturated s | >90% util s | Longest sat. s | Missing repl.-s | Acted after last change s |\n")
		b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
		var base Result
		for _, r := range by[name] {
			if r.Scaler == Baseline {
				base = r
			}
		}
		for _, r := range by[name] {
			rel := "-"
			if r.Scaler != Baseline && base.CostUSD > 0 {
				rel = fmt.Sprintf("%+.1f%%", 100*(r.CostUSD-base.CostUSD)/base.CostUSD)
			}
			fmt.Fprintf(&b, "| %s | %.2f | %s | %.1f | %d | %d | %d | %.2f | %.0f | %.0f | %.0f | %.0f | %.0f |\n",
				r.Scaler, r.CostUSD, rel, r.AvgReplicas, r.MaxReplicas, r.ScaleActions, r.Reversals, r.UnmetDemandPct,
				r.SaturatedSeconds, r.AboveCeilingSeconds, r.LongestSaturationSecs, r.MissingReplicaSeconds, r.ActedAfterLastS)
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "#### Summary against the baseline (%s)\n\n", Baseline)
	fmt.Fprintf(&b, "Acceptance criteria, fixed before the results were produced: on every scenario a scaler must not exceed the baseline by more than %.2f pp unmet demand, %.0f s saturated, %.0f s longest saturation, or %d reversals.\n\n",
		MaxExtraUnmetPct, MaxExtraSaturatedSecs, MaxExtraLongestSatSecs, MaxExtraReversals)
	b.WriteString("| Scaler | Total cost USD | vs baseline | Scenarios cheaper (>2%) | Scenarios dearer (>2%) | Total actions | Total reversals | Extra s above 90% util (advisory) | Acceptable | Breaches |\n|---|---|---|---|---|---|---|---|---|---|\n")
	totals := map[string]float64{}
	for _, name := range scenarios {
		for _, r := range by[name] {
			totals[r.Scaler] += r.CostUSD
		}
	}
	for _, sName := range scalers {
		var cheaper, dearer, actions, revs int
		var extraAbove float64
		var viol []string
		for _, name := range scenarios {
			var r, base Result
			for _, x := range by[name] {
				if x.Scaler == sName {
					r = x
				}
				if x.Scaler == Baseline {
					base = x
				}
			}
			actions += r.ScaleActions
			extraAbove += r.AboveCeilingSeconds - base.AboveCeilingSeconds
			revs += r.Reversals
			if base.CostUSD > 0 {
				switch d := (r.CostUSD - base.CostUSD) / base.CostUSD; {
				case d < -0.02:
					cheaper++
				case d > 0.02:
					dearer++
				}
			}
			if sName != Baseline {
				for _, v := range Judge(r, base) {
					viol = append(viol, v.Scenario+": "+v.What)
				}
			}
		}
		acc, br := "yes", "-"
		if len(viol) > 0 {
			acc, br = "NO", strings.Join(viol, "; ")
		}
		if sName == Baseline {
			acc = "(baseline)"
		}
		rel := "-"
		if sName != Baseline {
			rel = fmt.Sprintf("%+.1f%%", 100*(totals[sName]-totals[Baseline])/totals[Baseline])
		}
		fmt.Fprintf(&b, "| %s | %.2f | %s | %d | %d | %d | %d | %+.0f | %s | %s |\n", sName, totals[sName], rel, cheaper, dearer, actions, revs, extraAbove, acc, br)
	}
	b.WriteString("\nThe \"extra s above 90% util\" column is advisory and has no threshold: it is the sum, over all scenarios, of the seconds a scaler spent above 90% utilisation beyond the baseline. It was added after the first results showed that the pre-declared criteria (which look at saturation, not at headroom) do not see a controller that rides the ceiling.\n\n")
	return b.String()
}
