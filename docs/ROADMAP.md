# Roadmap

Ordered by what unlocks real use, not by what looks impressive. Items marked BLOCKED need something outside this repository.

## P0: before anyone relies on a recommendation or an apply

1. **Validate the recommender on real workloads over real time** (days, real traffic, real latency). The defaults (p95 + 15%, p99 + 20%, 200 samples, 24 h) are untuned: a backtest on GKE (100 min of constant synthetic traffic) found them safe there but is no basis for tuning. A compressed simulated week of shoppers with daily/weekly cycles and flash sales (docs/LAB.md result 19): the CPU default held on the unseen days (0% exceeded), the memory default was tight (2.9% exceeded, unseen peak 1.35x; p99 + 15% held), and an applied change was kept through a simulated day with p95 86 -> 75 ms. Still generated load, not real users. BLOCKED: needs weeks of real traffic.
2. **Managed clusters and real billing**: GKE done (docs/LAB.md result 12, a real defect found and fixed there). AKS mechanics done (docs/LAB.md result 14) and **a node saving measured on AKS** once the autoscaler profile allowed it (`skip-nodes-with-system-pods=false`: one of two nodes removed, 50% of node cost at list price, result 17). The applier and the Helm chart on AKS: done, 27/27 in one run and the Helm lifecycle 4/4 (docs/LAB.md result 15). Remaining: EKS (owner deferred AWS). A seven-day GKE soak with Google's native FOCUS export and detailed export is running (docs/LAB.md result 13, report due 2026-11-17). Node matching on real Google FOCUS rows: done for one day against the soak cluster (result 22); the account is on credits, so the check used `ListCost`.
3. ~~Kubecost as a cost source~~ **resolved by using OpenCost**: Kubecost 2.8.7 free tier stalled ~10 min after its first data in four runs (results 11, 16 and 18), also with custom pricing (which did not stop its Azure API calls); the cause is not determined. OpenCost, the open-source model Kubecost is built on and the same `/model/allocation` API, served fresh data for 2 h on AKS and passed the live contract (result 18). That run also found a client defect: `window=1h` means the current clock hour in both, so coverage was refused in the first half of every hour; the client now asks for whole minutes, verified live against OpenCost (result 18). Remaining: Kubecost 2.9.x/3.x or with a real cloud integration, for teams that must use Kubecost.
4. ~~Seasonality~~ partly done: windows under 7 days cap confidence and add a risk note; a four-quarter stationarity check lowers confidence; a 7-day window whose data misses most of a weekday also caps confidence and names the day. A real weekly-pattern model (peak per weekday) is not built: it needs weeks of real traffic to be validated, and fitting it to the synthetic generator would prove nothing.

## P1: product completeness

Done in the ninth pass (2026-11-12; unit-tested, and the multi-container, signed-approval and audit-chain paths ran on a real cluster: docs/LAB.md result 20):

* ~~Per-container and multi-container recommendations in one record~~: `rightsize plan --container app,proxy`, one rollout, one rollback (docs/RECOMMENDER.md).
* ~~GitOps editors and the pull request~~: `rightsize propose --format helm-values|kustomize`, `--push --open-pr` (docs/GITOPS.md). Remaining: revert-commit rollback after a merge, and finding the source file from Argo CD / Flux objects.
* ~~Billing adapters for provider exports~~ (partly): FOCUS exports are read as delivered, with the provider's own currency conversion, BigQuery/Azure timestamps, Google's `ResourceName` and Azure scale-set splitting (docs/COST.md). Remaining, BLOCKED on data: a real export with node usage rows (the GCP billing export had none after 2026-10-31), a real Azure/AWS export, discounts/reservations, and reconciliation against an invoice.
* ~~Outcome metrics exported to Prometheus~~: acceptance, rollback ratio, kept request reduction and the kept ESTIMATE, from the objects (operator) or the audit trail (`rightsize report --format prometheus`) (docs/OBSERVABILITY.md). Remaining: *observed* savings from two equivalent periods of billed cost (needs the billing data above).
* ~~Audit shipping to append-only storage; a signed approval instead of an annotation~~: hash-chained `--audit-file` + `rightsize audit-verify`, `--audit-url` shipping, Ed25519 approvals with `requireSignedApproval` (docs/SAFETY.md). The operator ships its applier events too (`--audit-url`, chart `operator.recommendationApplier.auditURL` + `auditTokenSecret`).

Still open:

* (Done, opt-in) Recommendation CRD and applier controller: ADR-0003. Remaining: turning it on anywhere only after P0 item 1. (Self-approval is blocked by ValidatingAdmissionPolicy, and 60-object load and a 40 s API outage were exercised: docs/LAB.md result 10.)
* Node-level optimisation beyond the candidate report (affinity, topology, PDB budgets) and integration with Karpenter/Cluster Autoscaler.

## P2: differentiation

Per-team/tenant policies (today: one policy ConfigMap per operator and the same-namespace rule), savings reports, SLO-aware thresholds per workload. Not started; each needs an ADR (docs/adr). Namespace budgets and cost-deviation alerts exist in `rightsize cost` (estimated basis unless billing data is supplied).

## Done and verified in the local lab (2026-11-09)

Real Argo CD and Flux, a two-node cluster compared with a real drain, real HTTP traffic and latency before/after, the opt-in applier (CRD + controller) on a real cluster: docs/LAB.md results 6-9.

Observe-only default, read-only recommender, policy engine, controlled apply with recorded state, automatic rollback (restart and SLO triggers), audit, Helm install/upgrade-off/rollback/uninstall: docs/LAB.md.

## Prioritised backlog after the twelfth pass (2026-11-15)

Status of every feature: docs/STATUS.md. In order:

| # | Item | Why | Blocked on |
|---|---|---|---|
| 1 | Read the 7-day GKE soak report (due 2026-11-17) and act on it | First multi-day evidence | Time |
| 2 | Backtest the recommender on weeks of real traffic; tune or keep the defaults; validate the growth forecast and spike detection on that history | The defaults and the new insights are untested on real data | A cluster with real traffic |
| 3 | Reconcile one saving with an invoice (`rightsize reconcile` on two real exports, an account not on credits) | No level-4 saving exists | Billing exports |
| 4 | First real Azure FOCUS export through the adapter | The Azure side only saw fixtures | The owner's Cost Management export |
| 5 | Helm chart and applier on GKE; a multi-day run on AKS | Gaps in provider coverage | Approval of the cloud spend |
| 6 | Run the benchmark harness on a cluster (baseline, then this product), then against one competitor | The harness has never run; no comparison exists | A cluster; a vendor account |
| 7 | Automated mode under real load; several operator replicas through an API outage | Needed for a Release Candidate | 2 |
| 8 | ~~Remove the unused code~~ done in v0.2.0 (`internal/castsuite`, `internal/gpu`, `internal/psi`, `features.*`, the three ignored `KubeCostAutoscaler` fields). Left: the engine's sample endpoints and the demo console that calls them | Dead code and dead schema mislead | 12 |
| 9 | Licence check (signed file, verified offline) | First step of the commercial architecture | 2, 3 |
| 10 | Node consolidation as its own controller, with policy, approval and rollback | The largest missing capability | An ADR; 7 |
| 11 | Multi-cluster read-only view | Enterprise tier | 9 |
| 12 | Replace the demo console with one that reads the cluster | The console is sample data | 11 |
| 13 | Third-party security review | No external audit exists | Budget |
