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
	mAPIErrors = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "kubecost_autoscaler_kubecost_api_errors_total", Help: "KubeCost API failures by class."}, []string{"class"})
)

var nodeClasses = []string{"spot", "ondemand", "unknown", "pending"}

func init() {
	ctrlmetrics.Registry.MustRegister(mCurrent, mDesired, mHourly, mBudget, mAge, mFallback, mNodes, mDecisions, mActions, mAPIErrors)
}

// forgetSeries drops the per-object series of a deleted autoscaler.
func forgetSeries(namespace, name string) {
	for _, g := range []*prometheus.GaugeVec{mCurrent, mDesired, mHourly, mBudget, mAge, mFallback} {
		g.DeleteLabelValues(namespace, name)
	}
	for _, c := range nodeClasses {
		mNodes.DeleteLabelValues(c, namespace, name)
	}
	for _, t := range []string{"ScaleUp", "ScaleDown", "Hold", "Fallback", "InvalidMetrics"} {
		mDecisions.DeleteLabelValues(t, namespace, name)
	}
	for _, d := range []string{"up", "down"} {
		for _, r := range []string{"ok", "error"} {
			mActions.DeleteLabelValues(d, r, namespace, name)
		}
	}
}
