# KubeCost Auto-HPA: final engineering report, 2026-11-09

Supersedes nothing: `2026-11-09_KUBECOST_AUTOHPA_ENGINEERING_AUDIT_REPORT.md` (earlier the same day) is kept as the starting point.
Words: **PASS / FAIL / BLOCKED / NOT RUN**. Classes: IMPLEMENTED AND VERIFIED, IMPLEMENTED BUT NOT VERIFIED, PARTIALLY IMPLEMENTED, SIMULATED, MISSING, BLOCKED BY EXTERNAL DEPENDENCY.
Nothing was pushed, tagged, published or deployed to a non-disposable cluster. Work is committed locally on branch `claude/kubecost-auto-hpa-prompt-91afa7`.

## A. Executive summary

* **Start:** an autoscaling operator (replica scaling, KubeCost cost policy, legacy in-place resize, Spot drain). No rightsizing recommender, no policy engine, no rollback, no real-cluster evidence in this environment (Docker daemon was off).
* **End:** the same operator plus a read-only recommender, a fail-closed policy engine, a controlled apply with recorded previous state, automatic rollback and audit, cost/capacity analysis libraries, an operator-wide **observe-only default**, and a reproducible lab. The lab was created (Docker Desktop started, dedicated minikube profile) and the new pipeline and the Helm chart were run on a **real Kubernetes 1.33.4** cluster: 22/22 and 16/16 checks.
* **Maturity:** a credible, honestly-bounded safe-rightsizing core for **Deployments in a lab**. Not a commercial product; not production-ready (section K).
* **Key honesty points:** the Obsidian project note claims "production-ready, full parity" with code that this checkout does not contain; KubeCost is a mock in the lab; the recommender's defaults are untuned; no managed cloud was available.

## B. Initial audit

| Component | Class at start | Evidence / problem |
|---|---|---|
| Kubernetes integration, controllers | IMPLEMENTED BUT NOT VERIFIED here | controller-runtime reconcilers, fake-client tests PASS; envtest on another machine in an earlier pass |
| Prometheus | PARTIALLY IMPLEMENTED | legacy CRD only; the new autoscaler used `metrics.k8s.io` only; no history, no throttling/OOM/restarts |
| KubeCost | IMPLEMENTED BUT NOT VERIFIED | fixtures only; live test never run |
| Recommendation engine | MISSING | none |
| Cost engine | PARTIALLY IMPLEMENTED | money type and allocation sample; no billed/allocated separation, no reconciliation |
| Policy & safety, execution, rollback | MISSING (only `dryRun` and `pause`) | none |
| GitOps / HPA-VPA detection | MISSING (ownership of replicas existed) | none |
| Security | PARTIALLY IMPLEMENTED | reduced RBAC verified in a prior pass; supply chain unverified |
| Testing | PARTIALLY IMPLEMENTED | 15 Go packages + 27 Python tests; one Windows-only test failure (fixed) |

Debt/risks found: Obsidian note vs repo contradiction; `TestVersionIsInjectedAtLinkTime` failed on Windows; no global observe-only; no way to say "this workload is over-provisioned".

## C. Implementations

