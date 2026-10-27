# Benchmark procedure on a real cluster (NOT YET RUN)

`docs/BENCHMARKS.md` is a simulation and contains no latency or error data. This is the procedure to produce the real
numbers (p50/p95/p99, errors, dependency availability, estimated cost). **It has not been executed**; any figure produced
must be reported with the cluster, versions and run count, and must not be mixed with the synthetic ones.

## Design

* Two identical copies of the same Deployment (same image, requests, limits, readiness, replicas range) in two namespaces, on the
  same cluster and node pool: **A** scaled by a native HPA (CPU target 70%, `behavior.scaleDown.stabilizationWindowSeconds` set to
  the same window as B), **B** scaled by a `KubeCostAutoscaler`. Never both on one Deployment (the operator refuses).
* Start B in `dryRun: true` for a full day and compare `status.lastDecision` with what A did, before letting it write.
* The same traffic to both: a load generator replaying one recorded or synthetic trace (rate per second) to two endpoints in lockstep
  (k6 `ramping-arrival-rate`, vegeta or hey; pick one, record its version). Closed-loop generators hide queueing: use an open model.
* Patterns: steady, ramp, step, short spikes, burst, gradual decline, intermittent. At least **3 runs per pattern**, alternating which
  side goes first, each long enough for scale-down windows to elapse (at least 3x the window). Record node events.
* Disturbances (one at a time, after the baseline runs): KubeCost unreachable (network policy), delayed metrics-server, operator restart,
  `autoscaling.kubecost.io/pause`, another actor running `kubectl scale`.

## What to record per run

| Item | Source |
|---|---|
| Request latency p50/p95/p99 and error rate (5xx and timeouts) | the application's histogram, or the load generator's client-side measurement (preferred: it includes queueing) |
| Throughput and dropped/unserved requests | load generator |
| Replicas over time, scale-up/down timestamps | `kube_deployment_status_replicas`, operator metrics, `kubectl get events` |
| Time above target utilisation / pending pods / scheduling delay | Prometheus, `kube_pod_status_phase`, events |
| Estimated cost for the run window | KubeCost `GET /model/allocation?window=<start>,<end>&aggregate=controller&accumulate=true` for each namespace |
| Dependency health during the run | `kubecost_autoscaler_kubecost_api_errors_total`, `..._request_duration_seconds`, `..._metric_age_seconds`, `..._demand_metric_age_seconds` |
| Recovery after each disturbance | `kubecost_autoscaler_recovery_duration_seconds` and time until A/B latency returns to baseline |

## Analysis rules

* Report each metric per pattern for A and B side by side, with run-to-run spread; say when n is too small for a percentile.
* Judge **reliability first**: if B's p99 latency, error rate or unserved requests are worse than A's beyond the margin set in advance, B fails
  that pattern regardless of cost. Compare cost only among patterns where B did not fail.
* State what was not controlled (spot/on-demand mix, node autoscaler timing, KubeCost allocation lag). Do not extrapolate to other workloads.

## Result sheet (fill in; leave blank until run)

| Pattern | Run | A p95 / p99 (ms) | B p95 / p99 (ms) | A / B errors | A / B cost USD | A / B max replicas | Notes |
|---|---|---|---|---|---|---|---|
