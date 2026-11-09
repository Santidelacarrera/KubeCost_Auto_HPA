# KubeCost Auto-HPA: engineering audit and checkpoint, 2026-11-09

Status words: **PASS**, **FAIL**, **BLOCKED** (needs infrastructure/authorisation not available), **NOT RUN**.
Nothing was committed, pushed, tagged or deployed. Work is uncommitted in the worktree `claude/kubecost-auto-hpa-prompt-91afa7`.

## 1. Executive summary

* The repository is an **autoscaling operator** (replica scaling by demand plus a KubeCost cost policy, legacy in-place resize, Spot drain).
  It is **not** a percentile-based rightsizing/recommendation product, and it has no policy/approval/rollback engine. The master prompt describes
  a much larger product; most of it is MISSING (matrix, section 3). This pass did **not** attempt to build it in one go: doing so without a real
  cluster would produce unverifiable code, which the prompt itself forbids.
* Done and verified here: the first P0 control: a global **observe-only** mode (section 4), plus a Windows test fix.
* No real Kubernetes was available (Docker daemon not running, no kind/k3d; the only kubeconfig context is a stopped `minikube`, which was not touched).
  All "real cluster" criteria remain BLOCKED.

## 2. Obsidian (second brain)

* MCP `obsidian`: **worked** (search and read). Note read: `1_Projects/KubeCost_Auto_HPA.md` (found by search; other notes not read in this pass).
* **Contradiction found:** the note says `status: production-ready`, "100% ScaleOps/Cast AI parity", with `web/`, `frontend/`, `saas.py`,
  Stripe billing, GPU/MIG, 25 Go packages and 81 Python tests. **This checkout has none of that**: no `web/`/`frontend/`, engine has 5 modules and
  27 tests, 15 Go test packages. Either the note describes a different branch/history or it overstates; the repo and the commands below are the
  source of truth. The note should not be relied on for status claims. The commits it cites (`c37812d`, `90addac`...) were not checked in this pass.
* Updated: appended a dated section with this audit's decisions (see Obsidian note).

## 3. Audit matrix (from code and test runs, not from claims)

| Component | State | Evidence | Gap / action |
|---|---|---|---|
| Kubernetes integration (controllers, CRDs) | IMPLEMENTED BUT NOT VERIFIED on a real cluster | controller-runtime reconcilers; fake-client tests PASS; prior report: envtest on kube-apiserver 1.33.4 | envtest NOT RUN here (no assets); no kind/k3d lab runs here |
| Controller (replica scaling, single writer, failure policy) | IMPLEMENTED, unit-tested | `internal/controller`, `internal/ownership`, `internal/policy` PASS | no live-cluster evidence in this pass |
| Prometheus | PARTIALLY IMPLEMENTED | client + cache for legacy `KubeCostHPA`; new autoscaler uses `metrics.k8s.io` only | no percentiles, throttling, OOM, restarts, latency signals; never run against real Prometheus here |
| KubeCost | IMPLEMENTED BUT NOT VERIFIED | contract tests on fixtures PASS; live test written | BLOCKED: needs a real KubeCost (`-tags kubecost_live`) |
| Recommendation engine (p50..p99, headroom, OOM/throttle gating, explainable output) | **MISSING** | no code | P0.3 not started |
| Cost engine (estimated/billed/allocated, reconciliation, billing adapters) | PARTIALLY IMPLEMENTED | `internal/money`, KubeCost allocation sample, Python `economics.py` | no billing adapters, no billed/allocated reconciliation, no idle/shared cost |
| Autoscaling (HPA-like) | IMPLEMENTED, simulator-benchmarked only | `internal/sim`, `docs/BENCHMARKS.md` (synthetic) | no latency/error data |
| Policy & safety engine | PARTIALLY IMPLEMENTED | per-resource `dryRun`, `pause` annotation, **new** global observe-only | allow/deny lists, maintenance windows, approvals, canary, rate limits, audit log: MISSING |
| Execution engine / rollback on SLO degradation | **MISSING** (only "delete CR returns pods to template") | no code | P0.2/16.6 not started |
| GitOps (Argo CD/Flux) detection | MISSING | no code | |
| HPA/VPA conflict handling | PARTIALLY IMPLEMENTED | `internal/ownership` (single writer, fallback HPA) tested | no VPA detection |
| Security | PARTIALLY IMPLEMENTED | reduced RBAC verified by tests (prior pass), mTLS/bearer options | image scan/signing unverified (`docs/SUPPLY_CHAIN.md`) |
| Observability | IMPLEMENTED BUT NOT VERIFIED live | metrics, alerts (PromQL syntax-checked), dashboard | no live scrape |
| Testing | see section 5 | | no E2E scenario of the master prompt exists |

## 4. Implemented in this pass

**Global observe-only mode** (P0.2/P0.4, first slice).
* Problem: writes were gated only by per-resource `spec.dryRun` (default false); nothing let an operator install start safe.
* Change: `--observe-only` flag; Helm `operator.observeOnly` (**default `true` in the chart**, `false` in the binary so kustomize users are unchanged).
  When on: both reconcilers behave as dry-run regardless of spec (decisions, events, metrics still produced), Spot drain controller not started,
  autoscaler condition `ScalingActive=False/ObserveOnly`, decision message prefix `observe-only`. It can only add safety.
* Files: `operator/cmd/main.go`, `internal/controller/{kubecostautoscaler_controller,kubecosthpa_controller,settings}.go`, chart `values.yaml`,
  `values.schema.json`, `templates/operator.yaml`, `CHANGELOG.md`, `docs/OPERATIONS.md`.