| # | Name | Problem | Solution | Key files | Result |
|---|---|---|---|---|---|
| 1 | Observe-only mode | writes gated only by per-resource `dryRun` | `--observe-only`, chart `operator.observeOnly` (default true); both controllers behave as dry-run, Spot drain not started | `cmd/main.go`, `internal/controller/*`, chart | PASS: unit tests + real cluster (replicas stay 4; flipping the value scales to 1) |
| 2 | Recommender (ADR-0001) | none existed | percentile/headroom algorithm with abstention and vetoes; Prometheus reader; CLI | `internal/recommend`, `.../promsrc`, `cmd/recommend` | PASS: unit + mock + one real-Prometheus run; defaults untuned |
| 3 | Policy engine (ADR-0002) | nothing decided if a change was allowed | fail-closed pure function: kill switch, allow/deny, labels, bounds, confidence, windows, rate limit, approval by exact ID, GitOps/HPA/VPA refusals, idempotency | `internal/safety` | PASS: unit; denials also verified on the real cluster |
| 4 | Execution + rollback + audit | no apply path | apply with previous state on the object, resourceVersion update, observation (restarts, OOM, availability, replicas, SLO query, rollout timeout), verified rollback, JSONL audit | `internal/execute`, `cmd/rightsize` | PASS: fake + real cluster; **bug found and fixed on the real cluster** (rollback conflict) |
| 5 | Cost intelligence | no separation of estimated/billed/allocated | allocation, shared/idle/unallocated, reconciliation, CSV billing adapter, observed-savings comparison | `internal/cost` | PASS: unit with synthetic fixtures; not validated against a bill |
| 6 | Capacity analysis | requests reduction is not a cost reduction | candidate-node analysis (FFD on requests), stranded capacity | `internal/capacity`, `rightsize nodes` | PASS: unit; run on the 1-node lab (nothing removable); bug found by a test and fixed |
| 7 | Reproducible lab | no cluster evidence | `lab-up/e2e/helm/down` scripts, mock KubeCost, workloads, evidence logs | `hack/lab*`, `docs/reports/evidence/` | PASS |
| 8 | Docs | | ARCHITECTURE, SAFETY, COST, COMPATIBILITY, GITOPS, LAB, ROADMAP, TROUBLESHOOTING, RECOMMENDER, 2 ADRs, README section | `docs/` | done |

Decisions: ADR-0001 (separate read-only recommender), ADR-0002 (policy-gated execution as a human-run CLI, state on the object, no controller). Not added on purpose: a recommendation CRD, an applying controller, a database, new microservices (docs/ARCHITECTURE.md "Not built").

## D. Tests executed (Windows 11; Go 1.27.3; Python 3.12.10)

| Command | Result |
|---|---|
| `gofmt -l .`, `go vet ./...` | PASS |
| `go test -count=1 -p 2 ./...` (23 test packages incl. new `recommend`, `promsrc`, `safety`, `execute`, `cost`, `capacity`, `cmd/recommend`, `cmd/rightsize`) | PASS |
| `pytest` (engine) | PASS, 27 |
| `helm lint`, `helm template` | PASS |
| `bash hack/release-dry-run.sh v0.1.0` | PASS (nothing built/signed/published) |
| Negative controls (OOM veto off; rollback off) | tests FAIL as expected (1 and 4), restored, PASS |
| Failure injection, mock level: Prometheus 500 / down / non-JSON / no kube-state-metrics / NaN; API conflict; failed rollback; spec changed mid-apply; unknown facts | PASS (unit, fakes/mocks) |
| **Real cluster** `hack/lab-e2e.sh` | **PASS 22/22** (`docs/reports/evidence/2026-11-09-lab-e2e.log`) |
| **Real cluster** `hack/lab-helm.sh` | **PASS 16/16** (`.../2026-11-09-lab-helm.log`; KubeCost is a MOCK) |
| `go test -race` | BLOCKED (no cgo) |
| envtest against kube-apiserver binaries | NOT RUN (no assets) |
| GitHub CI on these changes | NOT RUN |
| kind / k3d | NOT RUN (minikube used) |
| EKS/GKE/AKS, real KubeCost, real billing, real traffic/latency | BLOCKED |

## E. Kubernetes real

* Environment: minikube v1.39.0 profile `kubecost-lab`, docker driver, **Kubernetes v1.33.4**, containerd 2.3.4, one node, 4 CPU / 5 GB (Docker Desktop 29.8.0, WSL2). The user's pre-existing `minikube` profile was not touched.
* Installed: metrics-server v0.9.0 (addon), `prometheus-community/prometheus` 29.46.0 (Prometheus v3.16.0, kube-state-metrics 2.20.0), this repo's chart (operator, engine, Redis, 2 CRDs) from locally built images, a mock KubeCost.
* Verified operations: see docs/LAB.md. Highlights: recommendation from real series; denial cases; approved apply 1 CPU / 1 GiB to 500m / 512Mi; rollback on a real container restart and on an SLO signal, with values verified restored; chart install, observe-only behaviour, upgrade to act, rollback, uninstall (CRDs kept by Helm).
* Limits: single node; synthetic load and SLO; mock KubeCost; no GitOps, HPA or VPA objects exercised; short observation windows (60-120 s); lab-relaxed minimums.

