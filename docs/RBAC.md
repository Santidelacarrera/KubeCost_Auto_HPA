# RBAC: what the operator may do and why

Audited against the code on the branch (not against the old markers). Source of truth for the
generated role is the `+kubebuilder:rbac` markers in `operator/internal/controller/*.go`
(`make manifests` writes `operator/config/rbac/role.yaml`); the Helm chart carries its own copy
(`deploy/helm/kubecost-auto-hpa/templates/rbac.yaml`) because it conditions two rules on
`operator.enableSpotDrain`. `operator/internal/rbaccheck` pins both to the allow-list below, so any
change in permissions is a deliberate, reviewed edit.

## Cluster-wide permissions (ClusterRole)

| Resource | Verbs | Needed by | Where in the code | Can it be reduced? |
|---|---|---|---|---|
| `events` | create, patch | event recorder (all controllers) | `Recorder.Event(...)` | no |
| `nodes` | get, list, watch | Spot drain controller; `KubeCostAutoscaler` classifies pods by node capacity class | `node_controller.go`, `kubecostautoscaler_controller.go` `nodeLookup` | no |
| `nodes` | **patch** | cordon/uncordon and the `drained` annotation | `actuator/drain.go`, `node_controller.go` | **Helm: only with `operator.enableSpotDrain=true`**. The kustomize base keeps it because the flag defaults to true |
| `pods` | get, list, watch | demand/utilisation and pod gates, drain, resize | cached client in both controllers, `drain.go` | no |
| `pods/eviction` | create | Spot drain (honours PodDisruptionBudgets) | `drain.go` `EvictV1` | **Helm: only with `enableSpotDrain=true`** |
| `pods/resize` | patch | in-place vertical resize (`KubeCostHPA`) | `actuator/resize.go` | no (needed by the legacy controller) |
| `deployments` | get, list, watch | read target, replicas, selector | both controllers | no |
| `deployments/scale` | get, update | the **only** replica write (compare-and-swap) | `actuator/scale.go` `GetScale`/`UpdateScale` | no |
| `horizontalpodautoscalers` | get, list, watch | detect a competing writer (single-writer rule) | `ownership/ownership.go` | no |
| `horizontalpodautoscalers` | create, patch, delete | the operator-owned fallback HPA (`ResourceMetricsFallback`) | `ownership/fallback.go` | not by verb (see below) |
| `kubecostautoscalers` | get, list, watch | the spec is read-only for the operator | `kubecostautoscaler_controller.go` | no |
| `kubecostautoscalers/status` | get, update | status and persisted stabilisation history | `persist()` | no |
| `kubecosthpas` | get, list, watch, update | read; `update` writes finalizers | `kubecosthpa_controller.go` | no |
| `kubecosthpas/finalizers`, `/status` | update; get, update | restore pods on delete; status | same | no |
| `metrics.k8s.io` `pods` | list | resource metrics for demand | `resmetrics.go` `PodMetricses().List` | no |
| `poddisruptionbudgets` | get, list, watch | a PDB selecting the Deployment makes legacy scale-down more cautious | `kubecosthpa_controller.go` `hasPDB` | no |
| `leases`, `events` (Role, release namespace) | leader election | controller-runtime | n/a | already namespaced |

### Removed in this pass (each one shown unused by the code)

`kubecosthpas`: create, delete, patch · `kubecosthpas/status` and `kubecostautoscalers/status`: patch ·
`pods/resize`: update · `deployments/scale`: patch · `horizontalpodautoscalers`: update ·
`metrics.k8s.io pods`: get · Helm only: `nodes: patch` and `pods/eviction` without Spot draining. The
Helm role had also drifted from the generated one (it granted `kubecosthpas` create/delete and
`kubecostautoscalers/status` patch); both now match.

### Evidence that the remaining set is sufficient

The real manager binary and every real-apiserver reconciler scenario (scale subresource + status,
competing native HPA, fallback HPA create/patch/delete, in-place resize, finalizer restore) run
against a real kube-apiserver 1.33 **as a user that holds only `config/rbac/role.yaml`**
(`restrictedOperator` in `internal/controller/envtest_test.go`). Negative controls: deleting the
`pods/resize` rule fails the manager test with `forbidden`, deleting `deployments/scale` fails the
scale test. Not exercised against a real apiserver with the restricted user: Spot draining
(`nodes: patch`, `pods/eviction`) and leader election; they are justified by code reading and the
drain unit tests only.

## What the controller can and cannot touch

* It cannot read Secrets or ConfigMaps, exec into pods, change RBAC, create workloads, or use `*`:
  enforced by `TestGeneratedClusterRoleEqualsTheAllowList` / `neverGranted`. KubeCost, Prometheus
  and engine credentials are mounted into the operator pod (Secret volumes/env), not read through the API.
* HPAs: create/patch/delete cannot be limited by resource name (the fallback name is derived per
  autoscaler). The code only mutates an HPA that carries a controller reference to the
  `KubeCostAutoscaler` (`ErrNotOurs` otherwise, `TestFallbackNeverTouchesForeignObjects`); other HPAs are read.
* Nodes: the only node write is cordon plus one annotation, on Spot nodes with an interruption signal.
* The legacy controller can resize pods and the horizontal controller scales Deployments **cluster-wide**,
  because targets live in any namespace. Namespace-scoped installs (a Role per namespace) are not supported yet.

