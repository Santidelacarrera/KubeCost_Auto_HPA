package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/cost"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/gitops"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/httpx"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/money"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/outcome"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/recommend"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/recommend/promsrc"
)

// LabelExclude opts a workload out: the planner abstains for it.
const LabelExclude = "autoscaling.kubecost.io/exclude"

func readRecommendation(path string) (recommend.Recommendation, error) {
	var rec recommend.Recommendation
	b, err := os.ReadFile(path)
	if err != nil {
		return rec, err
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		return rec, fmt.Errorf("recommendation: %w", err)
	}
	return rec, nil
}

// approve stamps the approval of exactly one recommendation with the identity the API server reports.
func approveCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("approve", flag.ContinueOnError)
	var c common
	c.bind(fs)
	var recFile string
	fs.StringVar(&recFile, "recommendation", "", "JSON written by cmd/recommend or rightsize plan (required).")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if recFile == "" || c.kctx == "" {
		return fmt.Errorf("--recommendation and --context are required")
	}
	rec, err := readRecommendation(recFile)
	if err != nil {
		return err
	}
	ex, closeAudit, err := c.executor()
	if err != nil {
		return err
	}
	defer closeAudit()
	who, err := ex.Approve(context.Background(), rec)
	if err != nil {
		return err
	}
	return printJSON(out, map[string]any{"approved": rec.ID, "approvedBy": who, "target": rec.Namespace + "/" + rec.Kind + "/" + rec.Workload})
}

// plan is cmd/recommend plus the cluster facts that cmd/recommend cannot see (HPA on CPU, VPA, exclusion label). Read-only.
func planCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	var c common
	c.bind(fs)
	d := recommend.Defaults()
	cfg := d
	var promURL, ns, wl, kind, container, slo, now, cluster string
	var window, step time.Duration
	var tls httpx.Options
	fs.StringVar(&promURL, "prometheus-url", os.Getenv("PROMETHEUS_URL"), "Prometheus base URL (or PROMETHEUS_URL).")
	fs.StringVar(&ns, "namespace", "", "namespace (required).")
	fs.StringVar(&wl, "workload", "", "workload name (required).")
	fs.StringVar(&kind, "kind", "Deployment", "Deployment, StatefulSet or DaemonSet.")
	fs.StringVar(&container, "container", "", "container name (required).")
	fs.StringVar(&cluster, "cluster", "", "cluster name recorded in the output (default: the context).")
	fs.StringVar(&slo, "slo-breach-query", "", "optional instant PromQL that is > 0 while the SLO is breached.")
	fs.StringVar(&now, "now", "", "evaluation time, RFC3339 (default now).")
	fs.DurationVar(&window, "window", 7*24*time.Hour, "history to analyse.")
	fs.DurationVar(&step, "step", 5*time.Minute, "sampling step.")
	fs.IntVar(&cfg.MinSamples, "min-samples", d.MinSamples, "minimum samples per resource.")
	fs.DurationVar(&cfg.MinWindow, "min-window", d.MinWindow, "minimum history length.")
	fs.Float64Var(&cfg.CPUPricePerCoreHour, "cpu-price-per-core-hour", 0, "optional price for the ESTIMATED potential saving.")
	fs.Float64Var(&cfg.MemPricePerGiBHour, "mem-price-per-gib-hour", 0, "optional price for the ESTIMATED potential saving.")
	fs.StringVar(&tls.CAFile, "prometheus-ca-file", "", "PEM CA bundle to verify Prometheus.")
	fs.StringVar(&tls.BearerTokenFile, "prometheus-bearer-token-file", "", "file with a bearer token for Prometheus.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	tls.BearerToken = os.Getenv("PROMETHEUS_TOKEN")
	if promURL == "" || ns == "" || wl == "" || container == "" || c.kctx == "" {
		return fmt.Errorf("--prometheus-url, --namespace, --workload, --container and --context are required")
	}
	if cluster == "" {
		cluster = c.kctx
	}
	at := time.Now().UTC()
	if now != "" {
		var err error
		if at, err = time.Parse(time.RFC3339, now); err != nil {
			return fmt.Errorf("--now: %w", err)
		}
	}
	cfg.Now = at
	if err := cfg.Validate(); err != nil {
		return err
	}
	ex, closeAudit, err := c.executor()
	if err != nil {
		return err
	}
	defer closeAudit()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	facts, _, ierr := ex.Inspect(ctx, kind, ns, wl)
	if ierr != nil && !facts.Known {
		// without the cluster facts we cannot rule out a VPA or an HPA: say so instead of recommending blind
		return fmt.Errorf("cannot establish the cluster facts for %s/%s/%s: %w", ns, kind, wl, ierr)
	}
	hc, err := httpx.NewClient(tls, 30*time.Second)
	if err != nil {
		return err
	}
	in, err := (&promsrc.Client{BaseURL: promURL, HTTP: hc}).Collect(ctx,
		promsrc.Target{Cluster: cluster, Namespace: ns, Workload: wl, Kind: kind, Container: container, SLOBreachQuery: slo}, window, step, at)
	if err != nil {
		return err
	}
	in.HPAOnCPU, in.VPAAuto = facts.HPAOnCPU, facts.VPAAuto
	in.Excluded = strings.EqualFold(facts.Labels[LabelExclude], "true")
	rec, err := recommend.Recommend(in, cfg)
	if err != nil {
		return err
	}
	return printJSON(out, rec)
}

