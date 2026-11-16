# Custom resources

API group `autoscaling.kubecost.io`, version `v1alpha1`, all namespaced. The schemas with every field, default and validation
rule are the generated files in `deploy/helm/kubecost-auto-hpa/crds/` (identical to `operator/config/crd/bases/`).
`kubectl explain <kind>.spec` prints them from a cluster.

| Kind | Short name | Purpose | Status |
|---|---|---|---|
| `KubeCostAutoscaler` | — | Cost-aware replica scaling of one Deployment | Available in `dryRun`; writing pending validation on production traffic |
| `KubeCostHPA` | — | SLO- and cost-driven in-place resize of CPU and memory; optional replicas, predictive pre-warming, OOM protection | Experimental |
| `WorkloadRecommendation` | `wrec` | A published rightsizing recommendation, its approval and its outcome | Available for a pilot (applier off by default) |

## KubeCostAutoscaler

```yaml
apiVersion: autoscaling.kubecost.io/v1alpha1
kind: KubeCostAutoscaler
metadata: {name: payments, namespace: production}
spec:
  targetRef: {apiVersion: apps/v1, kind: Deployment, name: payments-api}   # same namespace, immutable
  minReplicas: 3
  maxReplicas: 20
  dryRun: true
  costPolicy: {maxHourlyCostUSD: "5.00", targetHourlyCostUSD: "4.00", scaleUpCostGuardrail: true}
  stabilization: {downscaleWindow: 10m, scaleUpWindow: 30s, minimumScaleActionInterval: 60s}
  failurePolicy: {mode: Freeze, maxMetricAge: 5m}
```

* One writer per target: with a native HPA, another autoscaler or a `KubeCostHPA` with replicas on the same Deployment, it
  writes nothing and reports `ScalingActive=False/Conflict`.
* Demand decides. Cost only bounds the decision: a lower cost never removes capacity by itself, and an exceeded budget never
  leaves an overloaded workload under-scaled.
* Invalid or stale cost or demand data freezes scaling (`failurePolicy`).
* Conditions: `Ready`, `MetricsAvailable`, `Degraded`, `ScalingActive`, and `UnsupportedFields` (below).
* `autoscaling.kubecost.io/pause=true` stops all writes of that autoscaler immediately.

**Fields accepted and not implemented:** `spec.predictive`, `spec.oomProtection`, `spec.nodeBinPacking`. They are kept so
manifests that set them still apply. They have no effect. While one is enabled, the status carries
`UnsupportedFields=True` with the list, and a Warning event is emitted once. Full reference:
[KUBECOST_AUTOSCALER.md](KUBECOST_AUTOSCALER.md).

## KubeCostHPA

```yaml
apiVersion: autoscaling.kubecost.io/v1alpha1
kind: KubeCostHPA
metadata: {name: checkout, namespace: shop}
spec:
  targetRef: {name: checkout}
  slo: {p99LatencyMs: 250}
  cpu: {min: 100m, max: "2"}
  memory: {min: 64Mi, max: 2Gi}            # opt-in
  replicas: {min: 2, max: 10}              # opt-in; never together with another HPA
  unitEconomics: {maxCostPerRequest: "0.000002"}
  budget: {monthlyLimitUSD: "5000", onSpendUnavailable: Ignore}
  predictive: {enabled: false}             # experimental
  oomProtection: {enabled: false}          # experimental
  dryRun: true
```

* Resizes pods in place (`pods/resize`, Kubernetes 1.33+). It refuses when the container's `resizePolicy` is
  `RestartContainer`.
* Never lowers CPU below `usage / 0.7` or outside `cpu.min`/`cpu.max`. Memory grows fast and shrinks slowly.
* A frozen budget blocks growth only. Memory still grows under a frozen budget: an OOM kill costs more than the spend.
* Deleting the resource returns the pods to the template's resources (finalizer, bounded to 2 minutes).
* `spec.metrics` overrides the PromQL queries per resource.

## WorkloadRecommendation

Written by `rightsize plan --publish`. The operator applies it only when the applier is enabled, the policy allows it and
`status.approval` names its exact `spec.recommendationID`.

| Field | Meaning |
|---|---|
| `spec.target` | Kind (Deployment, StatefulSet, DaemonSet), name, container |
| `spec.recommendationID`, `spec.record` | The full recommendation record that is applied, and its ID |
| `spec.summary` | Display only. `potentialMonthlyUSD` is an **estimate** |
| `spec.sloQueryName` | Name of an SLO query defined in the policy ConfigMap. Free-form PromQL is not accepted |
| `status.phase` | `Pending`, `DryRun`, `Applying`, `Verified`, `RolledBack`, `Denied`, `Failed` |
| `status.approval` | Recommendation ID, the approver as the API server reports it, time, optional signature |
| `status.reasons` | Why it was denied or rolled back |

Approving needs `update` on the `status` subresource, a different permission from creating the object. With
`enforceSeparationOfDuties` (default), admission policies require the approver to be the authenticated user and different from
the publisher.

## Versioning and compatibility

All three kinds are `v1alpha1`, served and stored. There is one version, so no conversion webhook exists.

* New optional fields may be added in a minor version. Existing objects keep working.
* A field is removed or renamed only in a new API version, with both versions served for at least one minor release and a
  conversion path documented in [UPGRADE.md](UPGRADE.md).
* Helm installs CRDs once and never upgrades them: apply `crds/` before every chart upgrade.
* Deleting a CRD deletes its objects. `helm uninstall` keeps the CRDs for that reason.