## F. Before / after (only what was measured)

| Area | Before | After |
|---|---|---|
| Safe default | per-resource `dryRun` | operator-wide observe-only, chart default |
| Rightsizing | none | recommend / authorise / apply / observe / rollback, verified on a lab cluster |
| Real-cluster evidence in this environment | none | 38 checks (22 + 16) |
| Go test packages | 15 | 23 |
| Cost semantics | single number | estimated / billed / allocated, reconciliation (unit-level) |
| Savings numbers | none | **none claimed**: the lab only shows requests 1 CPU / 1 GiB reduced to 500m / 512Mi on a synthetic workload; no infrastructure saving was measured |

## G. Versus Cast AI, StormForge, Kubecost

Sources were **not re-fetched in this pass**; nothing below is attributed to them. What can be said is only about this repository: it implements a narrow, auditable slice (CPU/memory request recommendations for Deployments with abstention rules, policy gating, verified rollback, audit)
and none of: spot automation, cluster autoscaling, bin-packing execution, multi-tenant allocation UI, real billing integration, or fleet management. Parity is not claimed. Differentiation opportunities that this design already leans into: explainable per-input records, abstention over guessing,
rollback that was exercised on a real cluster, a stated separation of estimated/billed/allocated, GitOps refusal instead of silent fights. A competitor comparison with cited sources is pending.

## H. Pending and blockers

* **P0:** validate recommender defaults on real workloads over days; run on a managed cluster; real KubeCost contract test (all BLOCKED on infrastructure).
* **P1:** HPA/VPA/exclusion detection inside `cmd/recommend`; GitOps pull-request flow; billing adapters + real-invoice reconciliation; outcome metrics exported to Prometheus; identity for approvals; StatefulSet/DaemonSet; recommendation CRD.
* **P2:** tenants/teams, budgets, deviation alerts, savings reports.
* **External:** cloud cluster, KubeCost/OpenCost, billing exports, real traffic, CI run, cgo toolchain for `-race`, envtest binaries.

## I. Maturity (0-5, from behaviour that was actually run)

| Dimension | Score | Why |
|---|---|---|
| Kubernetes integration | 3 | real 1.33.4 lab, chart and CRDs work; one node, no managed cloud |
| Metrics collection | 3 | real cAdvisor/kube-state-metrics via Prometheus; pod-name convention; no latency unless an SLO query is given |
| Recommendation engine | 2 | works on real data once; untuned; no seasonality |
| Cost intelligence | 1 | logic + tests; no real billing or adapters |
| HPA/VPA compatibility | 1 | detection and refusal unit-tested; not run against real HPA/VPA |
| Rightsizing | 2 | end to end on Deployments in the lab; synthetic workload |
| Node optimisation | 1 | candidate report only; 1-node lab |
| Policy & safety | 3 | fail-closed, negative cases run on the real cluster |
| Execution engine | 3 | run on the real cluster; a real race found and fixed |
| Rollback | 3 | restart and SLO triggers exercised for real; OOM trigger unit-tested only |
| GitOps integration | 1 | detection/refusal only |
| Security | 2 | approval is an annotation anyone with update rights can write; actor unauthenticated; supply chain unverified |
| Reliability | 2 | conflict handling, idempotency, fail-safe on watch errors; no soak or chaos beyond the injected faults |
| Observability | 2 | audit JSONL and operator metrics; no new exported metrics for the rightsizing flow |
| Testing | 3 | 23 Go packages, negative controls, 38 real-cluster checks; no CI run, no envtest/race here |
| Documentation | 3 | install-to-rollback documented; managed-cloud and real-data docs absent because they were not possible |

## J. Obsidian

