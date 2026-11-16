package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/approval"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/capacity"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/license"
)

// ClusterSummary is one cluster of the fleet report. Everything in it is read at the moment of the call: it is a snapshot, not
// a history, and it comes from requests and allocatable capacity, not from usage or a bill.
type ClusterSummary struct {
	Context    string
	Reachable  bool
	Error      string `json:",omitempty"`
	Kubernetes string `json:",omitempty"`
	// Provider is read from the first node's spec.providerID scheme (gce, azure, ...); "unknown" otherwise.
	Provider string `json:",omitempty"`
	Nodes    int
	Zones    []string `json:",omitempty"`
	// CPU and memory REQUESTED over allocatable, across all nodes.
	CPURequestedCores, CPUAllocatableCores float64
	MemRequestedGiB, MemAllocatableGiB     float64
	CPURequestedPct, MemRequestedPct       float64
	// RemovableCandidates are nodes whose pods fit elsewhere (rightsize nodes). Candidates, never savings.
	RemovableCandidates  []string `json:",omitempty"`
	PendingUnschedulable int
	NodesUnderPressure   int
	// Recommendations counts WorkloadRecommendation objects by phase; nil when the CRD is not installed.
	Recommendations map[string]int `json:",omitempty"`
}

// FleetReport is the read-only multi-cluster view.
type FleetReport struct {
	GeneratedAt time.Time
	Clusters    []ClusterSummary
	Totals      struct {
		Clusters, Reachable, Nodes                  int
		CPURequestedCores, CPUAllocatableCores      float64
		MemRequestedGiB, MemAllocatableGiB          float64
		RemovableCandidates, PendingRecommendations int
	}
	Notes []string
}

// summariseCluster reads one cluster. It needs the read role of deploy/rbac/rightsize.yaml, plus list on workloadrecommendations
// for the phase counts (optional).
func summariseCluster(ctx context.Context, name string, kc kubernetes.Interface, dyn dynamic.Interface, target float64) ClusterSummary {
	s := ClusterSummary{Context: name}
	snap, err := take(ctx, kc, nil, 0)
	if err != nil {
		s.Error = err.Error()
		return s
	}
	s.Reachable = true
	if v, err := kc.Discovery().ServerVersion(); err == nil && v != nil {
		s.Kubernetes = v.GitVersion
	}
	s.Provider = "unknown"
	zones := map[string]bool{}
	for i, n := range snap.nodes {
		if i == 0 {
			if scheme, _, ok := strings.Cut(n.ProviderID, "://"); ok && scheme != "" {
				s.Provider = scheme
			}
		}
		if n.Zone != "" && !zones[n.Zone] {
			zones[n.Zone] = true
			s.Zones = append(s.Zones, n.Zone)
		}
		s.CPUAllocatableCores += n.CPUAllocatable
		s.MemAllocatableGiB += n.MemAllocatable / (1 << 30)
	}
	s.Nodes = len(snap.nodes)
	for _, p := range snap.pods {
		s.CPURequestedCores += p.CPURequest
		s.MemRequestedGiB += p.MemRequest / (1 << 30)
	}
	if s.CPUAllocatableCores > 0 {
		s.CPURequestedPct = s.CPURequestedCores / s.CPUAllocatableCores * 100
	}
	if s.MemAllocatableGiB > 0 {
		s.MemRequestedPct = s.MemRequestedGiB / s.MemAllocatableGiB * 100
	}
	s.PendingUnschedulable, s.NodesUnderPressure = len(snap.signals.PendingUnschedulable), len(snap.signals.NodesUnderPressure)
	if rep, err := capacity.Analyze(snap.nodes, snap.pods, target); err == nil {
		s.RemovableCandidates = rep.Removable
	}
	if dyn != nil {
		if list, err := dyn.Resource(wrecGVR).List(ctx, metav1.ListOptions{}); err == nil {
			s.Recommendations = map[string]int{}
			for _, it := range list.Items {
				phase, _, _ := unstructuredString(it.Object, "status", "phase")
				if phase == "" {
					phase = "Pending"
				}
				s.Recommendations[phase]++
			}
		}
	}
	return s
}

func unstructuredString(obj map[string]any, path ...string) (string, bool, error) {
	var cur any = obj
	for _, k := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", false, nil
		}
		cur = m[k]
	}
	s, ok := cur.(string)
	return s, ok, nil
}