* Tests added: `TestObserveOnlyOperatorNeverWritesWhateverTheSpecSays` (also checks it acts again once lifted),
  `TestObserveOnlyOperatorDoesNotTouchPodsEvenWithoutSpecDryRun`. Both PASS. They use a fake client: **not** a real-cluster verification.
* Also fixed: `TestVersionIsInjectedAtLinkTime` failed on Windows (missing `.exe`); now passes.
* Behaviour change to note: a fresh chart install no longer writes anything until `operator.observeOnly=false`.
  I did not check `hack/helm-lifecycle.sh`/`helm-rehearsal.sh` against that (they install the chart without pods, so they should be unaffected; NOT RUN).

## 5. Tests executed (this Windows machine)

| Command | Result |
|---|---|
| `go build ./...`, `go vet ./...` (operator) | PASS |
| `gofmt -l .` | PASS (no output) |
| `go test -count=1 -p 2 ./...` (15 test packages) | PASS |
| `go test -race ...` | BLOCKED: needs cgo/gcc, not installed (earlier report ran it elsewhere) |
| `pytest` (engine) | PASS, 27 passed |
| `helm lint deploy/helm/kubecost-auto-hpa`; `helm template` with default and `observeOnly=false` | PASS; flag rendered `true`/`false` |
| `bash hack/release-dry-run.sh v0.1.0` | PASS (nothing built/signed/published) |
| envtest against real kube-apiserver (`-tags envtest`) | NOT RUN: no `KUBEBUILDER_ASSETS` |
| kind/k3d lab, E2E scenario, rollback, negative E2E, Prometheus integration, billing | NOT RUN / BLOCKED: no Docker daemon, no kind/k3d |
| `rbaccheck` tests | PASS (before and after; chart role unchanged) |

## 6. Pending and blockers

* **External blockers:** running Docker/kind (or authorisation to use the local `minikube`), real KubeCost/OpenCost, real Prometheus, cloud billing, GitHub CI on these changes.
* **P0 (not started):** safe recommendation engine (percentiles, OOM/throttle/latency vetoes, insufficient-data abstention);
  policy engine (allow/deny lists, windows, approvals, canary, rate limit, audit log); execution with previous-state record and verified automatic rollback;
  reproducible kind lab and the E2E scenario of the master prompt.
* **P1:** billing adapters and estimated/billed/allocated reconciliation; outcome metrics (observed savings); GitOps detection; HPA/VPA conflict detection; node/bin-packing analysis.
* **P2:** multi-tenancy, budgets per namespace, PR-based GitOps flow.
* Recommended next step: decide whether the product is "autoscaler" (current) or "rightsizing recommender" (master prompt), then build the
  recommender as a separate read-only package against a kind cluster, before any execution engine. A plan of that size should be an ADR first.

## 7. Maturity (0-5, from verified behaviour only)

Kubernetes integration 2 · Metrics collection 1 · Recommendation engine 0 · Cost intelligence 1 · HPA/VPA compatibility 2 · Rightsizing 0 · Node optimisation 0 ·
Policy & safety 2 (dry-run, pause, observe-only; nothing else) · Execution engine 1 · Rollback 0 · GitOps 0 · Security 2 · Reliability 2 · Observability 2 ·
Testing 2 (strong unit/simulator, no live cluster here) · Documentation 3.

## 8. Verdict

* Works for real? Unit/simulator level yes; against a real cluster: not shown in this pass.
* Connected to real Kubernetes / real metrics / real costs: **no evidence in this pass**.
* Safe to apply changes: partially (single-writer, pause, dry-run, observe-only); no policy engine.
* Rollback: **not implemented**.
* Commercial demo / production: **no**.
* Next engineering priority: kind lab + read-only recommendation engine with abstention rules.

## 9. Addendum: recommender phase 1 (same day)

Implemented (ADR-0001 phase 1): `operator/internal/recommend` (algorithm), `operator/internal/recommend/promsrc` (Prometheus reader),
`operator/cmd/recommend` (CLI), `docs/RECOMMENDER.md`. No Kubernetes client is imported by any of them (checked by grep), so they cannot write to a cluster.

| Check | Result |
|---|---|
| `go vet ./...`, `gofmt -l .` | PASS |
| `go test -count=1 -p 2 ./...` (now 18 test packages, incl. 3 new) | PASS |
| Negative control: OOM veto disabled | `TestVetoesBlockOnlyTheRightDecreases` FAILS as expected; restored, PASS |
| Failure injection (mock Prometheus: HTTP 500, down, non-JSON, kube-state-metrics missing, NaN throttle, PromQL-injection names, too many points) | PASS; each yields an error or an abstention, never a recommendation from partial data |
| Real Prometheus / cluster / E2E scenario | NOT RUN: BLOCKED (no Docker daemon, no kind/k3d) |

Status of the recommender: **IMPLEMENTED BUT NOT VERIFIED**. Defaults are untuned; no seasonality model; HPA/VPA/exclusion flags are manual inputs.
Maturity change: Recommendation engine 0 -> 1, Metrics collection stays 1 (no real scrape), Rightsizing 0 -> 1.

Still MISSING from the master prompt (each needs its own ADR and, for most, a real cluster): CRD surface (phase 2); policy engine, approvals, canary, rate limits,
audit log; execution with recorded previous state and verified rollback; GitOps detection; billing adapters and estimated/billed/allocated reconciliation;
node/bin-packing analysis; kind lab and the full E2E scenario. The repository is therefore far from "100% of the prompt"; this pass does not claim it.
