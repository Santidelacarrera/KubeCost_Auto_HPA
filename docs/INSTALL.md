# Installation

The chart installs in **observe-only** mode: the operator measures, recommends and reports, and writes nothing to your
workloads. Changing that is a separate, explicit step ([CONFIGURATION.md](CONFIGURATION.md)).

## Requirements

| Need | Version / detail | Why |
|---|---|---|
| Kubernetes | 1.33 or later. Run on 1.33.4 (lab, CI), 1.36.4 (GKE), 1.36.5 (AKS) | `KubeCostHPA` uses in-place pod resize (`pods/resize`, 1.33+). The applier's admission policies need 1.30+ |
| Helm | 3.16 (the version used) | Chart install |
| Prometheus | Scraping cAdvisor and kube-state-metrics | History for the recommender (see [COMPATIBILITY.md](COMPATIBILITY.md) for the series) |
| metrics-server | `metrics.k8s.io` | Demand signal of `KubeCostAutoscaler` |
| OpenCost (optional) | 1.121.3 was verified | Cost signal and budgets. Without it, leave `kubecost.url` empty |
| Cluster-admin for the install | CRDs, ClusterRoles | The operator itself runs with the reduced role in [RBAC.md](RBAC.md) |

Versions other than those listed are not validated. Kubernetes below 1.33 is not supported.

## Helm (recommended)

```bash
helm upgrade --install kubecost-auto-hpa \
  oci://ghcr.io/santidelacarrera/charts/kubecost-auto-hpa --version 0.1.0 \
  --namespace kubecost-auto-hpa-system --create-namespace \
  --set prometheus.url=http://prometheus-server.monitoring:80 \
  --set kubecost.url=http://opencost.opencost:9003
```

From a checkout, replace the OCI reference with `deploy/helm/kubecost-auto-hpa`.

Check it:

```bash
kubectl -n kubecost-auto-hpa-system get pods
```

```bash
kubectl -n kubecost-auto-hpa-system logs deploy/kubecost-auto-hpa-operator | grep -i observe
```

The second command must show the operator started in observe-only mode.

A mistyped value fails the install (`values.schema.json`) instead of being ignored. Tokens and certificates are never
values: they are referenced as Secrets (`auth.*.secretName`, `tls.engine.*`).

### Production values

`deploy/helm/kubecost-auto-hpa/values-production.yaml` raises resources and enables the network policy and TLS hooks. Review
it before use, and note it sets the webhook address: read it, do not apply it blind.

```bash
helm upgrade --install kubecost-auto-hpa deploy/helm/kubecost-auto-hpa \
  -n kubecost-auto-hpa-system --create-namespace \
  -f deploy/helm/kubecost-auto-hpa/values-production.yaml \
  --set prometheus.url=<your Prometheus> --set kubecost.url=<your OpenCost>
```

## kubectl (Kustomize)

```bash
kubectl apply -k operator/config/default
```

This installs the CRDs, RBAC, operator, engine and Redis into `kubecost-auto-hpa-system`. Set the Prometheus and cost URLs in
`operator/config/manager` (or a Kustomize overlay) before applying.

The base passes `--observe-only`, like the chart. Confirm it on what you are about to apply:

```bash
kubectl kustomize operator/config/default | grep -n "observe-only"
```

The binary's own default for that flag is **off**, so an overlay that replaces the operator's `args` also removes the safe
default. The overlays under `operator/config/overlays/` do exactly that: they are for tests on disposable clusters, and on them
the operator writes.

## The CLI

`rightsize` and `recommend` run on a workstation or in a job, with the kubeconfig of the person running them and the roles in
`deploy/rbac/rightsize.yaml`.

```bash
cd operator && go build -o ../bin/rightsize ./cmd/rightsize
```

```bash
kubectl apply -f deploy/rbac/rightsize.yaml
```

Bind `kubecost-rightsize-read` to whoever reads; bind the apply role per namespace, only to whoever may change workloads.

## GKE

Nothing provider-specific is required. Details, permissions and what was validated: [GKE.md](GKE.md).

* Standard clusters. Autopilot is not validated (it manages requests itself).
* Prometheus: self-managed (`prometheus-community/prometheus`) was used. Google Managed Prometheus exposes the same series but a
  different query endpoint and authentication: pass `--prometheus-bearer-token-file`.
* Spot nodes are recognised by `cloud.google.com/gke-spot`, `gke-preemptible` and `gke-provisioning`.

## AKS

Details: [AKS.md](AKS.md).

* The Helm lifecycle and the applier were run on AKS 1.36.5.
* For the cluster autoscaler to release a node that holds system pods, AKS needs the autoscaler profile
  `skip-nodes-with-system-pods=false`. That is your decision about your cluster, not something this product changes.
* Spot nodes are recognised by `kubernetes.azure.com/scalesetpriority=spot`.

## Upgrade

```bash
kubectl apply -f deploy/helm/kubecost-auto-hpa/crds/
```

```bash
helm upgrade kubecost-auto-hpa oci://ghcr.io/santidelacarrera/charts/kubecost-auto-hpa --version <new> \
  -n kubecost-auto-hpa-system --reuse-values
```

Apply the CRDs **first**: Helm never upgrades CRDs from `crds/`, and an operator that starts against an old CRD restarts until
they are applied. More in [UPGRADE.md](UPGRADE.md).

## Uninstall safely

1. Return to observe-only, so nothing is mid-change: `helm upgrade ... --set operator.observeOnly=true`.
2. Check no recommendation is being applied: `kubectl get workloadrecommendations -A` shows no `Applying` phase.
3. Delete the autoscaler resources while the operator still runs. Deleting a `KubeCostHPA` returns its pods to the template's
   resources through a finalizer; without the operator the finalizer cannot run.

   ```bash
   kubectl delete kubecosthpa,kubecostautoscaler --all -A
   ```

4. Uninstall the release:

   ```bash
   helm uninstall kubecost-auto-hpa -n kubecost-auto-hpa-system
   ```

5. Helm keeps the CRDs on purpose. Deleting them deletes every `WorkloadRecommendation` (your audit trail of approvals). Export
   them first if you need them, then:

   ```bash
   kubectl delete -f deploy/helm/kubecost-auto-hpa/crds/
   ```

Workloads changed by an applied recommendation keep their new requests after an uninstall. The previous values stay recorded
in the workload's annotations; `rightsize rollback` restores them without the operator.

## Persistence and recovery

The operator keeps its state in Kubernetes: the status of its custom resources, annotations on workloads, and Events. Redis
holds only short-lived caches (metric readings, cooldown timers); losing it loses nothing durable, and the operator rebuilds it.
There is no database to back up. To recover from a lost cluster, reinstall the chart and re-apply your custom resources and the
policy ConfigMap from Git. The audit trail survives only if you shipped it (`--audit-url`, [SAFETY.md](SAFETY.md)).
