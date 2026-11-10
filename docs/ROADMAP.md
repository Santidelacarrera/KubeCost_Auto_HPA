# Roadmap

Ordered by what unlocks real use, not by what looks impressive. Items marked BLOCKED need something outside this repository.

## P0: before anyone relies on a recommendation or an apply

1. **Validate the recommender on real workloads over real time** (days, real traffic, real latency). The defaults (p95 + 15%, p99 + 20%, 200 samples, 24 h) are untuned: a backtest on GKE (100 min of constant synthetic traffic) found them safe there but is no basis for tuning. BLOCKED: needs weeks of real traffic.
2. **Managed clusters and real billing**: GKE done (docs/LAB.md result 12, a real defect found and fixed there). Remaining: EKS and AKS, the applier and chart on a managed cluster, and a real FOCUS export (GCP billing export to BigQuery, set up by the owner).
3. **Kubecost as a cost source on a healthy installation**: the contract now holds against a real Kubecost 2.8.7 (a defect was found and fixed, docs/LAB.md result 11), but its data went stale on the lab install, so the freshness behaviour with Kubecost is unproven. Next: measure Kubecost's refresh cadence on a real cluster and decide `maxMetricAge`; try Kubecost 3.x.
4. ~~Seasonality~~ partly done: windows under 7 days cap confidence and add a risk note; a four-quarter stationarity check lowers confidence. A real weekly-pattern model is not built.

## P1: product completeness

* Per-container and multi-container recommendations in one record. (StatefulSet/DaemonSet apply, `rightsize plan` cluster-fact detection and a local-git GitOps proposal are done.)
* GitOps: Helm-values and Kustomize editors, and opening the pull request (docs/GITOPS.md).
* Billing adapters that read provider exports (CUR/FOCUS, GCP, Azure) into the `cost` CSV shape; discounts/reservations; reconciliation against a real invoice. BLOCKED: real billing data.
* Outcome metrics exported to Prometheus and *observed* savings from two equivalent periods. (`rightsize report` already derives acceptance rate, rollback rate and kept ESTIMATED savings from the audit trail.)
* (Done, opt-in) Recommendation CRD and applier controller: ADR-0003. Remaining: turning it on anywhere only after item 1 above. (Self-approval is now blocked by ValidatingAdmissionPolicy, and 60-object load and a 40 s API outage were exercised: docs/LAB.md result 10.)
* Audit shipping to append-only storage; a signed approval instead of an annotation. (Approver identity from SelfSubjectReview and an `approvers` allow-list are done.)
* Node-level optimisation beyond the candidate report (affinity, topology, PDB budgets) and integration with Karpenter/Cluster Autoscaler.

## P2: differentiation

Per-team/tenant policies (today: one policy ConfigMap per operator and the same-namespace rule), savings reports, SLO-aware thresholds per workload. Not started; each needs an ADR (docs/adr). Namespace budgets and cost-deviation alerts exist in `rightsize cost` (estimated basis unless billing data is supplied).

## Done and verified in the local lab (2026-11-09)

Real Argo CD and Flux, a two-node cluster compared with a real drain, real HTTP traffic and latency before/after, the opt-in applier (CRD + controller) on a real cluster: docs/LAB.md results 6-9.

Observe-only default, read-only recommender, policy engine, controlled apply with recorded state, automatic rollback (restart and SLO triggers), audit, Helm install/upgrade-off/rollback/uninstall: docs/LAB.md.
