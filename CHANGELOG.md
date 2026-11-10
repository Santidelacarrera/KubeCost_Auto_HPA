# Changelog

Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Versions follow semver once `v0.1.0` is tagged.

## [Unreleased]

### Added (fourth pass)
- Applier: separation of duties enforced by three ValidatingAdmissionPolicies (no webhook), per-namespace policies (`policy.<ns>.yaml`), metrics and alerts, `rightsize publish`. Executor: reads of the workload or the SLO query may fail for up to 2 minutes during observation without a rollback, and unobserved time never counts as healthy.
### Fixed (fourth pass)
- Executor `Watch`: time during which the cluster could not be read no longer counts against the rollout timeout (a control-plane outage stalls the rollout too); before, a 40 s API-server outage could roll back a healthy change with "rollout did not complete". Regression test `TestWatchDoesNotCountBlindTimeAgainstTheRolloutTimeout`.
### Added (fourth pass, lab)
- `hack/lab-hardening.sh`: chart upgrade from the pre-work chart and rollback, admission checks with real users, 60 objects at once, kube-apiserver stopped for ~40 s.
### Added (third pass: the rest that can be done without cloud credentials)
- Opt-in **recommendation applier**: `WorkloadRecommendation` CRD, `rightsize plan --publish`, `rightsize approve --cr` (approval through the status subresource, own RBAC verb), operator controller behind `--enable-recommendation-applier` / chart `operator.recommendationApplier.enabled=false`, policy ConfigMap re-read on every reconcile, same-namespace rule, observe-only honoured, resume after operator restart. ADR-0003, docs/SAFETY.md. Roles in `deploy/rbac/rightsize.yaml`.
- `rightsize backtest` (replays percentile/headroom settings on history; train on the past, test on the unseen recent part), FOCUS billing adapter (`cost --billing-format focus`, provider-id mapping), `hack/cloud-validate.sh` (read-only validation of any cluster) and docs/CLOUD_VALIDATION.md.
- Lab evidence on real systems: Argo CD and Flux (15 checks), a two-node cluster compared with a real drain (9), the applier on a real cluster, real HTTP traffic and latency before/after (docs/LAB.md results 6-9).
### Fixed (third pass, found on real clusters)
- Applier: policy ConfigMap read with a direct GET (the cached client needed list/watch that the Role rightly denies); a final phase can no longer be overwritten by a stale cached read (RolledBack had been overwritten by Denied).
- Recommender: variability below the configured floors (1 MB memory, 5-15 millicores) no longer lowers confidence; the "window shorter than 7 days" note shows minutes for short windows.
- FOCUS parsing accepts negative credit rows (they are skipped and reported, not fatal).

### Added (second pass)
- `execute` supports StatefulSet and DaemonSet (OnDelete update strategies are refused; the DaemonSet replica-count check is off because the desired count follows the node count).
- `rightsize approve`: stamps the approval with the identity the API server reports (SelfSubjectReview); policy `approvers` restricts who may approve.
- `rightsize plan` (recommendation plus HPA / VPA / exclusion-label facts read from the cluster), `rightsize propose` (GitOps: commit on a new local branch, no push), `rightsize report` (acceptance, rollback rate, kept ESTIMATED savings from the audit), `--as` impersonation.
- Recommender: windows under 7 days cap confidence and add a seasonality risk; a four-quarter stationarity check lowers confidence.
- `cost`: namespace budgets and cost-per-hour deviation against an earlier report.
- `deploy/rbac/rightsize.yaml` (read and apply roles for the person running the CLI), proven by impersonation.
- Lab: `hack/lab-e2e2.sh` (23 checks: real HPA, real VPA object, StatefulSet, identity, GitOps proposal, report, nodes/cost, live contract test against a real OpenCost), `lab-rbac.sh` (14), `lab-crash.sh` (7), `lab-opencost.sh`. docs/COMPETITIVE.md with fetched sources.

