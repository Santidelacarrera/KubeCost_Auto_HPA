# Pilot guide

This guide is for a platform team that wants to evaluate KubeCost_Auto_HPA in a **non-production** cluster first.
It states what is verified, what is not, and the exit criteria before anyone should rely on it.

**Status:** the operator is not validated against a live KubeCost installation and has not been run on a real
workload cluster. Treat any pilot as an evaluation until the checks in §8 are complete.

## 1. Architecture (short)

* `KubeCostAutoscaler` (cluster-scoped operator controller) reads demand from `metrics.k8s.io`, cost from the KubeCost
  allocation API (`GET /model/allocation`, aggregated per controller), and writes replicas through the
  `deployments/scale` subresource. Optional in-place memory and CPU resize goes through `pods/resize`.
* The decision engine is pure Go (`internal/policy`): given demand, cost, bounds and history it returns one decision
  with its reasons. Its outcome is written to status and events; nothing else reads or writes the target.
* A single writer per target: if a native HPA or another autoscaler owns the workload, the operator does not write
  and reports a conflict.
* Failure states freeze scaling. A native HPA can be installed as a fallback only when resource metrics work, and it
  is removed on recovery.

Details: `docs/KUBECOST_AUTOSCALER.md`. Threats: `docs/THREAT_MODEL.md`.

## 2. Requirements and compatibility

| Component | Tested in CI | Evidence |
|---|---|---|
| Kubernetes | 1.33 (CI: kind 1.33 with metrics-server) | `e2e-kind`, `e2e-kca` in CI; envtest uses kube-apiserver 1.33.4 |
| metrics-server | Required for demand | `e2e-kca` |
| KubeCost / OpenCost allocation API | Shape from OpenCost source | **Not validated against a live installation.** Only fixtures and a simulated KubeCost in CI |
| Redis | Optional (cache, TLS supported) | `TestRedisDownDegradesGracefully` |
| Go (build) | 1.26 | `operator/go.mod` |

**Versions of KubeCost are not yet certified.** Record the exact KubeCost chart and app version in your pilot report.

## 3. Install (reproducible, non-production)

```bash
# 1. Create the namespace and the KubeCost token secret (key: token). Do not commit it.
kubectl create namespace kubecost-auto-hpa
kubectl -n kubecost-auto-hpa create secret generic kubecost-token --from-file=token=./kubecost-token.txt

# 2. Install with the optional controllers you need. Values are validated by values.schema.json.
helm upgrade --install kubecost-auto-hpa deploy/helm/kubecost-auto-hpa -n kubecost-auto-hpa \
  --set kubecost.url=http://kubecost-cost-analyzer.kubecost:9090 \
  --set auth.kubecost.secretName=kubecost-token   # key defaults to "token"

# 3. Confirm the RBAC you actually granted before going further.
helm template kubecost-auto-hpa deploy/helm/kubecost-auto-hpa | less   # review ClusterRole rules
```

The chart key names above are taken from `deploy/helm/kubecost-auto-hpa/values.yaml`; verify them against your chart
version with `helm template` before installing. `docs/RBAC.md` lists every verb and why it exists.

Start every target with `spec.dryRun: true` (as in `operator/config/samples/autoscaling_v1alpha1_kubecostautoscaler.yaml`)
and read `status.lastDecision` for at least two full cost windows before setting it to `false`.

## 4. KubeCost configuration

* Use one KubeCost installation per pilot cluster, reachable over HTTP(S) from the operator namespace.
* Create a token with read-only allocation access. The operator never needs write access to KubeCost.
* Prefer HTTPS with a certificate the operator trusts. The client refuses URLs with embedded credentials or query
  strings and never follows redirects.
* Multi-cluster KubeCost: set `spec.cluster`. Without it, an ambiguous match fails closed.
* Expect `CostNoData` for workloads younger than half of the cost window; this is by design.

## 5. Permissions

The operator's ClusterRole is generated from kubebuilder markers and mirrored in the chart. The restricted-user
envtest proves that a user bound only to that role can do what the operator needs and nothing else on the
resources tested. Re-run `make test-all` after any RBAC change. See `docs/RBAC.md`.

Namespace-scoped installs are not supported yet.

## 6. Failure policy

| Condition | Behaviour | Visible as |
|---|---|---|
| KubeCost unreachable, timeout, 5xx/429 | Retry (≤ 3 attempts), then freeze; a safety scale-up is still allowed when demand requires it | `Degraded`, condition reason `CostTransient` |
| KubeCost 401/403 | No retry; freeze | `CostAuth` |
| Malformed response or drifted field | No retry; freeze, never read as zero | `CostMalformed` |
| Sample older than `maxMetricAge` or window not covered | Freeze | `CostStale` |
| Workload not in cost data | Freeze | `CostNoData` |
| Resource metrics missing | Hold; fallback HPA only if its preconditions hold | `DemandUnavailable` |
| Kubernetes API errors | Nothing written; retry with backoff | Events, `reconcile` error metrics |
| Future stabilisation timestamp | Replaced with now once, window bounded | Status updated |

A freeze holds replicas where they are. The only write allowed during a freeze is an authorised safety scale-up
when demand is already past the safety ceiling (`TestFreezeStillAllowsAuthorisedSafetyScaleUp`).

## 7. Pause, recovery and rollback

* **Pause:** `kubectl annotate kubecostautoscaler <name> autoscaling.kubecost.io/pause=true`. The controller keeps
  computing and reporting decisions, forces dry-run, and writes nothing. Verified by unit and envtest tests.
  **Pause does not undo earlier scaling.** Replicas stay where they are.
* **Resume:** remove the annotation. No restart is needed. Stabilisation state is kept in status, so a resume
  does not bypass cooldowns.
* **Rollback of a single target:** delete the `KubeCostAutoscaler`. The finalizer restores the template resources of
  the target (requests and limits changed by in-place resize). Then set the Deployment's replicas as needed.
* **Rollback of the operator:** `helm rollback kubecost-auto-hpa <revision>`; CRDs are not removed by Helm, so
  objects survive a rollback.
* **Emergency stop of the whole operator:** scale the operator Deployment to zero. Targets keep their last replica
  count; no controller writes during that time.

## 8. Pilot exit criteria (before any production decision)

- [ ] Integration test `go test -tags kubecost_live -run Live ./internal/kubecost` passes against your KubeCost
      version, with the version recorded.
- [ ] `make test-all` passes on the exact commit being piloted.
- [ ] Dry-run decisions for ≥ 2 cost windows reviewed against what a human would have done.
- [ ] Pause and resume exercised on the pilot cluster; observed behaviour matches §7.
- [ ] Alerts (`docs/OBSERVABILITY.md`) fire in a deliberate KubeCost outage.
- [ ] Image scan, SBOM and signature verified on the image you actually deploy (`docs/SUPPLY_CHAIN.md`).
- [ ] Benchmark on the pilot's own traces (`docs/BENCHMARK_PROCEDURE.md`). Simulator results are not savings
      estimates for your cluster.

## 9. Known limitations

* No live KubeCost validation (see §2).
* Demand is CPU and memory from `metrics.k8s.io` only; custom or external metrics are not supported.
* Cluster-wide permissions only.
* Pause is a stop, not a rollback.
* The simulator is synthetic; no production latency or cost measurement exists yet.
* SLOs are proposed, not measured (`docs/OBSERVABILITY.md`).

## 10. Support

Report vulnerabilities as described in `SECURITY.md`. For functional issues, open a GitHub issue with the operator
version, the KubeCost version, the `KubeCostAutoscaler` status and the controller logs with secrets removed.
