package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/capacity"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/cost"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/money"
)

// snapshot is a read-only view of the cluster: nodes and scheduled pods with their request footprint.
type snapshot struct {
	nodes []capacity.Node
	pods  []capacity.Pod
}

func podRequests(p corev1.Pod) (cpu, mem float64) {
	for _, c := range p.Spec.Containers {
		cpu += c.Resources.Requests.Cpu().AsApproximateFloat64()
		mem += c.Resources.Requests.Memory().AsApproximateFloat64()
	}
	return cpu, mem
}

func take(ctx context.Context, kc kubernetes.Interface, prices map[string]float64, defaultPrice float64) (snapshot, error) {
	var s snapshot
	ns, err := kc.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return s, err
	}
	for _, n := range ns.Items {
		price := defaultPrice
		if v, ok := prices[n.Name]; ok {
			price = v
		}
		s.nodes = append(s.nodes, capacity.Node{ProviderID: n.Spec.ProviderID, Name: n.Name, CPUAllocatable: n.Status.Allocatable.Cpu().AsApproximateFloat64(),
			MemAllocatable: n.Status.Allocatable.Memory().AsApproximateFloat64(), HourlyUSD: price})
	}
	pdbs, err := kc.PolicyV1().PodDisruptionBudgets(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return s, err
	}
	pods, err := kc.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return s, err
	}
	for _, p := range pods.Items {
		if p.Spec.NodeName == "" || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		cpu, mem := podRequests(p)
		cp := capacity.Pod{Namespace: p.Namespace, Name: p.Name, Node: p.Spec.NodeName, CPURequest: cpu, MemRequest: mem}
		controlled := false
		for _, o := range p.OwnerReferences {
			if o.Controller != nil && *o.Controller {
				controlled = true
				if o.Kind == "DaemonSet" {
					cp.DaemonSet = true
				}
			}
		}
		cp.Pinned = !controlled || pinned(p)
		for _, b := range pdbs.Items {
			if b.Namespace != p.Namespace || b.Spec.Selector == nil {
				continue
			}
			if sel, err := metav1.LabelSelectorAsSelector(b.Spec.Selector); err == nil && !sel.Empty() && sel.Matches(labels.Set(p.Labels)) {
				cp.ProtectedByPDB = true
			}
		}
		s.pods = append(s.pods, cp)
	}
	return s, nil
}

// pinned: tied to its node by a hostname selector, hostPath or a persistent volume claim.
func pinned(p corev1.Pod) bool {
	if _, ok := p.Spec.NodeSelector["kubernetes.io/hostname"]; ok {
		return true
	}
	for _, v := range p.Spec.Volumes {
		if v.HostPath != nil || v.PersistentVolumeClaim != nil {
			return true
		}
	}
	return false
}

func parsePrices(list string) (map[string]float64, error) {
	out := map[string]float64{}
	if list == "" {
		return out, nil
	}
	for _, kv := range strings.Split(list, ",") {
		var name string
		var v float64
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("--node-prices wants name=usdPerHour pairs, got %q", kv)
		}
		name = parts[0]
		if _, err := fmt.Sscanf(parts[1], "%f", &v); err != nil || v < 0 {
			return nil, fmt.Errorf("bad price %q", kv)
		}
		out[name] = v
	}
	return out, nil
}

func nodesCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("nodes", flag.ContinueOnError)
	var c common
	c.bind(fs)
	var target, price float64
	var prices string
	fs.Float64Var(&target, "target-utilisation", 0.85, "receiving nodes may be requested up to this fraction.")
	fs.Float64Var(&price, "node-hourly-usd", 0, "optional price applied to every node for the candidate estimate.")
	fs.StringVar(&prices, "node-prices", "", "optional per-node prices: name=usdPerHour,name2=...")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if c.kctx == "" {
		return fmt.Errorf("--context is required")
	}
	pm, err := parsePrices(prices)
	if err != nil {
		return err
	}
	ex, closeAudit, err := c.executor()
	if err != nil {
		return err
	}
	defer closeAudit()
	snap, err := take(context.Background(), ex.KC, pm, price)
	if err != nil {
		return err
	}
	rep, err := capacity.Analyze(snap.nodes, snap.pods, target)
	if err != nil {
		return err
	}
	return printJSON(out, rep)
}

func costCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("cost", flag.ContinueOnError)
	var c common
	c.bind(fs)
	var billing, label, startS, endS, shared, budgetList, baseline, format, focusCost string
	var devPct float64
	var hourly, cpuWeight float64
	fs.StringVar(&billing, "billing-file", "", "billing CSV: resource,cost_usd,start,end, or a FOCUS export with --billing-format focus (real data, or labelled simulated).")
	fs.StringVar(&format, "billing-format", "simple", "simple (resource,cost_usd,start,end) or focus (FinOps FOCUS CSV).")
	fs.StringVar(&focusCost, "focus-cost-column", "BilledCost", "with --billing-format focus: BilledCost (invoiced) or EffectiveCost (amortised, after discounts).")
	fs.StringVar(&label, "billing-label", "", "label shown in the report, e.g. SIMULATED-lab or aws-cur-2026-10.")
	fs.Float64Var(&hourly, "node-hourly-usd", 0, "ESTIMATED basis: hourly price of every node when there is no billing file.")
	fs.StringVar(&startS, "start", "", "window start, RFC3339 (default: 24h before --end).")
	fs.StringVar(&endS, "end", "", "window end, RFC3339 (default: now).")
	fs.StringVar(&shared, "shared-namespaces", "kube-system", "comma list redistributed over the other namespaces.")
	fs.Float64Var(&cpuWeight, "cpu-weight", 0.5, "fraction of node cost attributed to CPU.")
	fs.StringVar(&budgetList, "budget", "", "namespace budgets for the window: ns=usd,ns2=usd.")
	fs.StringVar(&baseline, "baseline-report", "", "an earlier output of this command; namespaces whose cost per hour moved more than --deviation-pct are listed.")
	fs.Float64Var(&devPct, "deviation-pct", 20, "threshold for --baseline-report, in percent.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if c.kctx == "" {
		return fmt.Errorf("--context is required")
	}
	if (billing == "") == (hourly <= 0) {
		return fmt.Errorf("give exactly one of --billing-file (BILLED basis) or --node-hourly-usd (ESTIMATED basis)")
	}
	end := time.Now().UTC()
	if endS != "" {
		var err error
		if end, err = time.Parse(time.RFC3339, endS); err != nil {
			return err
		}
	}
	start := end.Add(-24 * time.Hour)
	if startS != "" {
		var err error
		if start, err = time.Parse(time.RFC3339, startS); err != nil {
			return err
		}
	}
	ex, closeAudit, err := c.executor()
	if err != nil {
		return err
	}
	defer closeAudit()
	snap, err := take(context.Background(), ex.KC, nil, 0)
	if err != nil {
		return err
	}
	var cn []cost.Node
	var cp []cost.Pod
	for _, n := range snap.nodes {
		cn = append(cn, cost.Node{ProviderID: n.ProviderID, Name: n.Name, CPUAllocatable: n.CPUAllocatable, MemAllocatable: n.MemAllocatable})
	}
	for _, p := range snap.pods {
		cp = append(cp, cost.Pod{Namespace: p.Namespace, Node: p.Node, CPURequest: p.CPURequest, MemRequest: p.MemRequest})
	}
	var sharedList []string
	for _, s := range strings.Split(shared, ",") {
		if s = strings.TrimSpace(s); s != "" {
			sharedList = append(sharedList, s)
		}
	}
	cfg := cost.Config{CPUWeight: cpuWeight, SharedNamespaces: sharedList}
	var rep cost.Report
	var focusNotes []string
	if billing != "" {
		ad := cost.FileAdapter{Path: billing, Label: label}
		var nc map[string]money.USD
		var extra []cost.BillingItem
		switch format {
		case "simple":
			items, err := ad.Billed(context.Background(), start, end)
			if err != nil {
				return err
			}
			nc, extra = cost.SplitByNode(items, cn)
		case "focus":
			f, err := os.Open(billing)
			if err != nil {
				return err
			}
			defer f.Close()
			items, sum, err := cost.ParseFOCUSWith(f, start, end, cost.FocusOptions{CostColumn: focusCost})
			if err != nil {
				return err
			}
			nc, extra = cost.SplitByNodeIdentity(items, cn)
			focusNotes = sum.Notes
			for cat, v := range sum.Skipped {
				focusNotes = append(focusNotes, fmt.Sprintf("skipped %s charges: %s", cat, v.String()))
			}
		default:
			return fmt.Errorf("--billing-format must be simple or focus")
		}
		var err error
		rep, err = cost.Allocate(cost.BasisBilled, ad.Name(), start, end, cn, cp, nc, extra, cfg)
		if err != nil {
			return err
		}
	} else {
		price, err := money.FromFloat(hourly)
		if err != nil {
			return err
		}
		nc := map[string]money.USD{}
		for _, n := range cn {
			nc[n.Name] = cost.EstimateNodeCost(price, start, end)
		}
		rep, err = cost.Allocate(cost.BasisEstimated, fmt.Sprintf("flag --node-hourly-usd=%g", hourly), start, end, cn, cp, nc, nil, cfg)
		if err != nil {
			return err
		}
	}
	budgets, err := parseBudgets(budgetList)
	if err != nil {
		return err
	}
	var deviations []map[string]any
	if baseline != "" {
		prev, err := loadReport(baseline)
		if err != nil {
			return err
		}
		ds, err := cost.Deviations(prev, rep, devPct)
		if err != nil {
			return err
		}
		deviations = deviationView(ds)
	}
	view := map[string]string{}
	for ns, v := range rep.ByNamespace {
		view[ns] = v.String()
	}
	return printJSON(out, map[string]any{"basis": rep.Basis, "source": rep.Source, "start": rep.Start, "end": rep.End,
		"total": rep.Total.String(), "accounted": rep.Accounted.String(), "difference": rep.Difference.String(), "reconciled": rep.Reconciled,
		"shared": rep.Shared.String(), "byNamespace": view, "budgets": budgetView(cost.CheckBudgets(rep, budgets)),
		"deviations": deviations, "notes": append(rep.Notes, focusNotes...)})
}