* MCP `obsidian`: worked (search, read, append). Read: `1_Projects/KubeCost_Auto_HPA.md`; a search hit on an unrelated ADR (CloudCost_Optimizer) was not used.
* **Contradiction recorded:** the note's "production-ready / full parity / web, saas.py, Stripe, GPU" claims do not match this checkout. The note should be corrected by its owner; it was not overwritten (appends only).
* Updated: three dated appends to the project note (audit + observe-only; ADR-0001 and recommender phase 1; execution/lab results at the end of this pass). Decisions recorded: ADR-0001, ADR-0002, the real-cluster conflict finding.
* Pending in the vault: correct the stale status claims; link this report.

## K. Verdict

* Does it really work? The new pipeline and the chart worked on a real lab cluster; the product as a whole is not proven on real workloads.
* Connected to real Kubernetes? Yes, a real 1.33.4 single-node lab cluster (not a managed or production cluster).
* Real metrics? Yes for the recommender (real cAdvisor/kube-state-metrics); the SLO signal in the rollback test and the load are synthetic.
* Real or estimated costs? **Estimated or simulated only.** No real billing was available; KubeCost is a mock.
* Can it apply changes safely? In the lab, under a policy and a human approval, with rollback: yes. For production: not demonstrated.
* Can it roll back? Yes, shown for a real container restart and an SLO signal; OOM trigger unit-tested only.
* Ready for a commercial demo? A technical demo of the safety pipeline on a lab cluster: yes, with these caveats said aloud. A savings demo: no, there is no measured saving.
* Ready for production? **No.**
* Missing to compete commercially: real-workload validation, managed-cloud validation, real KubeCost/billing, GitOps PR flow, tenancy and identity, outcome measurement of actual savings, UI/reporting, CI and release evidence.
* Next engineering priority: run the lab E2E on a staging cluster with real traffic and a real KubeCost, and use that data to tune or reject the recommender defaults.


## L. Addendum (same day, second pass): what was closed and what was not

Requested by the owner: do what remains of the prompt. Done where this machine allows; everything that needs a cloud account, credentials, real billing or real traffic stays BLOCKED.

| Item | Before this addendum | Now | Evidence |
|---|---|---|---|
| `-race`, envtest on a real kube-apiserver, CI | BLOCKED / NOT RUN here | **PASS in GitHub Actions** (`engine`, `operator` incl. `go test -race` and govulncheck, `manifests`, `envtest`, `e2e-kind`, `e2e-kca`) on the pushed commit b7ba429; later commits are not covered by that run | docs/LAB.md |
| Live KubeCost contract | mock only | **PASS against a real OpenCost 1.121.3** (default pricing, via the OpenCost UI service). Kubecost itself still not run | docs/reports/evidence/2026-11-09-opencost-live-contract.log |
| HPA / VPA detection | unit tests | **PASS on a real HPA and the real VPA CRD object** (no VPA controller) | evidence lab-e2e2 |
| StatefulSet / DaemonSet | not supported | StatefulSet **PASS on the real cluster**; DaemonSet unit-tested only | evidence lab-e2e2 |
| Approval identity | annotation | stamped with the API server's identity; policy `approvers`; **PASS** | evidence lab-e2e2 |
| Minimal RBAC for the CLI | undocumented | `deploy/rbac/rightsize.yaml`, **PASS 14/14** by impersonation | evidence lab-rbac |
| Interruption recovery | untested | kill -9 mid-watch then rollback from saved state, **PASS 7/7** | evidence lab-crash |
| GitOps | detection | detection + `rightsize propose` (local git branch; unit + lab) | evidence lab-e2e2 |
| Seasonality | none | window-length cap + stationarity check (heuristic, not a seasonal model) | unit tests |
| Outcome metrics | none | `rightsize report` from the audit (process metrics + ESTIMATED savings) | unit tests + lab |
| Cost budgets / deviation alerts | none | implemented on the estimated basis | unit tests + lab |
| Competitor comparison with sources | pending | docs/COMPETITIVE.md (4 pages read 2026-11-09; paraphrased, not tested) | docs |
| Original 22-check E2E after the executor refactor | | **PASS 22/22 again** | .lab rerun |
| Go tests | 23 packages | **25 packages PASS** locally (Windows, no `-race`) | |

