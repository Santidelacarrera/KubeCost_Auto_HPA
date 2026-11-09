# ADR-0001: A separate, read-only rightsizing recommender

Status: accepted, phase 1 implemented 2026-11-09 (`internal/recommend`, `internal/recommend/promsrc`, `cmd/recommend`; see docs/RECOMMENDER.md).
Phases 2 and 3 are not implemented. Phase 1 is IMPLEMENTED BUT NOT VERIFIED against a real Prometheus.

## Context

The repository today is an autoscaler: it scales replicas from demand and a KubeCost cost policy, can resize pods in place (legacy `KubeCostHPA`),
and drains Spot nodes. It has no way to say "this workload requests 2 CPU and needs 400m", and it has no policy, approval or rollback machinery.
The product brief asks for explainable CPU/memory requests/limits recommendations (percentiles, headroom, OOM/throttle/latency vetoes,
abstention on poor data), applied only under explicit policy and with verified rollback.

Constraints: the autoscaler is the single writer of replicas for what it manages (`internal/ownership`); changing CPU requests changes HPA utilisation
(a request cut raises the utilisation percentage and can trigger scale-out), so requests and replicas must not be changed together without isolation.
No real cluster, Prometheus or KubeCost has been available to validate anything so far, and the only metrics path in the new controller is `metrics.k8s.io`
(instantaneous, no history).

## Decision

1. Build the recommender as a **separate, read-only component**, not as a mode of the autoscaler. It has no write permission to workloads at all.
2. Split in phases, each shippable and testable on its own:
   * **Phase 1: pure library `operator/internal/recommend`.** Input: sample series per container (CPU usage, memory working set, CPU throttled ratio,
     OOMKilled count, restarts, optional p95 latency/SLO flag), current requests/limits, bounds. Output: a recommendation record with every field
     needed to reproduce it (window, sample count, p50/p90/p95/p99, formula, headroom, result, confidence, risks, reasons for abstaining). Deterministic,
     no I/O, table-tested including all abstention cases. Plus a thin command `cmd/recommend` that reads Prometheus range queries and prints the
     records as JSON: no cluster writes.
   * **Phase 2: surface.** A namespaced CRD `WorkloadRecommendation` written (status only) by the operator, so recommendations are auditable and
     diffable. Not before Phase 1 has run against a real Prometheus.
   * **Phase 3: application** (separate ADR): policy engine, approval, canary, previous-state record, verified rollback, GitOps detection.
3. Algorithm v1 (starting point, to be tuned on real data, not asserted correct):
   `request = max(p95 * (1 + headroom), floor)`, memory uses working-set p99 plus headroom, limits are not lowered below the observed max.
   Reductions are **vetoed** (the record says "no change" and why) when: fewer than N samples or window shorter than the configured minimum;
   OOMKilled in the window; throttling above a threshold; latency/SLO breached; workload excluded by label; metrics missing or stale.
   Increases are allowed under the same data-quality rule. Defaults for N, window and headroom are configuration, documented, with no claim of optimality.
4. Savings are reported only as **potential** (requests delta times a price source); any figure is labelled estimated unless a billing source exists.
   Reducing requests is not reported as infrastructure saving unless node capacity is shown to be released (out of scope for Phases 1-2).
5. Interaction rules for later phases: if an HPA targets CPU utilisation on the workload, a CPU request recommendation is flagged "affects HPA
   utilisation" and never applied together with a replica change; VPA in `Auto` mode on the same workload makes the recommender abstain.

## Alternatives considered

* **Extend `KubeCostAutoscaler`:** rejected. It would couple a statistical, history-based analysis to a control loop that must stay simple and single-writer,
  and would hand write permissions to code whose output is advisory.
* **Adopt VPA in recommendation-only mode and read its status:** cheaper to build, and worth a spike before Phase 2. Not chosen as the sole answer because
  VPA recommendations are not explainable per input, and the brief requires abstention rules and cost context. Decision to revisit after the spike.
* **New CRD and controller first:** rejected for now. More surface (RBAC, schema, upgrade) before any evidence that the numbers are good.
* **Engine in Python:** rejected for v1. The Go operator already owns Prometheus and Kubernetes access; one language for the data path avoids another
  network hop and auth surface. The Python engine stays as is.

## Consequences

* Phase 1 needs no new RBAC and no cluster, so it can be tested here; its quality still needs real Prometheus history (BLOCKED until available).
* Data source gap: `metrics.k8s.io` is not enough; Prometheus with cAdvisor and kube-state-metrics is a hard requirement, which must be documented.
* Adds one package and one command to maintain; the autoscaler is untouched.
* Risk: percentile settings tuned on synthetic data may be wrong. Mitigation: ship Phase 1 as report-only and compare against actual usage before any application.

## Verification plan

Unit tests for each veto, for the arithmetic and for determinism (same input, same record). Then, when available: run `cmd/recommend` against a kind cluster
with kube-prometheus-stack and an over-provisioned workload, and check the recommendation against measured usage. Until that run exists, the status is
IMPLEMENTED BUT NOT VERIFIED at best.
