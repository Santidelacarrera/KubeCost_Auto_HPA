# GKE soak cluster, 2026-11-16

Generated 2026-11-16 03:44 UTC by `rightsize render` from the outputs of `plan`, `nodes`, `fleet` and `reconcile`. Read-only: producing this report changed nothing.

## Workloads

| Workload | Container | CPU | Memory | Confidence | Estimated USD/month | Signals | Main risk |
|---|---|---|---|---|---|---|---|
| lab/Deployment/shop | app | Decrease: 500m to 250m | Decrease: 128Mi to 64Mi | Medium | 4.11 |  | the window (120h0m0s) is shorter than 7 days: weekly seasonality is not observed |

1 to reduce, 0 to increase, 0 unchanged, 0 without a recommendation.

**Estimated** saving if every reduction were applied and the freed requests reclaimed: **4.11 USD/month**. Level: *estimated* (requests x the unit prices given to `plan`). Nothing was applied, and a requests reduction saves nothing until a node is released.

## Nodes

| Node | CPU requested | Memory requested | Pods | Candidate | Why |
|---|---|---|---|---|---|
| gke-kubecost-soak-default-pool-0275ac00-lb9p | 74% | 18% | 14 | no | pod gmp-system/gmp-operator-76d6bdb8b6-bkcwj is pinned (required node affinity (not evaluated)) |
| gke-kubecost-soak-default-pool-0275ac00-t4wh | 40% | 29% | 21 | no | pod kube-system/event-exporter-gke-5d7f7d58d9-xvcrq is pinned (hostPath volume) |

No node is a candidate for removal.

- based on CPU/memory requests; cordons, taints/tolerations, nodeSelector, zonal volumes, required per-node anti-affinity, PodDisruptionBudgets with no disruption allowed and the last node of a zone are honoured WHEN the caller supplies them; preferred affinities, topology spread constraints and inter...
- a removable node is a CANDIDATE: no saving exists until the node is actually released by an autoscaler/provider and the bill falls

## Clusters

| Cluster | Kubernetes | Provider | Nodes | CPU requested | Memory requested | Node candidates | Recommendations |
|---|---|---|---|---|---|---|---|
| gke_project-543a26ad-5f2e-4083-840_us-central1-a_kubecost-soak | v1.36.4-gke.2377000 | gce | 2 | 57% | 23% | 0 |  |
| kubecost-lab | v1.33.4 | unknown | 1 | 28% | 34% | 0 |  |
| kubecost-validate | unreachable | | | | | | Get "https://&lt;redacted-aks-api-host&gt;/api/v1/nodes": dial tcp: lookup &lt;redacted-aks-api-host&gt;: no such host |

2 of 3 cluster(s) read: 3 nodes, 5.5 of 15.9 CPU cores requested, 4.7 of 17.6 GiB requested.

## Billing: before and after

Level of the saving: **estimated**.
- Missing for the next level: the change has not been applied: this is requests x unit price

| | Before | After |
|---|---|---|
| Period | 2026-11-11 to 2026-11-12 | 2026-11-12 to 2026-11-13 |
| Billed usage (CLP) | -0.000115 | -0.000104 |
| Usage at list price (CLP) | 5538.962615 | 5538.988988 |

Difference on BilledCost: **0.000000 CLP per day**.
Difference at list price: -0.026373 CLP per day.

- no scope filter excluded anything: the comparison is of everything in the exports, so unrelated resources move the result
- a lower bill is attributed to the optimisation only if demand was equivalent in both periods; compare request volume before reading this as a saving

## How to read the savings in this report

estimated: a recommendation, nothing applied | projected: applied and kept, no cost measured | observed: cost measured in two equivalent windows, not an invoice | reconciled: two comparable provider bills show the invoiced amount fell.