**Still BLOCKED or not done:** EKS/GKE/AKS (no cloud credentials may be entered by me); real billing and any measured saving; real traffic/latency; Kubecost itself; Argo CD / Flux installed;
multi-node cluster (the Docker Desktop VM has no memory for a second node); chart upgrade between two released versions; a recommendation CRD and an applying controller (deliberately not built, ADR-0001/0002);
multi-tenancy; defaults of the recommender tuned on real workloads. The verdict in section K does not change: **not production-ready, no savings demonstrated**.

Maturity changes (0-5): Metrics collection 3 (unchanged); Recommendation engine 2 (unchanged); Cost intelligence 1 to 2 (real OpenCost contract + budgets, still no real bill); HPA/VPA compatibility 1 to 2 (real objects detected);
GitOps 1 to 2 (proposal flow); Security 2 to 3 (identity-stamped approval, proven minimal RBAC); Reliability 2 to 3 (interruption recovery proven); Observability 2 (unchanged: report from audit only, no exported metrics);
Testing 3 to 4 (CI incl. race/envtest green; 82 real-cluster checks over five scripts: 22 + 16 + 23 + 14 + 7); Node optimisation 1 (unchanged, single node).


## M. Addendum 2 (third pass): what the owner asked for after section L, and what is still outside my reach

The owner's list was: cloud (EKS/GKE/AKS), real billing and measured savings, real traffic and latency plus tuning the recommender defaults, Argo CD / Flux as real controllers, a multi-node cluster, and the CRD and applying controller that had been left out on purpose.

| Item | Result | Evidence |
|---|---|---|
| Argo CD and Flux as real controllers | **Done and verified, twice from an empty cluster**: both detected from the marks they really write (Argo CD v3.6.1 annotation; Flux v1.10.1 labels), a direct patch is reverted by both, and `propose` -> merge -> push is applied by both. 15/15 and 15/15 | LAB result 6 |
| A multi-node cluster | **Done**: real two-node cluster, the tool's prediction compared with a real drain, both the "fits" and the "does not fit" case. 9/9. (Two nodes, minikube; not a managed node pool) | LAB result 7 |
| Real traffic and latency | **Done at lab scale**: 35-37 requests/s, real latency histogram, equivalent windows: p95 4.75 ms before and after, no throttling, no restarts, with the requests halved; rollback driven by the real latency metric. Trivial workload on a laptop VM, so it shows the mechanism, not production behaviour | LAB result 9 |
| Tuning the recommender defaults | **Tool built, tuning NOT done**: `rightsize backtest` replays settings on your own history (train on the past, test on the unseen recent part). It needs weeks of real history, which only your clusters have. The defaults remain untuned | docs/RECOMMENDER.md |
| Recommendation CRD and an applying controller | **Built as opt-in (off by default) and verified on a real cluster** (ADR-0003). Three real defects found there and fixed (ConfigMap read through the cache; final phase overwritten by a stale read; confidence lowered by noise below the floors). No single 26/26 run exists: scenarios 1-7 and scenario 8 passed in two runs of the same final code | LAB result 8 |
| Real billing | **Adapter built, not validated**: a FOCUS CSV parser with provider-id mapping (negative credit rows handled after a unit test caught it failing). It has only seen hand-written fixtures. Needs your first real export | docs/COST.md, CLOUD_VALIDATION.md |
| EKS / GKE / AKS | **Not done and cannot be done by me**: it needs your cloud account and credentials. Delivered instead: `docs/CLOUD_VALIDATION.md` (provider FOCUS export sources read on 2026-11-09, cluster templates marked as not executed) and `hack/cloud-validate.sh`, a **read-only** script you run against your own context | docs, script (the script itself was syntax-checked and its building blocks ran on the lab; it was not run against a cloud) |
| Measured savings | **Still none.** The lab shows requests halved with unchanged latency and an ESTIMATED cost per 1k requests; no bill, no released node | |

