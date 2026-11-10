# Rightsizing recommender (phase 1, read-only)

Status: **verified once against a real Prometheus (cAdvisor + kube-state-metrics) in the local lab** (docs/LAB.md), and by unit tests plus a mock Prometheus. **Not validated on real workloads over real time**: the defaults below are untuned.
Design and rationale: [ADR-0001](adr/0001-read-only-rightsizing-recommender.md). Applying a recommendation is a separate, policy-gated step ([ADR-0002](adr/0002-policy-gated-execution-with-rollback.md), docs/SAFETY.md). Not implemented: the recommendation CRD (phase 2).

It cannot change a cluster: `internal/recommend` is pure and `cmd/recommend` has no Kubernetes client.

## Run

```bash
go run ./cmd/recommend --prometheus-url http://prometheus:9090 --namespace shop --workload api --container app --window 168h
```

Needs Prometheus with cAdvisor (`container_cpu_usage_seconds_total`, `container_memory_working_set_bytes`, `container_cpu_cfs_*`) and
kube-state-metrics (`kube_pod_container_*`). If kube-state-metrics is missing the command **fails** instead of guessing. Pods are matched by naming
convention (`name-<hash>-<hash>` Deployment, `name-<n>` StatefulSet). Use `--now` to make a run reproducible.
Exit 0 = recommendation or abstention; exit 1 = no data, Prometheus down, bad input.
`--hpa-on-cpu`, `--vpa-auto`, `--excluded` are passed by hand for now; detecting them from the cluster is not implemented.

## Method (v1, untuned)

Samples are all (pod, step) values in the window. Percentiles use linear interpolation; NaN, Inf and negatives are dropped.

| Resource | Target | Rounding |
|---|---|---|
| CPU request | `p95 x (1 + 0.15)` | up to 10m, clamped to [min, max] |
| Memory request | `p99 working set x (1 + 0.20)` | up to 1 MiB, clamped to [min, max] |

* A difference below 10% is `NoChange`. A decrease is capped at 50% of the current request per step. A missing request is reported as `Increase`.
* Limits are never lowered; a limit below the new request is raised to it; an unset limit stays unset.
* **Abstain** (both resources, with reasons) when: fewer than 200 samples, window under 24 h, metrics older than 15 min, window inverted, all samples zero,
  workload excluded, VPA Auto owns the requests.
* **Decreases are refused** (increases still allowed) on: SLO breached (only if the SLO query returned data), any restart, CPU throttling >= 5% or not measured (CPU only),
  OOMKilled in the window (memory only).
* Confidence: High = at least 3x min samples and CV < 0.5; Medium = at least min samples and CV < 1.0; else Low.
* Potential saving is an **estimate**: requests delta x the unit price you pass x 730 h. It is not a billed or observed saving, and lowering requests
  does not lower the cloud bill unless nodes are actually released (not analysed).
* If an HPA scales on CPU, the record carries a risk note and a condition: do not change replicas and CPU requests in the same step.

Every record contains the formula with the numbers substituted, the stats (p50/p90/p95/p99/max/CV), the config used, the window, the sample count,
risks, conditions and a deterministic ID (cluster, kind, workload, container, window end).

## Tuning the defaults on your own data: `rightsize backtest`

The defaults are starting values. `rightsize backtest --prometheus-url ... --namespace ns --workload w --container c --window 336h --holdout 24h` replays a grid of (percentile, headroom) settings:
it computes each recommendation from the training part of the history only and measures it on the unseen most-recent part: the share of test samples above the recommendation (`ExceedFraction`),
the largest test sample over the recommendation (`PeakRatio`) and the waste. `Best` is the qualifying setting with the least waste under `--target-exceed` (default 1%). It also says when no setting can cover a peak the training period never saw.
One split is one sample of the future: repeat it with several holdouts, including one with a weekly peak, before changing any default. It was unit-tested and run against a mock Prometheus; it has **not** been run on real multi-week history.

## Known limits

* The defaults (percentiles, headroom, 200 samples, 24 h) are starting values, **not validated on real workloads**.
* No seasonality model: a 7-day window is the only protection against weekly peaks; a shorter window can miss them.
* Multi-container pods are analysed one container per run. OOM detection relies on `last_terminated_reason`, which only shows the most recent termination.
* The pod-name convention can match unrelated pods with similar names in the same namespace.
