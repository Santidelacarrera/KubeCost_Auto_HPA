package controller

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	v1alpha1 "github.com/santidelacarrera/kubecost_auto_hpa/operator/api/v1alpha1"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/engine"
)

// DecisionAuditRecord captures every field required by Section 16 to reconstruct
// and explain any autonomous decision.
type DecisionAuditRecord struct {
	DecisionID          string    `json:"decision_id"`
	ClusterID           string    `json:"cluster_id"`
	Namespace           string    `json:"namespace"`
	Workload            string    `json:"workload"`
	Timestamp           time.Time `json:"timestamp"`
	RPS                 float64   `json:"rps"`
	P99LatencyMs        float64   `json:"p99_latency_ms"`
	PolicyApplied       string    `json:"policy_applied"`
	ExecutionMode       string    `json:"execution_mode"`
	PrevCPUMillicores   int64     `json:"prev_cpu_millicores"`
	PropCPUMillicores   int64     `json:"prop_cpu_millicores"`
	PrevMemoryMiB       int64     `json:"prev_memory_mib"`
	PropMemoryMiB       int64     `json:"prop_memory_mib"`
	PrevReplicas        int32     `json:"prev_replicas"`
	PropReplicas        int32     `json:"prop_replicas"`
	Reason              string    `json:"reason"`
	EstimatedRisk       string    `json:"estimated_risk"`
	ActionExecuted      string    `json:"action_executed"`
	Result              string    `json:"result"`
	ExecutionDurationMs int64     `json:"execution_duration_ms"`
	FinalState          string    `json:"final_state"`
	EstimatedSavingsUSD float64   `json:"estimated_savings_usd"`
	RealizedSavingsUSD  float64   `json:"realized_savings_usd"`
	HumanExplanation    string    `json:"human_explanation"`
}

var (
	metricLabels = []string{"namespace", "name"}

	gCostPerRequest = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "kubecost_hpa_cost_per_request_usd", Help: "Current cost per request (USD)."}, metricLabels)
	gDesiredCPU     = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "kubecost_hpa_desired_cpu_millicores", Help: "CPU request the operator converges pods to."}, metricLabels)
	gSavings        = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "kubecost_hpa_estimated_savings_usd_per_hour", Help: "Estimated hourly CPU savings vs the Deployment template."}, metricLabels)
	gFrozen         = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "kubecost_hpa_budget_frozen", Help: "1 when the budget guardrail blocks growth."}, metricLabels)
	cResizes        = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "kubecost_hpa_resizes_total", Help: "Resize decisions applied."}, []string{"namespace", "name", "action"})
	cSavings        = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "kubecost_hpa_savings_usd_total", Help: "Accumulated estimated CPU savings vs the Deployment template (USD)."}, metricLabels)
	cDrains         = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "kubecost_spot_drains_total", Help: "Spot node drains by result."}, []string{"result"})

	// Enterprise Autonomous Optimizer Prometheus Metrics (Section 16)
	aoDecisionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "autonomous_optimizer_decisions_total",
		Help: "Total autonomous optimization decisions by namespace, name, and action.",
	}, []string{"namespace", "name", "action"})
	aoResizeOpsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "autonomous_optimizer_resize_operations_total",
		Help: "Total in-place pod resize operations attempted.",
	}, []string{"namespace", "name", "action"})
	aoResizeFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "autonomous_optimizer_resize_failures_total",
		Help: "Total in-place pod resize failures by reason.",
	}, []string{"namespace", "name", "reason"})
	aoMemoryRiskScore = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "autonomous_optimizer_memory_risk_score",
		Help: "Normalized memory OOM risk score [0.0 - 1.0].",
	}, metricLabels)
	aoPredictedRPS = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "autonomous_optimizer_predicted_rps",
		Help: "Forecasted RPS from the predictive engine.",
	}, metricLabels)
	aoPredictionError = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "autonomous_optimizer_prediction_error",
		Help: "Observed prediction error (WAPE/MAE) of the forecast model.",
	}, metricLabels)
	aoSpotRiskScore = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "autonomous_optimizer_spot_risk_score",
		Help: "Spot interruption risk score [0.0 - 1.0].",
	}, metricLabels)
	aoNodesOptimizedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "autonomous_optimizer_nodes_optimized_total",
		Help: "Total nodes consolidated or safely drained.",
	}, []string{"result"})
	aoEstimatedSavingsUSD = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "autonomous_optimizer_estimated_savings_usd",
		Help: "Estimated hourly savings in USD.",
	}, metricLabels)
	aoRealizedSavingsUSD = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "autonomous_optimizer_realized_savings_usd",
		Help: "Cumulative realized and verified savings in USD.",
	}, metricLabels)
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		gCostPerRequest, gDesiredCPU, gSavings, gFrozen, cSavings, cResizes, cDrains,
		aoDecisionsTotal, aoResizeOpsTotal, aoResizeFailuresTotal, aoMemoryRiskScore,
		aoPredictedRPS, aoPredictionError, aoSpotRiskScore, aoNodesOptimizedTotal,
		aoEstimatedSavingsUSD, aoRealizedSavingsUSD,
	)
}

func exportMetrics(h *v1alpha1.KubeCostHPA, pods, baseCPU, desired int64, d engine.Decision, cpuPrice float64) {
	l := prometheus.Labels{"namespace": h.Namespace, "name": h.Name}
	if d.CostPerRequest != nil {
		gCostPerRequest.With(l).Set(*d.CostPerRequest)
	}
	gDesiredCPU.With(l).Set(float64(desired))
	estHourlySavings := float64(baseCPU-desired) / 1000 * float64(pods) * cpuPrice
	gSavings.With(l).Set(estHourlySavings)
	aoEstimatedSavingsUSD.With(l).Set(estHourlySavings)
	aoDecisionsTotal.WithLabelValues(h.Namespace, h.Name, d.Action).Inc()

	if d.OOMProtection != nil {
		score := 0.1
		switch d.OOMProtection.RiskLevel {
		case "WATCH":
			score = 0.35
		case "WARNING", "ELEVATED":
			score = 0.65
		case "CRITICAL":
			score = 0.88
		case "EMERGENCY":
			score = 1.0
		}
		aoMemoryRiskScore.With(l).Set(score)
	}
	if d.Predictive != nil {
		aoPredictedRPS.With(l).Set(float64(d.Predictive.PredictedReplicas))
	}

	frozen := 0.0
	if d.BudgetFrozen {
		frozen = 1
	}
	gFrozen.With(l).Set(frozen)
}
