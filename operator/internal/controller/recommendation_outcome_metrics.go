package controller

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/api/v1alpha1"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/recommend"
)

// OutcomeCollector exports the OUTCOME of recommendations as gauges computed at scrape time from the WorkloadRecommendation objects
// themselves. Unlike the applier's counters (recommendation_applier_metrics.go), these survive an operator restart and agree with
// `kubectl get wrec`, so a dashboard can show acceptance, rollbacks and what was actually reduced without anyone reading logs.
//
// What each number is:
//   - recommendations by phase: a count of objects, cluster-wide.
//   - acceptance ratio: applied / (applied + denied), where applied = Applying + Verified + RolledBack + Failed. Pending and DryRun
//     are not decisions yet; with no decision the gauge is absent rather than 0 or 1.
//   - rollback ratio: RolledBack / (Verified + RolledBack): of the changes that finished their observation, how many were undone.
//   - kept request reduction: for Verified objects whose action was Decrease, the sum of (current - recommended) requests PER POD
//     TEMPLATE (CPU cores, memory bytes), from the stored record. It is what changed in the workload specs, not a measured node or
//     bill reduction (multiply by replicas, and see docs/LAB.md results 14 and 17 for when that frees a node).
//   - kept estimated monthly USD: the recommender's ESTIMATE for the same objects (requests delta x supplied unit prices). Not money.
//
// Labels are closed sets (phase, resource). Namespaces, workloads and approvers are never labels.
type OutcomeCollector struct {
	Reader  client.Reader
	Timeout time.Duration

	byPhase, acceptance, rollback, reduction, estimated, ok *prometheus.Desc
}

// NewOutcomeCollector builds the collector. reader should be the manager's cached client.
func NewOutcomeCollector(reader client.Reader) *OutcomeCollector {
	return &OutcomeCollector{
		Reader: reader, Timeout: 5 * time.Second,
		byPhase: prometheus.NewDesc("kubecost_recommendations",
			"WorkloadRecommendation objects by current phase (cluster-wide count).", []string{"phase"}, nil),
		acceptance: prometheus.NewDesc("kubecost_recommendation_acceptance_ratio",
			"Applied / (applied + denied) over current WorkloadRecommendation objects; absent until one was decided.", nil, nil),
		rollback: prometheus.NewDesc("kubecost_recommendation_rollback_ratio",
			"RolledBack / (Verified + RolledBack) over current WorkloadRecommendation objects; absent until one finished.", nil, nil),
		reduction: prometheus.NewDesc("kubecost_recommendation_kept_request_reduction",
			"Sum of request reductions per pod template kept after verification (cpu_cores, memory_bytes). A spec change, not a measured bill.",
			[]string{"resource"}, nil),
		estimated: prometheus.NewDesc("kubecost_recommendation_kept_estimated_monthly_usd",
			"ESTIMATED monthly saving of verified decreases (requests delta x supplied unit prices). Not a bill and not an observed saving.", nil, nil),
		ok: prometheus.NewDesc("kubecost_recommendation_outcome_collect_success",
			"1 if the WorkloadRecommendation objects could be listed for this scrape, 0 otherwise (the other outcome gauges are then absent).", nil, nil),
	}
}

var outcomePhases = []string{v1alpha1.RecPhasePending, v1alpha1.RecPhaseDryRun, v1alpha1.RecPhaseApplying, v1alpha1.RecPhaseVerified,
	v1alpha1.RecPhaseRolledBack, v1alpha1.RecPhaseDenied, v1alpha1.RecPhaseFailed}

// Describe implements prometheus.Collector.
func (c *OutcomeCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.byPhase, c.acceptance, c.rollback, c.reduction, c.estimated, c.ok} {
		ch <- d
	}
}

// OutcomeTotals is what the collector derives from a list of objects; exported for tests and for `rightsize report`.
type OutcomeTotals struct {
	ByPhase                    map[string]int
	Acceptance, Rollback       float64
	HasAcceptance, HasRollback bool
	CPUCores, MemBytes, EstUSD float64
	UnreadableVerifiedRecords  int
}

