# Demand signals beyond CPU and memory (analysis; NOT implemented)

**Status: PENDING.** This file analyses whether and how `KubeCostAutoscaler` can use request rate, latency or custom signals. No
code for it exists; nothing below has been run.

## Today

* `KubeCostAutoscaler` demand = CPU/memory utilisation (`usage / request`) from `metrics.k8s.io` (`internal/resmetrics`), freshness-checked
  (`maxMetricAge`), requiring at least 50% of ready pods to report. With several `demand.resources` it uses the **most utilised one** (the
  HPA's "highest recommendation wins" rule).
* The legacy `KubeCostHPA` controller already reads request rate, p99 latency and per-pod CPU from Prometheus, but for **vertical** resize
  and a separate engine; it is not the horizontal signal path.
* The decision engine (`internal/policy`) takes one number: `Demand.Utilization`, where 1.0 means "at 100% of the target capacity". Everything
  downstream (target, ceiling, safety replicas, stabilisation) is expressed in that unit.

## Can the architecture admit more signals? Yes, without changing the engine

A new signal fits if it is **converted to a utilisation ratio before it reaches the engine**: `ratio = observed / target`, per metric, then
combined with the existing signals by `max` (the most constrained signal sets the replicas). Stabilisation, safety replicas, cost pressure,
ownership and the single-writer rule do not change.

## Proposed design (not built)

1. **Interface** in `internal/resmetrics` (or a sibling package), typed and unit-carrying:
   `type Signal struct { Name string; Ratio float64; Unit string; Age time.Duration; Pods int }` and
   `type Reader interface { Read(ctx, target, spec) (Signal, error) }`, one implementation per source. The controller combines the readings:
   `Utilization = max(ratio_i)`.
2. **Source**: the Kubernetes **external/custom metrics API** (`external.metrics.k8s.io`, `custom.metrics.k8s.io`), the same contract the HPA
   uses, served by an adapter the cluster already runs (for example a Prometheus adapter). The client packages
   `k8s.io/metrics/pkg/client/external_metrics` and `custom_metrics` are already in the module (`k8s.io/metrics v0.31.14`), so **no new Go dependency is
   needed**, and the operator does not talk to Prometheus directly (no new credentials or TLS path for this feature).
3. **CRD**: `spec.demand.signals[]` with `name`, `kind` (`Pods` | `Object` | `External`), the metric selector and a `target` quantity (for example
   `200 req/s per pod`, `p99 250ms`); unit and value validated by CEL. Absent = today's behaviour.
4. **Validation** per reading: finite, non-negative, in the expected unit, `Age <= maxMetricAge`, covers at least half of the ready pods (for pod
   metrics), a single sample is never enough: the engine's existing stabilisation windows and tolerance apply to the combined ratio.
5. **Missing signal**: a configured signal that is missing, stale or invalid makes the **demand reading invalid** (fail closed: hold, only a safety
   scale-up from the signals that are valid), never "treat as zero". A signal marked `optional: true` could be skipped, but then the controller
   must report that it is running on fewer signals (condition + metric).
6. **Latency is a different kind of signal.** Request rate scales linearly with replicas (ratio = rate / (replicas x per-pod target)); latency does
   not (more replicas do not lower p99 once the bottleneck is elsewhere). Using latency as a *scaling input* risks runaway scale-ups that cost money
   and fix nothing; it is safer as a **guard**: if p99 or error rate breaches the SLO, block cost-driven reductions and allow safety scale-up, but do
   not use it to size the workload. Recommended order: request rate first, latency only as a guard, after a real benchmark shows what latency does.
7. **Combination with cost**: unchanged. Cost pressure moves the target from `targetUtilization` to `costPressureUtilization` on the combined ratio and
   the budget never caps the safety replicas; a new signal can only raise the required replicas, never lower the safety floor.
8. **Observability**: per-signal ratio, age and validity as metrics (`signal` label from the configured, bounded names), reason `DemandMetricsUnavailable`
   with the signal named in the condition message.
9. **Tests required before shipping**: reader fakes with stale / missing / negative / NaN values; engine regression (a signal cannot lower the safety
   floor); simulator scenarios with a request-rate trace; envtest against a real apiserver with a fake external-metrics APIService; and a real-cluster
   run with an adapter (blocked by infrastructure).

## Why it was not implemented in this pass

It adds a CRD field (a compatibility surface), a new failure mode (adapter outage) and it can only be validated end to end against a real metrics
adapter, which is not available here. The honest scope was an analysis with a design that needs no new dependency; it is a roadmap item, not a feature.