Verification of this pass: 25 Go test packages and 27 Python tests pass locally (Windows, no `-race`); the default Helm render contains nothing of the applier; GitHub CI is run on each pushed commit.
Defects found by real systems during the pass (all fixed with tests): the applier's ConfigMap read, the applier's stale-cache overwrite, noise-driven low confidence (CPU and memory), the FOCUS credit-row parse failure, and the rollback conflict reported in section L.

**What I need from you to go further:** (1) a cluster you own (managed or not) with Prometheus and at least a week of real traffic, and (2) one billing export in FOCUS CSV for the same period. With those, `hack/cloud-validate.sh` and `rightsize backtest` produce the evidence that turns the remaining "untuned" and "not validated" items into results.

Updated maturity (0-5): Recommendation engine 2 (+ backtest tool, still untuned); Cost intelligence 2 (+ FOCUS adapter, unvalidated); HPA/VPA compatibility 2; GitOps 3 (real Argo CD and Flux); Node optimisation 2 (real two-node comparison);
Execution engine 3; Rollback 3-4 (restart, SLO, real-latency trigger and applier rollback all exercised for real; OOM trigger unit-tested only); Policy & safety 3-4 (separate publish/approve permissions, per-namespace RBAC, kill switch without restart, all exercised); Security 3; Reliability 3 (restart resume, stale-cache guard);
Observability 2; Testing 4; Documentation 4. Production readiness: still **no**.


## N. Addendum 3 (fourth pass): hardening of the opt-in applier

| Item | Result | Evidence |
|---|---|---|
| Separation of duties | **Enforced by the API server**: three ValidatingAdmissionPolicies (CEL, no webhook). The publisher stamp must be the real user, the approver must be a different real user, the stamp is immutable. Verified with two real users by impersonation, and the switch-off verified too | LAB result 10 |
| Chart upgrade from the pre-work chart (534c64d), rollback, re-upgrade | **PASS**. Known limitation verified: `helm upgrade` does not install a new CRD from `crds/`; the operator restarts until `kubectl apply -f deploy/helm/kubecost-auto-hpa/crds/` (docs/TROUBLESHOOTING.md) | LAB result 10 |
| Load: 60 approved recommendations at once (observe-only) | **PASS**: all reach DryRun, no reconcile errors, operator 49 MiB | LAB result 10 |
| kube-apiserver stopped ~40 s during two observations | **PASS** in the final run (both Verified, applied once). One earlier run rolled both back; a probable cause (blind time counted against the rollout timeout) was found, reproduced in a unit test and fixed. Intermittent before, so one green run is consistent with the fix, not proof of it | LAB result 10 |
| Per-namespace policy, applier metrics and alerts | implemented; metrics seen on the real cluster; per-namespace policy unit-tested | unit tests, LAB result 10 |

Final lab run: **26/26**. After the fix: 25 Go test packages and 27 Python tests pass locally (Windows, no `-race`), gofmt and go vet clean, `go mod tidy` without changes, `helm lint` passes with the applier off and on. CI runs on the pushed commit. Not covered: an outage longer than the 2-minute tolerance on a real cluster, a multi-replica operator with leader election, any managed cloud.
Nothing in this pass changes the verdict: **not production-ready**; no measured saving; recommender defaults untuned; EKS/GKE/AKS and real billing need the owner's account (section M).

Maturity changes: Security 3 to 4 (admission-enforced separation of duties); Reliability 3 to 3-4 (API outage ridden out, one defect fixed); Observability 2 to 3 (exported applier metrics and alerts).


## O. Addendum 4 (fifth pass): the owner's five remaining items