## Remaining over-breadth (not reduced, with reasons)

1. `nodes: patch` and `pods/eviction` in the kustomize base (default Spot draining on). Use Helm with
   `operator.enableSpotDrain=false` for installs that only need `KubeCostAutoscaler`.
2. `pods/resize`, `kubecosthpas*`, PDB read are granted even if only `KubeCostAutoscaler` is used,
   because there is no flag that disables the legacy controller (a roadmap item).
3. ClusterRole instead of per-namespace Roles (cross-namespace targets).

This is **reduced and verified least privilege for the current feature set**, not minimal for every
possible install.

## Namespace-scoped design (evaluated, not implemented)

Evaluated against the generated ClusterRole in `operator/config/rbac/role.yaml`:

| Resource (rules) | Scope it needs | Can move to a per-namespace Role? |
|---|---|---|
| `deployments`, `deployments/scale`, `pods`, `pods/resize`, `pods/eviction`, `poddisruptionbudgets`, `horizontalpodautoscalers`, `kubecostautoscalers`, `kubecosthpas`, `events`, `metrics.k8s.io pods` | Only the namespaces that hold targets | Yes |
| `nodes` (get, list, watch, patch) | Cluster (nodes are not namespaced) | **No.** Needs a ClusterRole, read-only unless Spot draining is on |

What would change to support it:

1. The manager's cache would watch an explicit namespace list instead of all namespaces. This is a code change in
   `cmd/main.go` and is **not** done.
2. The chart would create one Role and RoleBinding per listed namespace, plus a read-only ClusterRole for `nodes`.
3. Envtest would need a case with two namespaces and a restricted user per namespace, to prove the negative
   (no access to a non-listed namespace).

Why it is not applied now: a permission reduction that is not run against a real multi-namespace install could break
reconciliation silently (for example, a target in an unlisted namespace is ignored, not reported as an error). The
current ClusterRole stays. Do not describe the operator as least-privilege beyond what the restricted-user envtest
proves (the verbs above, in one namespace set).

## Essential permissions (current state, reviewed in the latest validation round)

Cluster-scoped permissions that cannot be namespaced today:
- `nodes` get, list, watch: the classification of pods by capacity class and the Spot interruption signal.
- `nodes` patch: only with Spot draining (cordon and the `drained` annotation). Helm grants it only with `operator.enableSpotDrain=true`.

Every other permission is namespaced in effect (targets live in any namespace, so the ClusterRole is still
required for the watch). A namespace-scoped install is designed above but **not implemented or validated**.

Before reducing any verb: run `make test-all` (the restricted-user envtest checks allowed and denied verbs) and
the Helm lifecycle (`hack/helm-lifecycle.sh`). A reduction is only acceptable if both still pass and a
target in a namespace the operator should manage still scales.

## Restricted mode by namespace: design (not implemented)

Current mode (default, validated): one ClusterRole, the manager watches every namespace.

Restricted mode would scope the operator to an explicit list of namespaces. It is **not implemented**; this section
is the design that an implementation must satisfy before anyone relies on it.

**1. Manager and cache.** Add a flag (for example `--watch-namespaces=a,b`) that sets `cache.Options.DefaultNamespaces`
in `operator/cmd/main.go`. Informers then list and watch only those namespaces. Cluster-scoped objects (`nodes`) keep
their own cluster-wide informer. Namespace-scoped `Get` and `List` calls in the controllers already pass
`client.InNamespace`, so they do not need changes.

**2. Watches and reconciliation.** `For(KubeCostAutoscaler)`, `Owns(HPA)`, `Watches(Deployment)` and the PDB and pod
reads all become namespace-bounded. An object outside the list is never reconciled, and the controller must say so in
its status or logs rather than silently ignore it. The fallback HPA and the legacy `KubeCostHPA` follow the same list.

**3. Helm.** A new value (for example `rbac.namespaces: []`). When empty, the chart renders today's ClusterRole. When
set, it renders one Role and one RoleBinding per namespace, with the namespaced rules above, plus a ClusterRole that
grants only `nodes` get, list, watch (and patch when Spot draining is enabled). The schema must reject an empty list
when the mode is requested.

**4. Permissions that stay cluster-scoped.** `nodes` (see the table above) and the CRDs themselves, which the installer
creates with cluster rights. Nothing else needs cluster scope in restricted mode.

**5. Tests that must exist before this is enabled:**
- Envtest with two namespaces, `production` in the list and `staging` not in it. Create a `KubeCostAutoscaler` in each.
  Assert that the one in `production` is reconciled and the one in `staging` is never touched (no status, no replica
  writes, no events).
- Restricted-user envtest bound only to the generated per-namespace Roles: allowed verbs in `production`, denied verbs
  in `staging` and in cluster scope, except `nodes`.
- Helm lifecycle (`hack/helm-lifecycle.sh`) with `rbac.namespaces` set, and uninstall removes every generated Role and
  RoleBinding.
- Drift check: the Helm templates must produce the same verbs as the kubebuilder markers, per namespace.

**Why it is not enabled now.** Without the flag and the two-namespace tests above, a restricted install would fail in
a way that is easy to miss: informers denied for a namespace are logged as errors, but the controller would look
healthy for the namespaces it can see. The ClusterRole stays until those tests pass.