### Added (policy, execution, rollback, cost, capacity, lab)
- `internal/safety` (fail-closed policy), `internal/execute` (apply with previous state on the object, observation, automatic rollback, audit JSONL), `cmd/rightsize` (`apply`, `rollback`, `nodes`, `cost`). ADR-0002, docs/SAFETY.md.
- `internal/cost` (estimated / billed / allocated kept apart, reconciliation, CSV billing adapter, observed-savings comparison), `internal/capacity` (would a node actually be released). docs/COST.md. Not validated against a real invoice.
- Reproducible lab: `hack/lab-up.sh`, `hack/lab-e2e.sh` (22 checks on a real Kubernetes 1.33.4), `hack/lab-helm.sh` (16 checks), `hack/lab-down.sh`; logs in `docs/reports/evidence/`. KubeCost is a mock there.
- Docs: ARCHITECTURE, COMPATIBILITY, GITOPS, LAB, ROADMAP, SAFETY, TROUBLESHOOTING.
### Fixed (found on the real cluster)
- Rollback failed with a resourceVersion conflict when the Deployment controller updated status at the same moment, leaving the change in place. Rollback now retries on conflict; apply retries only while the spec generation is unchanged.

### Added (read-only recommender, phase 1)
- `internal/recommend` (pure percentile/headroom algorithm with abstention and veto rules), `internal/recommend/promsrc` (Prometheus reader) and `cmd/recommend` (prints JSON; no Kubernetes client). See `docs/RECOMMENDER.md` and ADR-0001. Tested with a mock Prometheus only; defaults are untuned.

### Added (observe-only)
- Operator-wide `--observe-only` flag (Helm `operator.observeOnly`, **default `true` in the chart**; the binary default stays `false` for kustomize users). Every `KubeCostAutoscaler` and `KubeCostHPA` then behaves as if `spec.dryRun` were set whatever the spec says, the Spot drain controller is not started, and the autoscaler reports `ScalingActive=False, reason ObserveOnly`. It can only add safety. Tests: `TestObserveOnlyOperatorNeverWritesWhateverTheSpecSays`, `TestObserveOnlyOperatorDoesNotTouchPodsEvenWithoutSpecDryRun`.
- **Behaviour change for chart users:** a fresh install no longer writes anything until `--set operator.observeOnly=false`.
- Fixed `TestVersionIsInjectedAtLinkTime` on Windows (the test binary needs the `.exe` suffix).

### Fixed
- `hack/helm-rehearsal.sh`: `grep -q` at the end of a pipe under `pipefail` could report a false failure (exit 141); output is now captured first. The script also checks for docker, kind, kubectl and helm before starting.
- `hack/helm-lifecycle.sh` and `hack/release-dry-run.sh`: the same `pipefail` plus `grep -q` false-failure pattern was removed; `release-dry-run.sh` passes locally on v0.1.0 and `helm-lifecycle.sh` passes against the local apiserver.
- Live KubeCost test: freshness now uses the CRD default `maxMetricAge` (5m, configurable with `KUBECOST_LIVE_MAX_AGE`) instead of a fixed 30 min.
- Stabilisation timestamps written in the future (clock skew, a restored or hand-edited status) froze the cooldown and the stability window until that time passed (observed: 20 min freeze with a 2 h future timestamp). They are now replaced with the current time once and persisted, so the window is bounded. Regression test: `TestFutureStabilisationTimestampCannotFreezeScaling`.
- Race condition in the real-apiserver manager test (the log buffer was read while the manager still wrote to it). Test-only; found by `make test-all`.

### Added (validation round)
- Real-apiserver test for the emergency pause: no writes while paused (including against an external replica change), no revert on resume. Negative control verified.
- `hack/helm-lifecycle.sh`: Helm install, upgrade, rollback and uninstall against any disposable cluster (no pods). Ran green on kube-apiserver 1.33.4.
- `docs/PILOT.md` §11: staging procedure for the live KubeCost test; `docs/RBAC.md`: namespace-scoped design evaluation (not implemented).

### Changed
- KubeCost client: honours `Retry-After` on HTTP 429/503 (delta-seconds and HTTP-date forms, capped at 30 s) and allows at most 4 concurrent requests across all workloads.
- `make envtest` fails immediately when `KUBEBUILDER_ASSETS` is unset instead of skipping every real-apiserver test. New `make test-all` runs the complete tagged suite under the race detector.

