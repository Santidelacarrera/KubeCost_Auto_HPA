package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Label cardinality is bounded by the number of KubeCostAutoscaler objects and
// small closed sets (decision type, error class, node class). Workload names,
// pod names, URLs and credentials are never used as labels.
var (
	kcaLabels = []string{"namespace", "name"}

	mCurrent  = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "kubecost_autoscaler_current_replicas", Help: "Current replicas of the target."}, kcaLabels)
	mDesired  = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "kubecost_autoscaler_desired_replicas", Help: "Replicas proposed by the last decision."}, kcaLabels)
	mHourly   = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "kubecost_autoscaler_hourly_cost_usd", Help: "Allocated hourly run-rate from the last valid KubeCost sample."}, kcaLabels)
	mBudget   = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "kubecost_autoscaler_budget_usd", Help: "Configured maximum hourly cost."}, kcaLabels)
	mAge      = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "kubecost_autoscaler_metric_age_seconds", Help: "Age of the last fetched KubeCost sample."}, kcaLabels)
	mFallback = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "kubecost_autoscaler_fallback_active", Help: "1 while the native HPA fallback owns the replicas."}, kcaLabels)
	mNodes    = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "kubecost_autoscaler_node_classification", Help: "Pods of the target per capacity class (spot, ondemand, unknown, pending)."}, append([]string{"class"}, kcaLabels...))

	mDecisions = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "kubecost_autoscaler_scaling_decisions_total", Help: "Decisions by type."}, append([]string{"type"}, kcaLabels...))
	mActions   = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "kubecost_autoscaler_scaling_action_total", Help: "Replica writes by direction and result."}, append([]string{"direction", "result"}, kcaLabels...))
	mReasons   = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "kubecost_autoscaler_decision_reasons_total", Help: "Decisions by stable reason (why a move was made, held or refused: ScaleDownStabilizing, Cooldown, PendingPods, CostMetricsUnavailable, ...)."}, append([]string{"reason"}, kcaLabels...))
	mDemandAge = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "kubecost_autoscaler_demand_metric_age_seconds", Help: "Age of the oldest resource-metrics sample behind the last demand reading."}, kcaLabels)
	mPaused    = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "kubecost_autoscaler_paused", Help: "1 while the emergency pause annotation is set."}, kcaLabels)
	mPauses    = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "kubecost_autoscaler_pause_activations_total", Help: "Times the emergency pause was switched on."}, kcaLabels)

	// Not per object: bounded by outcome only.
	mReconcileSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "kubecost_autoscaler_reconcile_duration_seconds", Help: "Duration of one KubeCostAutoscaler reconcile.",
		Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30}}, []string{"result"})
	mCostRequestSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "kubecost_autoscaler_kubecost_request_duration_seconds", Help: "Duration of a KubeCost allocation query including retries, by outcome (ok or the error class).",
		Buckets: []float64{.01, .05, .1, .25, .5, 1, 2.5, 5, 10, 20, 45}}, []string{"outcome"})
	mRecoverySeconds = prometheus.NewHistogram(prometheus.HistogramOpts{Name: "kubecost_autoscaler_recovery_duration_seconds", Help: "Time from the first failed signal to the first fully valid one, observed on recovery.",
		Buckets: []float64{15, 30, 60, 120, 300, 600, 1200, 1800, 3600}})

	mAPIErrors = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "kubecost_autoscaler_kubecost_api_errors_total", Help: "KubeCost API failures by class."}, []string{"class"})
)

var nodeClasses = []string{"spot", "ondemand", "unknown", "pending"}

func init() {
	ctrlmetrics.Registry.MustRegister(mCurrent, mDesired, mHourly, mBudget, mAge, mFallback, mNodes, mDecisions, mActions, mAPIErrors, mReasons, mDemandAge, mPaused, mPauses, mReconcileSeconds, mCostRequestSeconds, mRecoverySeconds)
}

// forgetSeries drops the per-object series of a deleted autoscaler.
func forgetSeries(namespace, name string) {
	for _, g := range []*prometheus.GaugeVec{mCurrent, mDesired, mHourly, mBudget, mAge, mFallback, mDemandAge, mPaused} {
		g.DeleteLabelValues(namespace, name)
	}
	for _, c := range nodeClasses {
		mNodes.DeleteLabelValues(c, namespace, name)
	}
	for _, t := range []string{"ScaleUp", "ScaleDown", "Hold", "Fallback", "InvalidMetrics"} {
		mDecisions.DeleteLabelValues(t, namespace, name)
	}
	mPauses.DeleteLabelValues(namespace, name)
	for _, r := range knownReasons {
		mReasons.DeleteLabelValues(r, namespace, name)
	}
	for _, d := range []string{"up", "down"} {
		for _, r := range []string{"ok", "error"} {
			mActions.DeleteLabelValues(d, r, namespace, name)
		}
	}
}

// knownReasons mirrors the policy.Reason constants plus the controller-level ones, so series of a
// deleted autoscaler can be dropped without listing the registry.
var knownReasons = []string{
	"WithinTolerance", "DemandScaleUp", "SafetyScaleUp", "DemandScaleDown", "CostScaleDown", "CostPressureScaleUp",
	"BoundsEnforced", "ScaleUpStabilizing", "ScaleDownStabilizing", "ScaleDownWarmup", "Cooldown", "PendingPods",
	"ReplicasNotReady", "NoReadyReplicas", "DemandMetricsUnavailable", "CostMetricsUnavailable", "CostEstimateUnstable", "",
}
