# Feature status

Audit of 2026-11-15 (twelfth pass). One row per capability, classified by what the code does and what was actually run, not by
what a document says. This file is the source of truth: where another document disagrees with it, this one is right.

**Overall maturity: Beta.** Observe-only use is supported. Applying changes is ready for a supervised pilot. It is not validated
for unattended production (see "Why not higher" at the end).

## Legend

| Class | Meaning |
|---|---|
| **Verified** | Implemented, and exercised on a real cluster or real data. The evidence is named. |
| **Implemented, unit-tested** | Code and automated tests exist. It was not run against a real cluster or real data. |
| **Partial** | Part of what the name suggests exists. The row says which part. |
| **Documented only** | Described somewhere, accepted by a schema or shown in the console, but no code does it. |
| **Not implemented** | Does not exist. |
| **Out of scope** | Deliberately excluded from this phase. |

Availability for users: **Available** (use it), **Experimental** (works in tests, expect changes, do not rely on it), **Pending
validation** (needs a run on real infrastructure or data before anyone relies on it).

## Core: measure, recommend, apply, roll back

| Capability | Class | Availability | Evidence / what is missing |
|---|---|---|---|
| Observe-only default (operator writes nothing) | Verified | Available | Lab, GKE 1.36.4, AKS 1.36.5 (docs/LAB.md) |
| Rightsizing recommender: percentiles + headroom over a configurable history window, abstains on bad data | Verified | Available; defaults **pending validation** | Real Prometheus series in the lab, GKE and AKS. Defaults (p95+15%, p99+20%) are untuned: no weeks of real traffic |
| Vetoes: OOM kills, restarts, CPU throttling, breached SLO, HPA on CPU, VPA Auto, opt-out label | Verified | Available | Lab results 1-5 |
| Seasonality caveats: short window, weekday coverage, non-stationary quarters | Verified (weekday check on real soak data) | Available | LAB result 24. Not a seasonal model |
| **Growth trend and forecast** (daily p95, Theil-Sen slope, 14-day forecast folded into the CPU target) | Unit-tested; run once on 5 days of real GKE history (a weekend fall was correctly not read as growth: docs/LAB.md result 26) | Experimental | No real growing workload seen yet |
| **Spike (anomaly) detection** on CPU and memory samples | Implemented, unit-tested | Experimental | New in this pass. Reported as evidence; does not change a target |
| **OOM-risk signal** (working set within 10% of the limit) | Implemented, unit-tested | Experimental | New in this pass |
| Explanation and evidence in every record (formula, stats, reasons, risks, conditions, insights) | Verified | Available | Records in docs/reports/evidence |
| Policy engine (fail-closed): kill switch, allow/deny, bounds, confidence, windows, rate limit, age, approval | Verified | Available | Lab, GKE, AKS |
| Approvals: identity from the API server, optional Ed25519 signature, approver cannot be the publisher (admission policy) | Verified | Available | LAB results 10, 20, 21 |
| Apply with recorded previous state: Deployment, StatefulSet | Verified | Available | Lab, GKE, AKS |
| Apply: DaemonSet | Implemented, unit-tested | Experimental | Never run on a cluster |
| Automatic rollback on restarts, OOM, availability, SLO query, rollout timeout | Verified (restart, SLO, real latency); OOM trigger unit-tested | Available | LAB results 1, 9, 12, 14 |
| Manual rollback (`rightsize rollback`) | Verified | Available | Lab, incl. after `kill -9` (result 5) |
| Opt-in applier controller (`WorkloadRecommendation` CRD) | Verified | Available for a pilot; off by default | Lab 26/26, AKS 27/27 |
| Audit: JSONL, hash chain, shipping to an append-only collector, WORM storage | Verified | Available | LAB results 20-25 |
| GitOps: detection of Argo CD / Flux, proposals as commits or pull requests | Verified | Available | Real Argo CD 3.6.1, Flux 1.10.1, two real PRs |

## Autoscaling

