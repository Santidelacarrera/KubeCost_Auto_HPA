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