func fleetTotals(r *FleetReport) {
	for _, c := range r.Clusters {
		r.Totals.Clusters++
		if !c.Reachable {
			continue
		}
		r.Totals.Reachable++
		r.Totals.Nodes += c.Nodes
		r.Totals.CPURequestedCores += c.CPURequestedCores
		r.Totals.CPUAllocatableCores += c.CPUAllocatableCores
		r.Totals.MemRequestedGiB += c.MemRequestedGiB
		r.Totals.MemAllocatableGiB += c.MemAllocatableGiB
		r.Totals.RemovableCandidates += len(c.RemovableCandidates)
		r.Totals.PendingRecommendations += c.Recommendations["Pending"]
	}
	r.Notes = []string{
		"read-only snapshot of each cluster through your own kubeconfig contexts: nothing is installed, stored or sent anywhere, and no cluster talks to another",
		"percentages are REQUESTS over allocatable capacity, not usage; removable nodes are candidates, not savings",
	}
	if r.Totals.Reachable < r.Totals.Clusters {
		r.Notes = append(r.Notes, fmt.Sprintf("%d cluster(s) could not be read: the totals cover only the reachable ones", r.Totals.Clusters-r.Totals.Reachable))
	}
}

// fleetCmd prints one report over several kubeconfig contexts.
func fleetCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("fleet", flag.ContinueOnError)
	var c common
	c.bind(fs)
	var contexts string
	var target float64
	var timeout time.Duration
	fs.StringVar(&contexts, "contexts", "", "comma-separated kubeconfig contexts to read (required).")
	fs.Float64Var(&target, "target-utilisation", 0.85, "as in `rightsize nodes`.")
	fs.DurationVar(&timeout, "timeout", 60*time.Second, "time allowed per cluster; an unreachable cluster is reported, not fatal.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if contexts == "" {
		return fmt.Errorf("--contexts is required: name every cluster explicitly")
	}
	rep := FleetReport{GeneratedAt: time.Now().UTC()}
	seen := map[string]bool{}
	for _, name := range strings.Split(contexts, ",") {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		one := c
		one.kctx = name
		ex, closeAudit, err := one.executor()
		if err != nil {
			rep.Clusters = append(rep.Clusters, ClusterSummary{Context: name, Error: err.Error()})
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		rep.Clusters = append(rep.Clusters, summariseCluster(ctx, name, ex.KC, ex.Dyn, target))
		cancel()
		closeAudit()
	}
	fleetTotals(&rep)
	return printJSON(out, rep)
}

// licenseCmd: `license issue` (vendor side, needs the private key) and `license verify` (anyone, offline).
func licenseCmd(args []string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: rightsize license issue|verify [flags]")
	}
	fs := flag.NewFlagSet("license "+args[0], flag.ContinueOnError)
	switch args[0] {
	case "issue":
		var key, id, customer, tier, outFile string
		var maxNodes, days int
		fs.StringVar(&key, "key", "", "Ed25519 private key (PEM, from `rightsize keygen`). Keep it off the cluster.")
		fs.StringVar(&id, "id", "", "licence id (required).")
		fs.StringVar(&customer, "customer", "", "customer name (required).")
		fs.StringVar(&tier, "tier", "", "starter, professional or enterprise.")
		fs.IntVar(&maxNodes, "max-nodes", 0, "licensed node count; 0 = not limited.")
		fs.IntVar(&days, "days", 365, "validity from now, in days.")
		fs.StringVar(&outFile, "out", "", "file to write (required); refuses to overwrite.")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if key == "" || outFile == "" || days <= 0 {
			return fmt.Errorf("--key, --out and a positive --days are required")
		}
		if _, err := os.Stat(outFile); err == nil {
			return fmt.Errorf("%s exists: refusing to overwrite", outFile)
		}
		raw, err := os.ReadFile(key)
		if err != nil {
			return err
		}
		priv, err := approval.ParsePrivate(raw)
		if err != nil {
			return err
		}
		now := time.Now().UTC().Truncate(time.Second)
		l := license.License{ID: id, Customer: customer, Tier: license.Tier(tier), MaxNodes: maxNodes, NotBefore: now, Expires: now.Add(time.Duration(days) * 24 * time.Hour)}
		data, err := license.Issue(priv, l)
		if err != nil {
			return err
		}
		if err := os.WriteFile(outFile, data, 0o600); err != nil {
			return err
		}
		return printJSON(out, map[string]any{"written": outFile, "license": l})
	case "verify":
		var file, pub string
		fs.StringVar(&file, "file", "", "licence file (required).")
		fs.StringVar(&pub, "public-key", "", "public key file, PEM (required).")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if file == "" || pub == "" {
			return fmt.Errorf("--file and --public-key are required")
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		k, err := os.ReadFile(pub)
		if err != nil {
			return err
		}
		l, err := license.Verify(data, []string{string(k)}, time.Now())
		if err != nil {
			return err
		}
		return printJSON(out, map[string]any{"valid": true, "license": l,
			"mode": map[license.Tier]string{license.Starter: "observe-only", license.Professional: "assisted (human approval for every change)", license.Enterprise: "automated allowed inside the policy"}[l.Tier]})
	}
	return fmt.Errorf("unknown license command %q", args[0])
}