| Item | Result |
|---|---|
| 1. EKS / GKE / AKS | **NOT DONE.** The AWS CLI and gcloud on this machine are installed, but both sessions are expired (`aws login`, `gcloud auth login`); signing in is the owner's action, and creating a managed cluster spends the owner's money, which needs the owner's explicit go-ahead with a cost estimate. Runbook and read-only script unchanged (docs/CLOUD_VALIDATION.md, `hack/cloud-validate.sh`) |
| 2. Real FOCUS billing export | **NOT DONE**: no export exists on this machine; it comes from the owner's cloud billing account |
| 3. Measured savings over two equivalent periods | **NOT DONE**: needs 1 and 2 and real workloads over time. Nothing is claimed |
| 4. Tuning the recommender with `rightsize backtest` on weeks of real traffic | **NOT DONE**: there are no weeks of real traffic here. Running it on lab minutes would produce numbers that look like evidence and are not |
| 5. A real Kubecost | **PARTLY DONE** (docs/LAB.md result 11): a real Kubecost 2.8.7 found a real defect in the client (the aggregation dropped namespace and kind, so the workload was never found and same-named controllers would merge), fixed with captured fixtures and re-verified against a real OpenCost. The contract holds; the freshness check failed because Kubecost stopped publishing data on the lab install (ingestion did not finish in 75 min), so running the operator on Kubecost data is still unproven |

Verification: full Go suite (25 packages) passes after the client change; GitHub CI runs on the pushed commit.
Verdict unchanged: **not production-ready**, no measured saving.


## P. Addendum 5 (sixth pass): GKE, measured saving, backtest on real cluster data

The owner signed in to GCP and asked for the cloud run and for points 3 and 4. Billing export was left to the owner.