### Added
- `autoscaling.kubecost.io/pause` annotation: emergency stop for a `KubeCostAutoscaler` (decisions still reported, nothing written, lifted without restart).
- `internal/sim` + `cmd/simbench`: deterministic synthetic comparison of the native HPA with the decision engine (`docs/BENCHMARKS.md`), and `docs/ENGINEERING_REPORT.md`.
- Helm: `values.schema.json` (typos and wrong types fail at install time) and an optional Grafana dashboard for
  `KubeCostAutoscaler` (`grafanaDashboard.enabled`); CI renders the chart with every optional feature on.
- Tests for the replica-ownership conflicts (native HPA, legacy `KubeCostHPA`, reader errors never read as "no conflict").
- Apache-2.0 `LICENSE`, `SECURITY.md`, `CONTRIBUTING.md`, `docs/RELEASING.md`.
- `KubeCostAutoscaler` (namespaced CRD): cost-aware horizontal scaling with single-writer replica ownership,
  KubeCost allocation client, deterministic policy engine, persisted stabilization, spot awareness,
  `Freeze` / `ResourceMetricsFallback` failure policies, Prometheus metrics `kubecost_autoscaler_*`.
- `KubeCostHPA` (in-place vertical resize, optional memory/replica scaling, spot cordon/drain) with the unit economics
  engine (Python) and Redis state.
- Helm chart, Kustomize bases, Docker Compose stack, Prometheus alerts and NetworkPolicy.
- CI: unit/integration/race, envtest against a real kube-apiserver, `govulncheck`, generated-files check, kind
  end-to-end for `KubeCostHPA` (`e2e-kind`) and `KubeCostAutoscaler` (`e2e-kca`, real metrics-server, stubbed KubeCost).
- Release pipeline: multi-arch images to GHCR on `main`; versioned images and OCI Helm chart on `v*` tags.

- Validation tooling: pinned RBAC allow-list tests (`internal/rbaccheck`) and real-apiserver tests that run the reconcilers and the manager as a
  user holding only the generated ClusterRole; KubeCost contract fixtures and negative tests, plus a `kubecost_live`-tagged test for a real installation.
- Observability: reconcile and KubeCost-query latency histograms, decision-reason counter, demand-metric age, recovery time, pause gauge/counter,
  build info; alerts for pause, stale demand data, slow KubeCost and slow reconciles; `docs/OBSERVABILITY.md` with proposed SLOs.
- Release preparation (nothing published): version/commit injected into the binary, `--version`, SBOM/provenance/keyless signing steps on tags,
  `hack/release-dry-run.sh`, `hack/helm-rehearsal.sh`, manual `supply-chain` workflow, `docs/SUPPLY_CHAIN.md`, `docs/RBAC.md`,
  `docs/BENCHMARK_PROCEDURE.md`, `docs/DEMAND_SIGNALS.md`.
- Simulator rewritten to run every scaler on identical traces and disturbances (cost shock, metric lag, KubeCost outage, restart, pause, external writes).

### Changed
- RBAC reduced to the verbs the code uses (for example no create/delete/patch on `kubecosthpas`, no `update` on `pods/resize`, no `patch` on
  `deployments/scale`); the Helm role no longer drifts from the generated one and drops `nodes: patch` and `pods/eviction` when Spot draining is off.
- The cost signal is also unstable for a window after replica changes made by other actors (`status.lastReplicaChangeTime`).
- The sample `KubeCostAutoscaler` uses a conservative 75/80 cost-pressure/ceiling pair.

### Fixed
- The Helm chart job in `release.yml` pushed to an OCI repository with a mixed-case owner, which helm rejects (`invalid_reference`); it would have failed the first tagged release.
- Cost pressure was judged on today's spend, which depends on the replica count the controller chooses, so constant demand with a cost policy made the replica count oscillate; it is now judged on the cost of the comfortable size.

### Known limitations
- Not validated against a live KubeCost installation (response format checked against OpenCost's source).
- The tagged-release path (versioned images, OCI chart) has not been exercised yet.
