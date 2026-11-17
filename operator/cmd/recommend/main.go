// Command recommend prints a read-only rightsizing recommendation for one container as JSON.
//
// It only queries Prometheus. It has no Kubernetes client, so it cannot change anything in a cluster.
// Exit status: 0 for a recommendation or an abstention (both are valid answers), 1 for any error
// (Prometheus unreachable, kube-state-metrics missing, invalid input): no data means no recommendation.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/httpx"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/recommend"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/recommend/promsrc"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, time.Now); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string, out *os.File, clock func() time.Time) error {
	fs := flag.NewFlagSet("recommend", flag.ContinueOnError)
	d := recommend.Defaults()
	var (
		promURL, cluster, ns, wl, kind, container, slo, now string
		window, step                                        time.Duration
		hpa, vpa, excluded                                  bool
		cfg                                                 = d
		tls                                                 httpx.Options
	)
	fs.StringVar(&promURL, "prometheus-url", os.Getenv("PROMETHEUS_URL"), "Prometheus base URL (or PROMETHEUS_URL).")
	fs.StringVar(&cluster, "cluster", "", "Cluster name recorded in the output.")
	fs.StringVar(&ns, "namespace", "", "Namespace (required).")
	fs.StringVar(&wl, "workload", "", "Workload name (required).")
	fs.StringVar(&kind, "kind", "Deployment", "Deployment, StatefulSet or DaemonSet.")
	fs.StringVar(&container, "container", "", "Container name (required).")
	fs.StringVar(&slo, "slo-breach-query", "", "Optional instant PromQL that is > 0 while the SLO is breached.")
	fs.StringVar(&now, "now", "", "Evaluation time, RFC3339 (default: now). Fixing it makes a run reproducible.")
	fs.DurationVar(&window, "window", 7*24*time.Hour, "History to analyse.")
	fs.DurationVar(&step, "step", 5*time.Minute, "Sampling step.")
	fs.BoolVar(&hpa, "hpa-on-cpu", false, "An HPA scales this workload on CPU utilisation (adds a risk note).")
	fs.BoolVar(&vpa, "vpa-auto", false, "A VPA in Auto mode owns these requests (the recommender abstains).")
	fs.BoolVar(&excluded, "excluded", false, "The workload opted out (the recommender abstains).")
	fs.IntVar(&cfg.MinSamples, "min-samples", d.MinSamples, "Minimum samples per resource.")
	fs.DurationVar(&cfg.MinWindow, "min-window", d.MinWindow, "Minimum history length.")
	fs.Float64Var(&cfg.CPUPercentile, "cpu-percentile", d.CPUPercentile, "CPU percentile in (0,1].")
	fs.Float64Var(&cfg.MemPercentile, "mem-percentile", d.MemPercentile, "Memory percentile in (0,1].")
	fs.Float64Var(&cfg.CPUHeadroom, "cpu-headroom", d.CPUHeadroom, "CPU headroom fraction.")
	fs.Float64Var(&cfg.MemHeadroom, "mem-headroom", d.MemHeadroom, "Memory headroom fraction.")
	fs.Float64Var(&cfg.CPUPricePerCoreHour, "cpu-price-per-core-hour", 0, "Optional price for the ESTIMATED potential saving.")
	fs.Float64Var(&cfg.MemPricePerGiBHour, "mem-price-per-gib-hour", 0, "Optional price for the ESTIMATED potential saving.")
	fs.StringVar(&tls.CAFile, "prometheus-ca-file", "", "PEM CA bundle to verify Prometheus.")
	fs.StringVar(&tls.BearerTokenFile, "prometheus-bearer-token-file", "", "File with a bearer token for Prometheus.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	tls.BearerToken = os.Getenv("PROMETHEUS_TOKEN")
	if promURL == "" || ns == "" || wl == "" || container == "" {
		return fmt.Errorf("--prometheus-url, --namespace, --workload and --container are required")
	}
	at := clock().UTC()
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
	hc, err := httpx.NewClient(tls, 30*time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	in, err := (&promsrc.Client{BaseURL: promURL, HTTP: hc}).Collect(ctx,
		promsrc.Target{Cluster: cluster, Namespace: ns, Workload: wl, Kind: kind, Container: container, SLOBreachQuery: slo}, window, step, at)
	if err != nil {
		return err
	}
	in.HPAOnCPU, in.VPAAuto, in.Excluded = hpa, vpa, excluded
	rec, err := recommend.Recommend(in, cfg)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(rec)
}
