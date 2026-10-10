package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	v1alpha1 "github.com/santidelacarrera/kubecost_auto_hpa/operator/api/v1alpha1"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/engine"
)

var (
	metricLabels = []string{"namespace", "name"}

	gCostPerRequest = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "kubecost_hpa_cost_per_request_usd", Help: "Current cost per request (USD)."}, metricLabels)
	gDesiredCPU     = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "kubecost_hpa_desired_cpu_millicores", Help: "CPU request the operator converges pods to."}, metricLabels)
	gSavings        = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "kubecost_hpa_estimated_savings_usd_per_hour", Help: "Estimated hourly CPU savings vs the Deployment template."}, metricLabels)
	gFrozen         = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "kubecost_hpa_budget_frozen", Help: "1 when the budget guardrail blocks growth."}, metricLabels)
	cResizes        = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "kubecost_hpa_resizes_total", Help: "Resize decisions applied."}, []string{"namespace", "name", "action"})
	cSavings        = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "kubecost_hpa_savings_usd_total", Help: "Accumulated estimated CPU savings vs the Deployment template (USD)."}, metricLabels)
	cDrains         = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "kubecost_spot_drains_total", Help: "Spot node drains by result."}, []string{"result"})
)

func init() {
	ctrlmetrics.Registry.MustRegister(gCostPerRequest, gDesiredCPU, gSavings, gFrozen, cSavings, cResizes, cDrains)
}

func exportMetrics(h *v1alpha1.KubeCostHPA, pods, baseCPU, desired int64, d engine.Decision, cpuPrice float64) {
	l := prometheus.Labels{"namespace": h.Namespace, "name": h.Name}
	if d.CostPerRequest != nil {
		gCostPerRequest.With(l).Set(*d.CostPerRequest)
	}
	gDesiredCPU.With(l).Set(float64(desired))
	gSavings.With(l).Set(float64(baseCPU-desired) / 1000 * float64(pods) * cpuPrice)
	frozen := 0.0
	if d.BudgetFrozen {
		frozen = 1
	}
	gFrozen.With(l).Set(frozen)
}