// report summarises the audit trail into process outcomes (acceptance, rollback rate, estimated savings kept).
func reportCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	var file, since string
	fs.StringVar(&file, "audit-file", "", "JSONL audit file written with --audit-file (required).")
	fs.StringVar(&since, "since", "", "only events at or after this RFC3339 time.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if file == "" {
		return fmt.Errorf("--audit-file is required")
	}
	var from time.Time
	if since != "" {
		var err error
		if from, err = time.Parse(time.RFC3339, since); err != nil {
			return fmt.Errorf("--since: %w", err)
		}
	}
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	s, err := outcome.Summarize(f, from)
	if err != nil {
		return err
	}
	return printJSON(out, s)
}

// propose writes the recommendation into a manifest on a new local git branch (no push, no cluster access).
func proposeCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("propose", flag.ContinueOnError)
	var recFile, repo, file, branch string
	fs.StringVar(&recFile, "recommendation", "", "recommendation JSON (required).")
	fs.StringVar(&repo, "repo", ".", "git repository holding the manifest.")
	fs.StringVar(&file, "file", "", "manifest path relative to --repo (required).")
	fs.StringVar(&branch, "branch", "", "new branch name (default rightsize/<kind>-<name>-<id>).")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if recFile == "" || file == "" {
		return fmt.Errorf("--recommendation and --file are required")
	}
	rec, err := readRecommendation(recFile)
	if err != nil {
		return err
	}
	res, err := gitops.Propose(gitops.Options{RepoDir: repo, File: file, Branch: branch, Rec: rec})
	if err != nil {
		return err
	}
	return printJSON(out, map[string]any{"branch": res.Branch, "commit": res.Commit, "pushed": false,
		"next": "review the branch and open a pull request yourself; nothing was pushed and no cluster was contacted", "diff": res.Diff})
}

// ---- cost extras: budgets and deviation against an earlier report

func parseBudgets(list string) (map[string]money.USD, error) {
	out := map[string]money.USD{}
	if list == "" {
		return out, nil
	}
	for _, kv := range strings.Split(list, ",") {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 || parts[0] == "" {
			return nil, fmt.Errorf("--budget wants namespace=usd pairs, got %q", kv)
		}
		v, err := money.Parse(parts[1])
		if err != nil || v < 0 {
			return nil, fmt.Errorf("bad budget %q", kv)
		}
		out[parts[0]] = v
	}
	return out, nil
}

// savedReport is the subset of the "cost" JSON output needed to compare two reports.
type savedReport struct {
	Basis       cost.Basis        `json:"basis"`
	Start       time.Time         `json:"start"`
	End         time.Time         `json:"end"`
	ByNamespace map[string]string `json:"byNamespace"`
}

func loadReport(path string) (cost.Report, error) {
	var r cost.Report
	b, err := os.ReadFile(path)
	if err != nil {
		return r, err
	}
	var s savedReport
	if err := json.Unmarshal(b, &s); err != nil {
		return r, fmt.Errorf("baseline report: %w", err)
	}
	r = cost.Report{Basis: s.Basis, Start: s.Start, End: s.End, ByNamespace: map[string]money.USD{}}
	for ns, v := range s.ByNamespace {
		u, err := money.Parse(v)
		if err != nil {
			return r, fmt.Errorf("baseline report: namespace %s: %w", ns, err)
		}
		r.ByNamespace[ns] = u
	}
	return r, nil
}

func budgetView(sts []cost.BudgetStatus) []map[string]any {
	var v []map[string]any
	for _, s := range sts {
		v = append(v, map[string]any{"namespace": s.Namespace, "spent": s.Spent.String(), "budget": s.Budget.String(),
			"used": math.Round(s.Used*1000) / 1000, "over": s.Over, "basis": s.Basis})
	}
	return v
}

func deviationView(ds []cost.Deviation) []map[string]any {
	var v []map[string]any
	for _, d := range ds {
		ch := any(math.Round(d.ChangePct*10) / 10)
		if math.IsInf(d.ChangePct, 0) {
			ch = "new"
		}
		v = append(v, map[string]any{"namespace": d.Namespace, "usdPerHourBefore": d.BeforePerHour, "usdPerHourAfter": d.After, "changePct": ch})
	}
	return v
}