| Capability | Class | Availability | Evidence / what is missing |
|---|---|---|---|
| `KubeCostAutoscaler`: cost-aware replica scaling, single-writer ownership, stabilisation windows, cooldown, failure policy, fallback HPA | Verified in kind + envtest (CI); cost source verified against OpenCost | Available in `dryRun`; writing **pending validation** on production traffic | CI jobs `envtest`, `e2e-kca`; LAB result 18 |
| Conflict avoidance with HPA, other autoscalers and `KubeCostHPA` | Verified | Available | envtest with a real HPA object |
| `KubeCostHPA`: SLO/cost-driven in-place resize of CPU and memory | Verified in kind (CI job `e2e-kind`) | Experimental | Never run on a managed cluster or real traffic |
| `KubeCostHPA` predictive pre-warming (`spec.predictive`) | Implemented, unit-tested | Experimental | Python engine + Go tests. No accuracy measured on real traffic |
| `KubeCostHPA` OOM protection (`spec.oomProtection`, raises the memory limit in place) | Implemented, unit-tested | Experimental | Never observed preventing a real OOM kill |
| PodDisruptionBudget respected on Spot drain (Eviction API) | Verified in envtest | Experimental | Never run against a real Spot interruption |

## Nodes

| Capability | Class | Availability | Evidence / what is missing |
|---|---|---|---|
| Read-only node candidate analysis (`rightsize nodes`) on requests | Verified | Available | Compared with a real drain on two nodes (LAB result 7) |
| Same, honouring cordons, taints/tolerations, nodeSelector, zonal volumes, anti-affinity, PDB with no disruption allowed, last node of a zone | Verified on real GKE and AKS after three defects found there were fixed (docs/LAB.md results 26, 29) | Experimental | Not compared with a real drain under these constraints |
| Pending-pod and node-pressure signals in the node report | Implemented, unit-tested | Experimental | New in this pass |
| Manual consolidation plan (`rightsize nodes --plan`): ordered steps, commands, verification, undo | Implemented, unit-tested | Experimental | Added 2026-11-16. A proposal: nothing executes it (ADR-0004) |
| Draining, cordoning or deleting nodes for consolidation | **Not implemented** | Not available | By decision (ADR-0004): nothing removes a node. GKE and AKS autoscalers do (docs/NODES.md) |
| Spot interruption drain (cordon + evict on a provider signal) | Implemented, unit-tested (envtest) | Experimental | GKE and AKS taints are recognised; never triggered by a real interruption |
| Preferred affinities, topology spread constraints, inter-pod affinity in the analysis | **Not implemented** | Not available | Stated in the report's caveats |

## FinOps

| Capability | Class | Availability | Evidence / what is missing |
|---|---|---|---|
| Cost allocation by namespace and workload, idle and unallocated buckets | Verified against OpenCost; allocation logic unit-tested | Available | LAB result 18 |
| Cost per request / per 1k requests | Verified at lab scale | Experimental | LAB result 9 (estimated price basis) |
| Budgets and cost-deviation alerts (`rightsize cost`) | Implemented, unit-tested | Experimental | Estimated basis unless billing data is supplied |
| FOCUS billing adapter: Google export | Verified on one real day | Available | LAB result 22 (account on credits: `ListCost`) |
| FOCUS billing adapter: Azure export | Implemented, unit-tested | Pending validation | Built from the documented shape; no real Azure export yet |
| **Bill reconciliation before/after** (`rightsize reconcile`): currency, period, tax, credits, purchases, unattributable rows | Unit-tested; an A/A control on two real consecutive Google billing days found no saving, as it must (docs/LAB.md result 26) | Pending validation | No real before/after pair with an invoice that fell |
| **Savings levels**: estimated, projected, observed, reconciled | Implemented, unit-tested | Available | New in this pass (docs/FINOPS.md) |
| Observed saving | Verified three times, small scale | n/a | GKE 0.067 USD/h (20-minute windows), AKS 0.096 USD/h, and the GKE soak: 2 nodes to 1 after five days of history, 0.067 USD/h (docs/LAB.md result 27). List price, test workloads |
| Saving reconciled with an invoice | **Not achieved** | n/a | Nothing has been confirmed against an invoice |
| Spend forecast | **Not implemented** | Not available | Only the per-workload CPU demand forecast exists |
| Kubecost as a cost source | Partial | Not recommended | Kubecost 2.8.7 free tier stalled in four runs; use OpenCost |

## Platform

