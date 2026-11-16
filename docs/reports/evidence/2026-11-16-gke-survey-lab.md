# Namespace lab on gke_project-543a26ad-5f2e-4083-840_us-central1-a_kubecost-soak

Generated 2026-11-16 13:41 UTC by `rightsize render` from the outputs of `plan`, `nodes`, `fleet` and `reconcile`. Read-only: producing this report changed nothing.

## Workloads

| Workload | Container | CPU | Memory | Confidence | Estimated USD/month | Signals | Main risk |
|---|---|---|---|---|---|---|---|
| lab/Deployment/loadgen | gen | Increase: 50m to 140m | Increase: 16Mi to 32Mi | Low | 0.00 | no-memory-limit | the window (144h0m0s) is shorter than 7 days: weekly seasonality is not observed |
| lab/Deployment/shop | app | Decrease: 250m to 125m | Decrease: 64Mi to 32Mi | Medium | 2.06 |  | the window (144h0m0s) is shorter than 7 days: weekly seasonality is not observed |

1 to reduce, 1 to increase, 0 unchanged, 0 without a recommendation.

**Estimated** saving if every reduction were applied and the freed requests reclaimed: **2.06 USD/month**. Level: *estimated* (requests x the unit prices given to `plan`). Nothing was applied, and a requests reduction saves nothing until a node is released.

## Nodes

| Node | CPU requested | Memory requested | Pods | Candidate | Why |
|---|---|---|---|---|---|
| gke-kubecost-soak-default-pool-0275ac00-lb9p | 92% | 33% | 26 | no | pod kube-system/event-exporter-gke-5d7f7d58d9-4zvd4 is pinned (hostPath volume) |

No node is a candidate for removal.

- based on CPU/memory requests; cordons, taints/tolerations, nodeSelector, zonal volumes, required per-node anti-affinity, PodDisruptionBudgets with no disruption allowed and the last node of a zone are honoured WHEN the caller supplies them; preferred affinities, topology spread constraints and inter...
- a removable node is a CANDIDATE: no saving exists until the node is actually released by an autoscaler/provider and the bill falls

## How to read the savings in this report

estimated: a recommendation, nothing applied | projected: applied and kept, no cost measured | observed: cost measured in two equivalent windows, not an invoice | reconciled: two comparable provider bills show the invoiced amount fell.
