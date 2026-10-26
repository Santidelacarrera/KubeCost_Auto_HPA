# Observability and proposed SLOs

Metrics are served by the operator (controller-runtime registry, `--metrics-bind-address`, port 8080; optional
`serviceMonitor`). Labels are `namespace`/`name` of the `KubeCostAutoscaler` plus small closed sets (decision type, reason,
error class, node class). Workload names, pod names, URLs and credentials are never labels; logs never contain tokens or
response bodies.

## `KubeCostAutoscaler` metrics

| Question to answer | Metric | Notes |
|---|---|---|
| How many decisions, of what kind? | `kubecost_autoscaler_scaling_decisions_total{type}` | ScaleUp, ScaleDown, Hold, Fallback, InvalidMetrics |
| Why was a move made, held or **refused**? | `kubecost_autoscaler_decision_reasons_total{reason}` | e.g. `ScaleDownStabilizing`, `ScaleDownWarmup`, `Cooldown`, `PendingPods`, `ReplicasNotReady`, `SafetyScaleUp`, `CostPressureScaleUp`: this is the "reason a reduction was rejected" counter |
| Which decisions were blocked by missing data? | same, `reason=~"DemandMetricsUnavailable\|CostMetricsUnavailable\|CostEstimateUnstable\|NoReadyReplicas"` | |
| Replica writes and their outcome | `kubecost_autoscaler_scaling_action_total{direction,result}` | `result="error"` includes compare-and-swap conflicts |
| Desired vs current replicas | `kubecost_autoscaler_desired_replicas`, `kubecost_autoscaler_current_replicas` | |
| Cost and budget | `kubecost_autoscaler_hourly_cost_usd`, `kubecost_autoscaler_budget_usd` | |
| How old is the cost data? | `kubecost_autoscaler_metric_age_seconds` | compare with `failurePolicy.maxMetricAge` |
| How old is the demand data? | `kubecost_autoscaler_demand_metric_age_seconds` | oldest resource-metrics sample behind the last reading |
| How slow is KubeCost? | `kubecost_autoscaler_kubecost_request_duration_seconds{outcome}` | histogram, includes retries; `outcome` is `ok` or the error class |
| KubeCost errors and timeouts | `kubecost_autoscaler_kubecost_api_errors_total{class}` | `transient` = network error, timeout, 408/429/5xx |
| How long did a recovery take? | `kubecost_autoscaler_recovery_duration_seconds` | first failed signal to first fully valid one |
| Reconcile time | `kubecost_autoscaler_reconcile_duration_seconds{result}` | plus controller-runtime's `controller_runtime_reconcile_time_seconds` |
| Is the fallback HPA in charge? | `kubecost_autoscaler_fallback_active` | |
| Emergency pause | `kubecost_autoscaler_paused`, `kubecost_autoscaler_pause_activations_total` | |
| Where do pods run? | `kubecost_autoscaler_node_classification{class}` | spot, ondemand, unknown, pending |

Not covered by a dedicated metric: Kubernetes API errors (use controller-runtime's `rest_client_requests_total{code}` and
`controller_runtime_reconcile_errors_total`), metrics-server errors (they appear as the reason `DemandMetricsUnavailable` and the
`Degraded` condition), and the legacy `KubeCostHPA` controller's reasons. Every decision is also a structured explanation in
`status.lastDecision` (type, reason, message, proposed and candidate replicas, whether cost drove it).

`PrometheusRule` alerts (Helm `prometheusRule.enabled`) cover stale cost data, stale demand data, KubeCost errors, auth failures, slow
KubeCost queries, slow reconciles, fallback active, scale failures, over budget, a pause left on for over an hour, and operator down.
All 13 expressions were parsed with the real Prometheus PromQL parser (syntax only; they have not been evaluated against live data).

## Proposed SLOs (objectives, NOT results)

These are targets to validate against real data. None has been measured.

| # | Objective | Indicator |
|---|---|---|
| 1 | The decision loop is available: 99% of reconciles succeed | `1 - rate(reconcile_duration_seconds_count{result="error"}) / rate(reconcile_duration_seconds_count)` |
| 2 | Cost data is fresh: sample age within `maxMetricAge` 99% of the time | `metric_age_seconds <= maxMetricAge` |
| 3 | Reconciles are quick: p99 under 5 s | `histogram_quantile(0.99, ...reconcile_duration_seconds_bucket)` |
| 4 | KubeCost answers: p95 under 5 s | `histogram_quantile(0.95, ...kubecost_request_duration_seconds_bucket{outcome="ok"})` |
| 5 | Recovery is fast: p95 under 5 minutes | `histogram_quantile(0.95, ...recovery_duration_seconds_bucket)` |
| 6 | Workload SLO is not degraded: p99 latency and error rate no worse than the same workload under a native HPA by more than a margin to be set from the real benchmark | needs `docs/BENCHMARK_PROCEDURE.md`; **no value is claimed** |