| Capability | Class | Availability | Evidence / what is missing |
|---|---|---|---|
| Helm chart: install, upgrade, rollback, uninstall | Verified | Available | Lab 16/16, AKS 4/4 |
| Kustomize install | Verified in CI (kind) | Available | CI jobs `e2e-kind`, `e2e-kca` |
| Least-privilege RBAC, checked against a documented allow-list in CI | Verified | Available | `internal/rbaccheck`, lab-rbac 14/14 |
| GKE | Verified partially | Pending validation for production | docs/GKE.md |
| AKS | Verified partially | Pending validation for production | docs/AKS.md |
| AWS / EKS | **Out of scope** | Not available | No AWS integration is offered. See "AWS" below |
| Multi-cluster **read-only report** (`rightsize fleet --contexts a,b`): nodes, requested capacity, node candidates, recommendation phases per cluster, and totals | Verified on real clusters (GKE + lab + AKS, docs/LAB.md result 26) | Available | Added 2026-11-16. A snapshot through your kubeconfig; no central service |
| Multi-cluster management (central control plane, policies pushed to clusters) | **Not implemented** | Not available | One operator per cluster. The console's cluster tabs are samples |
| Namespace report in one command (`rightsize survey`) | Verified on the real GKE cluster (docs/LAB.md result 30) | Available | Added 2026-11-16. DaemonSets are left out |
| Readable report (`rightsize render`: Markdown from the outputs of `plan`, `nodes`, `fleet`, `reconcile`) | Verified on the real outputs of the GKE cluster (docs/LAB.md result 26) | Available | Added 2026-11-16. Copies figures and their savings level; adds none |
| Benchmark harness (`benchmarks/`) | Run once for real on the lab cluster against its own baseline (docs/LAB.md result 28) | Available | No competitor has been run |
| Web console | API-connected | Preview | `app.html` uses the enterprise API. `upstream-demo.html` retains the historical simulation. |
| Python engine endpoints `/v1/cast/*`, `/v1/gpu/*`, `/v1/greenops/*`, `/v1/gameday/*`, `/v1/cluster/topology`, `/v1/pricing/lookup`, `/v1/billing/invoice` | Partial: they compute from built-in example inputs | Demo | Since this pass every answer carries `data_source: sample` |
| Licence files: Ed25519-signed, verified offline; opt-in enforcement maps the tier to the operating mode and fails safe to observe-only | Verified on a real cluster under four licences (docs/LAB.md result 31) | Experimental | Added 2026-11-16 (docs/LICENSE_KEYS.md). The forced approval of the professional tier is unit-tested only; no licence issued to anyone |
| Billing and payments | **Not implemented** | Not available | `scripts/setup_stripe_billing.py` only has a dry-run that was ever run; the invoice endpoint is a pricing preview (docs/COMMERCIAL.md) |

## AWS

AWS is out of scope for this phase. Nothing new was built for it and it is not offered as an integration.

What exists in the code from earlier work, and why it was left: node labels and taints that mark Spot capacity and interruption on
EKS and Karpenter are still recognised next to the GKE and AKS ones (`internal/infra`, `internal/controller/node_controller.go`),
and the static price tables still contain AWS instance types. They are a few table rows with tests; removing them would change
behaviour for anyone who already runs the operator there and buys nothing. They were never validated on EKS and are not supported.

## Why not higher than Beta

A Release Candidate needs all of these, and none exists yet:

1. The recommender's defaults checked on weeks of real traffic (`rightsize backtest` is the tool; it needs your history).
2. A multi-day run on GKE and on AKS. The GKE soak has five days before its change and, so far, hours after it.
3. One saving reconciled with a provider invoice.
4. The automated mode (no human approval) exercised under real load.
5. An operator with several replicas riding out an API-server outage.


## Enterprise API extension

The additional Python control-plane workflows have separate status and evidence in [STATUS_ENTERPRISE_API.md](STATUS_ENTERPRISE_API.md), [FOLLOWUP_IMPLEMENTATION.md](FOLLOWUP_IMPLEMENTATION.md) and [MAINTENANCE_IMPLEMENTATION.md](MAINTENANCE_IMPLEMENTATION.md). Kubernetes CLI/applier licensing and Python API licensing are separate implementations. Their validation scopes must not be combined.
