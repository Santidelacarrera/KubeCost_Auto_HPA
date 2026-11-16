package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/capacity"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/cost"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/recommend"
)

// renderInputs are the JSON outputs of the other commands, each optional.
type renderInputs struct {
	Title    string
	Recs     []recommend.Recommendation
	Nodes    *capacity.Report
	Plan     *capacity.ConsolidationPlan
	Fleet    *FleetReport
	Bills    *reconcileOutput
	Now      time.Time
	Problems []string // inputs that could not be read: listed in the report, never silently dropped
}

type reconcileOutput struct {
	Level          cost.SavingsLevel
	MissingForNext []string
	Reconciliation cost.BillReconciliation
}

// md makes a value safe inside a Markdown table cell: values come from cluster objects and files, not from us.
func md(s string) string {
	s = strings.NewReplacer("|", "\\|", "\n", " ", "\r", " ", "<", "&lt;", ">", "&gt;", "`", "'").Replace(s)
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

func cores(v float64) string { return fmt.Sprintf("%.0fm", v*1000) }
func mib(v float64) string   { return fmt.Sprintf("%.0fMi", v/(1<<20)) }

func change(r recommend.ResourceResult, unit func(float64) string) string {
	switch r.Action {
	case recommend.Decrease, recommend.Increase:
		return fmt.Sprintf("%s: %s to %s", r.Action, unit(r.Current), unit(r.Recommended))
	}
	return string(r.Action)
}

// renderMarkdown writes one report a person can read. It adds no number of its own: every figure is copied from an input, and
// every saving carries the level the input gave it.
func renderMarkdown(w io.Writer, in renderInputs) {
	p := func(format string, a ...any) { fmt.Fprintf(w, format+"\n", a...) }
	title := in.Title
	if title == "" {
		title = "Rightsizing and capacity report"
	}
	p("# %s", md(title))
	p("")
	p("Generated %s by `rightsize render` from the outputs of `plan`, `nodes`, `fleet` and `reconcile`. Read-only: producing this report changed nothing.", in.Now.UTC().Format("2006-01-02 15:04 MST"))
	p("")
	for _, pr := range in.Problems {
		p("> **Input not included:** %s", md(pr))
	}
	if len(in.Problems) > 0 {
		p("")
	}

	if len(in.Recs) > 0 {
		p("## Workloads")
		p("")
		var est float64
		var down, up, hold, abst int
		p("| Workload | Container | CPU | Memory | Confidence | Estimated USD/month | Signals | Main risk |")
		p("|---|---|---|---|---|---|---|---|")
		for _, r := range in.Recs {
			c, m := r.PotentialMonthlyUSD()
			est += c + m
			switch {
			case r.Abstained:
				abst++
			case r.CPU.Action == recommend.Decrease || r.Memory.Action == recommend.Decrease:
				down++
			case r.CPU.Action == recommend.Increase || r.Memory.Action == recommend.Increase:
				up++
			default:
				hold++
			}
			var signals string
			if r.Insights != nil {
				signals = strings.Join(r.Insights.Signals, ", ")
			}
			risk := ""
			if len(r.Risks) > 0 {
				risk = r.Risks[0]
			}
			cpu, mem := change(r.CPU, cores), change(r.Memory, mib)
			if r.Abstained {
				cpu, mem = "no recommendation", "no recommendation"
			}
			p("| %s | %s | %s | %s | %s | %.2f | %s | %s |", md(r.Namespace+"/"+r.Kind+"/"+r.Workload), md(r.Container), cpu, mem, r.Confidence, c+m, md(signals), md(risk))
		}
		p("")
		p("%d to reduce, %d to increase, %d unchanged, %d without a recommendation.", down, up, hold, abst)
		p("")
		p("**Estimated** saving if every reduction were applied and the freed requests reclaimed: **%.2f USD/month**. Level: *estimated* (requests x the unit prices given to `plan`). Nothing was applied, and a requests reduction saves nothing until a node is released.", est)
		p("")
	}

	if in.Nodes != nil {
		p("## Nodes")
		p("")
		p("| Node | CPU requested | Memory requested | Pods | Candidate | Why |")
		p("|---|---|---|---|---|---|")
		for _, n := range in.Nodes.Nodes {
			cand := "no"
			if n.Removable {
				cand = "**yes**"
			}
			p("| %s | %.0f%% | %.0f%% | %d | %s | %s |", md(n.Name), n.CPUPct*100, n.MemPct*100, n.Pods, cand, md(n.Why))
		}
		p("")
		if len(in.Nodes.Removable) > 0 {
			p("%d node(s) are candidates. At the node price given: %.4f USD/hour. A **candidate**, not a saving: the node must actually be released.", len(in.Nodes.Removable), in.Nodes.CandidateHourlyUSD)
		} else {
			p("No node is a candidate for removal.")
		}
		for _, x := range in.Nodes.Signals.PendingUnschedulable {
			p("- Pending as unschedulable: %s", md(x))
		}
		for _, x := range in.Nodes.Signals.NodesUnderPressure {
			p("- Under pressure: %s", md(x))
		}
		p("")
		for _, c := range in.Nodes.Caveats {
			p("- %s", md(c))
		}
		p("")
	}
	if in.Plan != nil && (len(in.Plan.Steps) > 0 || len(in.Plan.Blocked) > 0) {
		p("### Manual consolidation plan (a proposal: nothing executes it)")
		p("")
		for _, b := range in.Plan.Blocked {
			p("- Blocked: %s", md(b))
		}
		for i, st := range in.Plan.Steps {
			p("%d. **%s** (%d pod(s) under a PodDisruptionBudget)", i+1, md(st.Node), st.PDBProtected)
			for _, c := range st.Commands {
				p("   - `%s`", strings.ReplaceAll(c, "`", "'"))
			}
			for _, u := range st.Undo {
				p("   - undo: `%s`", strings.ReplaceAll(u, "`", "'"))
			}
		}
		p("")
	}

	if in.Fleet != nil {
		p("## Clusters")
		p("")
		p("| Cluster | Kubernetes | Provider | Nodes | CPU requested | Memory requested | Node candidates | Recommendations |")
		p("|---|---|---|---|---|---|---|---|")
		for _, c := range in.Fleet.Clusters {
			if !c.Reachable {
				p("| %s | unreachable | | | | | | %s |", md(c.Context), md(c.Error))
				continue
			}
			var phases []string
			for k, v := range c.Recommendations {
				phases = append(phases, fmt.Sprintf("%s %d", k, v))
			}
			sort.Strings(phases)
			p("| %s | %s | %s | %d | %.0f%% | %.0f%% | %d | %s |", md(c.Context), md(c.Kubernetes), md(c.Provider), c.Nodes, c.CPURequestedPct, c.MemRequestedPct, len(c.RemovableCandidates), md(strings.Join(phases, ", ")))
		}
		p("")
		t := in.Fleet.Totals
		p("%d of %d cluster(s) read: %d nodes, %.1f of %.1f CPU cores requested, %.1f of %.1f GiB requested.", t.Reachable, t.Clusters, t.Nodes, t.CPURequestedCores, t.CPUAllocatableCores, t.MemRequestedGiB, t.MemAllocatableGiB)
		p("")
	}

	if in.Bills != nil {
		r := in.Bills.Reconciliation
		p("## Billing: before and after")
		p("")
		p("Level of the saving: **%s**.", in.Bills.Level)
		for _, m := range in.Bills.MissingForNext {
			p("- Missing for the next level: %s", md(m))
		}
		p("")
		if !r.Comparable {
			p("The two bills **cannot be compared**, so no saving is shown: %s.", md(strings.Join(r.NotComparable, "; ")))
		} else {
			p("| | Before | After |")
			p("|---|---|---|")
			p("| Period | %s to %s | %s to %s |", r.Before.Start.Format("2006-01-02"), r.Before.End.Format("2006-01-02"), r.After.Start.Format("2006-01-02"), r.After.End.Format("2006-01-02"))
			p("| Billed usage (%s) | %s | %s |", md(r.Currency), r.Before.BilledUsage, r.After.BilledUsage)
			p("| Usage at list price (%s) | %s | %s |", md(r.Currency), r.Before.ListUsage, r.After.ListUsage)
			p("")
			p("Difference on %s: **%s %s per day**.", r.Basis, r.SavingPerDay, md(r.Currency))
			if r.ListSavingPerDay != nil {
				p("Difference at list price: %s %s per day.", *r.ListSavingPerDay, md(r.Currency))
			}
		}
		p("")
		for _, n := range r.Notes {
			p("- %s", md(n))
		}
		p("")
	}

	p("## How to read the savings in this report")
	p("")
	p("estimated: a recommendation, nothing applied | projected: applied and kept, no cost measured | observed: cost measured in two equivalent windows, not an invoice | reconciled: two comparable provider bills show the invoiced amount fell.")
}

// renderCmd: rightsize render --plan a.json,b.json --nodes n.json --fleet f.json --reconcile r.json > report.md
func renderCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("render", flag.ContinueOnError)
	var plans, nodes, fleet, bills, title string
	fs.StringVar(&plans, "plan", "", "comma-separated outputs of `rightsize plan` (one per workload).")
	fs.StringVar(&nodes, "nodes", "", "output of `rightsize nodes` (with or without --plan).")
	fs.StringVar(&fleet, "fleet", "", "output of `rightsize fleet`.")
	fs.StringVar(&bills, "reconcile", "", "output of `rightsize reconcile`.")
	fs.StringVar(&title, "title", "", "report title.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if plans == "" && nodes == "" && fleet == "" && bills == "" {
		return fmt.Errorf("give at least one of --plan, --nodes, --fleet, --reconcile")
	}
	in := renderInputs{Title: title, Now: time.Now()}
	load := func(path string, v any) bool {
		data, err := os.ReadFile(path)
		if err == nil {
			err = json.Unmarshal(data, v)
		}
		if err != nil {
			in.Problems = append(in.Problems, fmt.Sprintf("%s: %v", path, err))
			return false
		}
		return true
	}
	for _, f := range strings.Split(plans, ",") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		var r recommend.Recommendation
		if load(f, &r) {
			if r.ID == "" {
				in.Problems = append(in.Problems, f+": not a recommendation record")
				continue
			}
			in.Recs = append(in.Recs, r)
		}
	}
	if nodes != "" {
		// either the bare analysis, or {Analysis, Plan} from --plan
		var wrapped struct {
			Analysis *capacity.Report
			Plan     *capacity.ConsolidationPlan
		}
		if load(nodes, &wrapped) {
			if wrapped.Analysis != nil {
				in.Nodes, in.Plan = wrapped.Analysis, wrapped.Plan
			} else {
				var rep capacity.Report
				if load(nodes, &rep) {
					in.Nodes = &rep
				}
			}
		}
	}
	if fleet != "" {
		var f FleetReport
		if load(fleet, &f) {
			in.Fleet = &f
		}
	}
	if bills != "" {
		var b reconcileOutput
		if load(bills, &b) {
			in.Bills = &b
		}
	}
	renderMarkdown(out, in)
	return nil
}
