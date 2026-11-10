# Roadmap

Ordered by what unlocks real use, not by what looks impressive. Items marked BLOCKED need something outside this repository.

## P0: before anyone relies on a recommendation or an apply

1. **Validate the recommender on real workloads over real time** (days, real traffic, real latency). The defaults (p95 + 15%, p99 + 20%, 200 samples, 24 h) are untuned. BLOCKED: needs a staging cluster with traffic.
2. **Run the pipeline on a managed cluster** (EKS/GKE/AKS) and with a real FOCUS export: the procedure and a read-only script are ready (docs/CLOUD_VALIDATION.md, `hack/cloud-validate.sh`); running them needs YOUR cloud account. BLOCKED.
3. **Run the live contract test against Kubecost** (it passed against OpenCost in the lab; Kubecost itself is not installed). BLOCKED: a Kubecost installation.
4. ~~Seasonality~~ partly done: windows under 7 days cap confidence and add a risk note; a four-quarter stationarity check lowers confidence. A real weekly-pattern model is not built.

## P1: product completeness

* Per-container and multi-container recommendations in one record. (StatefulSet/DaemonSet apply, `rightsize plan` cluster-fact detection and a local-git GitOps proposal are done.)
* GitOps: Helm-values and Kustomize editors, and opening the pull request (docs/GITOPS.md).
* Billing adapters that read provider exports (CUR/FOCUS, GCP, Azure) into the `cost` CSV shape; discounts/reservations; reconciliation against a real invoice. BLOCKED: real billing data.
* Outcome metrics exported to Prometheus and *observed* savings from two equivalent periods. (`rightsize report` already derives acceptance rate, rollback rate and kept ESTIMATED savings from the audit trail.)
* (Done, opt-in) Recommendation CRD and applier controller: ADR-0003. Remaining: an admission webhook to forbid self-approval, load testing with many objects, API-outage chaos tests, and turning it on anywhere only after item 1 above.
* Audit shipping to append-only storage; a signed approval instead of an annotation. (Approver identity from SelfSubjectReview and an `approvers` allow-list are done.)
* Node-level optimisation beyond the candidate report (affinity, topology, PDB budgets) and integration with Karpenter/Cluster Autoscaler.

## P2: differentiation

Per-team/tenant policies (today: one policy ConfigMap per operator and the same-namespace rule), savings reports, SLO-aware thresholds per workload. Not started; each needs an ADR (docs/adr). Namespace budgets and cost-deviation alerts exist in `rightsize cost` (estimated basis unless billing data is supplied).

## Done and verified in the local lab (2026-11-09)

Real Argo CD and Flux, a two-node cluster compared with a real drain, real HTTP traffic and latency before/after, the opt-in applier (CRD + controller) on a real cluster: docs/LAB.md results 6-9.

Observe-only default, read-only recommender, policy engine, controlled apply with recorded state, automatic rollback (restart and SLO triggers), audit, Helm install/upgrade-off/rollback/uninstall: docs/LAB.md.