| Item | Result | Evidence |
|---|---|---|
| 1. Managed cluster | **GKE: run for real** (1.36.4, two hours, owner's account, cluster deleted afterwards): read-only checks 9/9, recommend/approve/apply/observe with an SLO on real latency, and rollback. **A real defect found only there** (rollback gave up on write conflicts during a 6-replica rollout and left a degraded change in place), fixed with a test and re-run on GKE: 11/11. EKS and AKS: not run. Short of the validation criteria in CLOUD_VALIDATION section 5 | LAB result 12 |
| 3. Measured saving | **Done at small scale**: after the change the GKE autoscaler went from 3 to 2 nodes; node-hours measured in two equivalent 20-minute windows: **0.0670 USD/hour less at list price (-33 %)**. List price from the Cloud Billing Catalog API, not an invoice; trivial over-provisioned app; no claim about real workloads | LAB result 12 |
| 4. Tuning with backtest | **Backtest run on real GKE data, defaults deliberately not changed**: 100 minutes of constant synthetic traffic show the defaults are safe there (no exceedance) but cannot support tuning; weeks of real, patterned traffic are still required | LAB result 12, RECOMMENDER.md |
| 2. FOCUS billing | Not done (owner's choice, later) | |

Maturity changes: Execution engine 3 to 3-4 and Rollback 3-4 (proven on a managed cluster after a fix); Cost intelligence 2 (one measured, list-priced infrastructure change; still no invoice). Verdict: **still not production-ready**: no week-long run, no invoice, EKS/AKS not run, defaults untuned.

## Eighth pass (2026-11-10): AKS

| Item | Result | Evidence |
|---|---|---|
| AKS (real, free tier, 1.36.5, 2 x Standard_D2as_v4) | **Run twice: 14/15 each.** Read-only checks, recommend/approve/apply under real traffic, SLO observation and forced rollback pass; no defect found. **No node removed** after the change (autoscaler `NoCandidates`: AKS system pods plus Prometheus fill most of one 2-vCPU node), so **no saving measured on AKS**. Resource groups deleted | LAB result 14, evidence/2026-11-10-aks-validation.log |
| Applier and Helm chart on AKS (1 x Standard_D4as_v4) | **Helm lifecycle 4/4; applier 27/27, all 8 scenarios in one run.** Three earlier runs failed only on lab-script defects (lab requests too big for the node, a scenario older than the admission policy, a Git Bash path translation), all fixed and stated | LAB result 15, evidence/2026-11-11-aks-applier-helm.log |
| Kubecost 2.8.7 refresh cadence on AKS (2 runs x 2 h) | **Kubecost stopped serving allocation data ~10 min after its first data**, both runs (HTTP 200, empty, no restarts). The client failed closed every time. No `maxMetricAge` can be derived; cause not determined. As installed, Kubecost is not a usable live cost source | LAB result 16, evidence/2026-11-11-aks-kubecost-cadence.log |
| EKS | Not run (owner deferred AWS) | — |
| 7-day GKE soak with FOCUS export | Running, report due 2026-11-17 | LAB result 13 |

Verdict unchanged: **not production-ready**.

## Ninth pass (2026-11-11/12): measured saving on AKS, cost sources, P1 product completeness

| Item | Result | Evidence |
|---|---|---|
| Node saving on AKS | **Measured**: with `skip-nodes-with-system-pods=false` the autoscaler removed one of two nodes after the change; 0.096 USD/h less at list price (50% of node cost). Not an invoice | LAB result 17, evidence/2026-11-11-aks-node-saving-retry.log |
| Kubecost vs OpenCost on AKS (2 h each) | Kubecost 2.8.7 stalled again (4th reproduction, also with custom pricing); **OpenCost fresh for 2 h, contract PASS**. Recommended source: OpenCost | LAB result 18, evidence/2026-11-11-aks-cost-sources.log |
| Client defect: `window=1h` is the current clock hour | **Found on AKS, fixed** (`60m`), verified live against OpenCost at minute 10 of the hour (10.6 vs 59 min of data) | LAB result 18 |
| Simulated shoppers, compressed week | **4/4**: CPU default held on unseen days, memory default tight (2.9% exceeded); applied change kept through a simulated day, p95 86 -> 75 ms. One invalid run (script edited while running) stated and repeated. Generated load, not real users | LAB result 19, evidence/2026-11-12-synthetic-users.log |
| P1: FOCUS adapters (GCP, Azure), outcome metrics, multi-container records, GitOps (Helm values, Kustomize, push, PR), hash-chained audit + shipping, signed approvals | **Implemented, unit-tested**; on a real cluster 10/10 after **one real defect found and fixed** (the audit chain never verified real `applied` events) | LAB result 20, evidence/2026-11-12-p1-real-cluster.log |
| Operator with signed approvals, `--audit-url` to a real collector, `propose --open-pr` on GitHub | **11/11 on the lab and two real PRs**; **two more defects found and fixed** (shipped audit events were unchained; propose re-indented whole files) | LAB result 21, evidence/2026-11-12-p1-verify.log, evidence/2026-11-12-gitops-open-pr.log |
| Not verified | FOCUS node matching on real Google/Azure rows (the GCP billing export had no data after 2026-10-31), the operator's https `--audit-url`, EKS (deferred) | — |

Verdict: unchanged in scope. The README's "listo para producción" holds only for observe-only use; automatic apply still waits for P0 item 1 (real traffic over real weeks).

## Tenth pass (2026-11-12): audit into WORM storage, real FOCUS rows

| Item | Result | Evidence |
|---|---|---|
| Operator https audit shipping with a private CA, reference WORM collector (S3 Object Lock) | **11/11 on the lab** (MinIO built from source): TLS and token enforced, operator and CLI events stored, the CLI chain verifies from the bucket, a delete with root credentials refused (COMPLIANCE) | LAB result 23, evidence/2026-11-12-audit-worm.log |
| FOCUS node matching on real Google rows | **Done for one day** against the live soak cluster: the 2 nodes matched by `ResourceName` (2.01 of 4.04 USD at list price, the rest correctly unallocated). **A wrong statement in the product found and fixed**: Google's `BilledCost` is after credits (0 on this account), not before; `ListCost` added | LAB result 22, evidence/2026-11-12-gcp-focus-real-nodes.log |
| Not doable here | Weeks of real traffic (needs your workload), the soak report (scheduled for 2026-11-17, now merges its own PR when green), Azure FOCUS export (your Cost Management export), a real cloud WORM bucket, EKS (deferred) | — |

