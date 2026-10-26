# Changelog

Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Versions follow semver once `v0.1.0` is tagged.

## [Unreleased]

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
