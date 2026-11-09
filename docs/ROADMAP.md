# Roadmap

Ordered by what unlocks real use, not by what looks impressive. Items marked BLOCKED need something outside this repository.

## P0: before anyone relies on a recommendation or an apply

1. **Validate the recommender on real workloads over real time** (days, real traffic, real latency). The defaults (p95 + 15%, p99 + 20%, 200 samples, 24 h) are untuned. BLOCKED: needs a staging cluster with traffic.
2. **Run the lab E2E on a managed cluster** (EKS/GKE/AKS) and with a real KubeCost/OpenCost (`docs/PILOT.md` section 11). BLOCKED: cloud account and a cluster.
3. **Run the live KubeCost contract test** (`-tags kubecost_live`). BLOCKED: a real installation.
4. **Seasonality**: weekly/daily peaks are covered only by a long window; add an explicit check (compare windows) before trusting a short history.

## P1: product completeness

* Per-container and multi-container recommendations in one record; StatefulSet/DaemonSet apply.
* Detect HPA/VPA/exclusion in `cmd/recommend` from the cluster instead of manual flags.
* GitOps pull-request flow (docs/GITOPS.md) so GitOps-managed workloads can be optimised.
* Billing adapters that read provider exports (CUR/FOCUS, GCP, Azure) into the `cost` CSV shape; discounts/reservations; reconciliation against a real invoice. BLOCKED: real billing data.
* Outcome metrics exported to Prometheus (observed savings, rollback rate, recommendation acceptance), comparing equivalent periods.
* Recommendation CRD / API (ADR-0001 phase 2) once the numbers are trusted; then, only then, consider a controller that applies approved items.
* Approval with identity (who approved) instead of a bare annotation; audit shipping to append-only storage.
* Node-level optimisation beyond the candidate report (affinity, topology, PDB budgets) and integration with Karpenter/Cluster Autoscaler.

## P2: differentiation

Per-team/tenant policies, namespace budgets and cost-deviation alerts, savings reports, SLO-aware thresholds per workload. Not started; each needs an ADR (docs/adr).

## Done and verified in the local lab (2026-11-09)

Observe-only default, read-only recommender, policy engine, controlled apply with recorded state, automatic rollback (restart and SLO triggers), audit, Helm install/upgrade-off/rollback/uninstall: docs/LAB.md.
