package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Label cardinality: phase and outcome are small closed sets. Namespaces, workload names, recommendation IDs and approver names are never labels.
var (
	mRecPhase = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "kubecost_recommendation_phase_transitions_total",
		Help: "WorkloadRecommendation phase transitions written by the applier, by the phase entered (Pending, Denied, DryRun, Applying, Verified, RolledBack, Failed)."}, []string{"phase"})
	mRecSavings = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "kubecost_recommendation_estimated_savings_usd_total",
		Help: "ESTIMATED monthly saving (requests reduced x supplied unit prices, USD) of applied recommendations by outcome: kept (Verified) or rolled_back (never took effect). Not a bill and not an observed saving."}, []string{"outcome"})
	mRecApplySeconds = prometheus.NewHistogram(prometheus.HistogramOpts{Name: "kubecost_recommendation_apply_duration_seconds",
		Help:    "Seconds from the change being applied to its final outcome (Verified, RolledBack or Failed), including the observation window.",
		Buckets: []float64{15, 30, 60, 120, 300, 600, 900, 1800}})
)

func init() {
	ctrlmetrics.Registry.MustRegister(mRecPhase, mRecSavings, mRecApplySeconds)
}