// Totals computes the outcome of a set of WorkloadRecommendation objects.
func Totals(items []v1alpha1.WorkloadRecommendation) OutcomeTotals {
	t := OutcomeTotals{ByPhase: map[string]int{}}
	for _, p := range outcomePhases {
		t.ByPhase[p] = 0
	}
	for i := range items {
		w := &items[i]
		phase := w.Status.Phase
		if phase == "" {
			phase = v1alpha1.RecPhasePending
		}
		t.ByPhase[phase]++
		if phase != v1alpha1.RecPhaseVerified {
			continue
		}
		var rec recommend.Recommendation
		if err := json.Unmarshal([]byte(w.Spec.Record), &rec); err != nil {
			t.UnreadableVerifiedRecords++
			continue
		}
		if rec.CPU.Action == recommend.Decrease && rec.CPU.Current > rec.CPU.Recommended {
			t.CPUCores += rec.CPU.Current - rec.CPU.Recommended
			t.EstUSD += rec.CPU.PotentialMonthlyUSD
		}
		if rec.Memory.Action == recommend.Decrease && rec.Memory.Current > rec.Memory.Recommended {
			t.MemBytes += rec.Memory.Current - rec.Memory.Recommended
			t.EstUSD += rec.Memory.PotentialMonthlyUSD
		}
	}
	applied := t.ByPhase[v1alpha1.RecPhaseApplying] + t.ByPhase[v1alpha1.RecPhaseVerified] + t.ByPhase[v1alpha1.RecPhaseRolledBack] + t.ByPhase[v1alpha1.RecPhaseFailed]
	if d := applied + t.ByPhase[v1alpha1.RecPhaseDenied]; d > 0 {
		t.Acceptance, t.HasAcceptance = float64(applied)/float64(d), true
	}
	if f := t.ByPhase[v1alpha1.RecPhaseVerified] + t.ByPhase[v1alpha1.RecPhaseRolledBack]; f > 0 {
		t.Rollback, t.HasRollback = float64(t.ByPhase[v1alpha1.RecPhaseRolledBack])/float64(f), true
	}
	return t
}

// Collect implements prometheus.Collector. It lists from the reader (the informer cache in the operator), bounded by Timeout.
func (c *OutcomeCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), c.Timeout)
	defer cancel()
	var list v1alpha1.WorkloadRecommendationList
	if c.Reader == nil {
		ch <- prometheus.MustNewConstMetric(c.ok, prometheus.GaugeValue, 0)
		return
	}
	if err := c.Reader.List(ctx, &list); err != nil {
		ch <- prometheus.MustNewConstMetric(c.ok, prometheus.GaugeValue, 0)
		return
	}
	ch <- prometheus.MustNewConstMetric(c.ok, prometheus.GaugeValue, 1)
	t := Totals(list.Items)
	for _, p := range outcomePhases {
		ch <- prometheus.MustNewConstMetric(c.byPhase, prometheus.GaugeValue, float64(t.ByPhase[p]), p)
	}
	if t.HasAcceptance {
		ch <- prometheus.MustNewConstMetric(c.acceptance, prometheus.GaugeValue, t.Acceptance)
	}
	if t.HasRollback {
		ch <- prometheus.MustNewConstMetric(c.rollback, prometheus.GaugeValue, t.Rollback)
	}
	ch <- prometheus.MustNewConstMetric(c.reduction, prometheus.GaugeValue, t.CPUCores, "cpu_cores")
	ch <- prometheus.MustNewConstMetric(c.reduction, prometheus.GaugeValue, t.MemBytes, "memory_bytes")
	ch <- prometheus.MustNewConstMetric(c.estimated, prometheus.GaugeValue, t.EstUSD)
}

// RegisterOutcomeCollector adds the collector to the operator's metrics registry once; a second call is a no-op.
func RegisterOutcomeCollector(reader client.Reader) error {
	err := ctrlmetrics.Registry.Register(NewOutcomeCollector(reader))
	var are prometheus.AlreadyRegisteredError
	if errors.As(err, &are) {
		return nil
	}
	return err
}
