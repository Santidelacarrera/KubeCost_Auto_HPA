# Engineering report

State of the repository after the audit and hardening pass described in the engineering brief. Everything below says
whether it was **run** (and where) or is an **assumption**. Nothing here is a production measurement.

## 1. Architecture and controller ownership

* Two controllers share one operator: the legacy `KubeCostHPA` (vertical in-place resize driven by Prometheus SLO signals and
  the Python unit-economics engine, optional horizontal scaling) and `KubeCostAutoscaler` (cost-aware horizontal scaling:
  `metrics.k8s.io` demand, KubeCost allocation cost, deterministic engine in `internal/policy`).
* **Single writer of replicas.** `KubeCostAutoscaler` writes only through `deployments/scale` with compare-and-swap; it refuses to
  write while a native HPA, a legacy `KubeCostHPA` with `spec.replicas`, or an older `KubeCostAutoscaler` targets the same
  Deployment (`internal/ownership`, deterministic oldest-wins). The legacy controller applies the mirror rule. The only native
  HPA it tolerates is its own fallback HPA (controller reference).
* Cost estimation (`internal/kubecost`, `internal/money`) is separate from control (`internal/policy`, no I/O, no clock).
  Diagram and derivation: `docs/KUBECOST_AUTOSCALER.md` sections 2-6.
* Spot awareness is an **input**, not placement control: pods per capacity class discount capacity (`spotInterruptionBuffer`)
  and weight marginal cost. The operator does not choose nodes. Only the legacy controller cordons/drains Spot nodes on
  interruption signals.

## 2. Findings, ranked

| # | Severity | Finding | Status |
|---|---|---|---|
| 1 | High (oscillation) | Cost pressure used today's spend, which the controller itself changes: under constant demand the replica count cycled (5 reversals in 2 h in simulation). | **Fixed**: pressure is judged on the cost of the comfortable size. Regression tests fail on the old code. |
| 2 | High (operability) | No live emergency stop except per-resource `dryRun` edits or a redeploy. | **Fixed**: `autoscaling.kubecost.io/pause` annotation, tested, lifted without restart. |
| 3 | Medium (cost/efficiency) | While replicas changed inside the 15 min cost window the cost signal is "unstable" and the engine freezes non-safety moves: scale-down is delayed after every action (gradual decline cost more than the HPA in simulation) and a ramp rides the utilisation ceiling (about 880 s in simulation, no saturation). | **Open, by design, quantified** in `docs/BENCHMARKS.md`. Trade-off decision below. |
| 4 | High (unverified) | The KubeCost response format is verified against OpenCost's source and a stub, not against a live KubeCost. | **Open**: needs a cluster with KubeCost; start in `dryRun`. |
| 5 | Medium (security) | RBAC is a ClusterRole including `nodes: patch` (spot cordon), `kubecosthpas: create/delete`, and HPA write cluster-wide. | **Open**, documented below. |
| 6 | Medium (coverage of the brief) | `KubeCostAutoscaler` demand is CPU/memory utilisation only; no request-rate, queue-depth, latency or error-rate signal. The legacy controller uses Prometheus SLO signals. | **Open** limitation. |
| 7 | Low | Tagged-release path (versioned images, OCI chart) never executed; `main` publishes images on every push (runs succeeded). | **Open**, steps in `docs/RELEASING.md`. |
| 8 | Info | Coverage of the single-writer logic was 62%: now 93%, with mutation checks. | Done (PR #30). |

## 3. Changes in this pass (uncommitted until authorised)

| File | Change |
|---|---|
| `operator/internal/policy/policy.go` | cost pressure from the projected cost of the comfortable size (finding 1) |
| `operator/internal/policy/policy_test.go` | regression tests for the new semantics, attribution assertion updated |
| `operator/internal/sim/` (new) | deterministic simulator + tests (determinism, bounds, no reversals at constant demand, HPA arithmetic) |
| `operator/cmd/simbench/` (new) | prints the benchmark tables |
| `operator/api/v1alpha1/kubecostautoscaler_types.go`, `internal/controller/{settings,kubecostautoscaler_controller}.go` + tests | pause annotation (finding 2) |
| `docs/BENCHMARKS.md`, `docs/KUBECOST_AUTOSCALER.md`, `docs/ENGINEERING_REPORT.md`, `README.md`, `CHANGELOG.md` | documentation |

Earlier passes (already merged): the `KubeCostAutoscaler` controller and CRD, ownership model, kind end-to-end
(`hack/e2e-kca.sh`), Helm values schema and dashboard, release pipeline, SECURITY/CONTRIBUTING/RELEASING docs.

## 4. Tests actually run in this pass (local sandbox)

```text
gofmt -l .                                              no output
go vet ./...                                            ok
go test -race -p 2 ./...                                all packages ok (policy, sim, controller, ownership, kubecost, ...)
KUBEBUILDER_ASSETS=/tmp/envtest/bin go test -tags envtest -p 2 ./internal/controller/...   ok (58 s, real kube-apiserver + etcd)
go run ./cmd/simbench                                   output reproduced in docs/BENCHMARKS.md
```

Mutation checks (code deliberately broken, then restored): reverting the pressure fix fails the new policy test and
`TestConstantDemandNeverReversesDirection`; disabling the pause line fails both pause tests; breaking the ownership
conflict checks fails `TestForAutoscalerSeesEveryOtherReplicaWriter`. The Python engine tests were not re-run (unchanged).
CI (GitHub Actions) ran the full matrix, including `e2e-kind`, `e2e-kca`, `govulncheck` and `helm lint`, on the merged
work up to PR #30; it has **not** run on this pass.

## 5. Scaling behaviour before and after

Synthetic only (`docs/BENCHMARKS.md`). The one behavioural change in this pass: at constant 1500 rps with the cost policy the
controller used to cycle between 22 and 19 replicas (5 direction reversals in 2 h); it now steps down once to 19 and
stays (0 reversals, about 12% fewer replica-hours than an HPA at 22).

## 6. Cost versus reliability

Measured in simulation only: no latency, error-rate or throughput data exist. In the simulated patterns the cost policy
never saturated more than the HPA except for 60 s extra in `burst`, saved about 12% (`steady`) and about 20%
(`intermittent`), and cost more than the HPA in `gradual-decline` (finding 3). Savings figures are not claims about production.

## 7. Security and RBAC

* ClusterRole `kubecost-auto-hpa-manager` (`operator/config/rbac/role.yaml`, generated from markers). Needed: read pods, deployments,
  PDBs, nodes; `deployments/scale` get/patch/update; `metrics.k8s.io` pods; HPA create/delete/update (fallback only); events; own
  CRs and status; `pods/resize` and `pods/eviction` (legacy controller only).
* Over-broad for the new controller alone: `nodes: patch` (legacy Spot cordon), `kubecosthpas: create/delete`
  (the controller does not create or delete its own CRs). Hardening option: ship a minimal role for installs that only use
  `KubeCostAutoscaler` and keep the legacy permissions behind a Helm switch. `Freeze` mode avoids HPA write permission use.
* No cloud credentials in manifests or images; KubeCost/Prometheus/engine bearer tokens come from Secrets, TLS/mTLS options
  exist, no redirects are followed, raw response bodies are never logged or stored. `govulncheck` runs in CI. Images are not
  signed and have no SBOM/provenance yet (roadmap).
* The pause annotation can be set by anyone who can update the CR; it only removes the operator's ability to act.

## 8. Known failure modes and remaining risks

Behaviour per scenario with test names: `docs/KUBECOST_AUTOSCALER.md` section 9 (KubeCost down, 5xx/429, malformed, stale,
API outage, missing metrics API, restart, target deleted, over budget). Additional: coverage of the ten scenarios in the brief:
stable demand (`TestNoOscillationUnderConstantDemand`, sim `steady`), spike (`TestSafetyScaleUpIsCappedByBudgetOnlyAboveTheCeiling`, sim
`burst`), gradual decline (sim `gradual-decline`, `TestScaleDownOnlyAfterStabilisationWindowAndSurvivesRestart`), missing cost
(`TestKubeCostOutageFreezesThenRecovers`), stale utilisation (`TestMissingDemandHolds`, `resmetrics` freshness tests), Spot
interruption/capacity (`TestSpotCapacityIsNotGuaranteedCapacity`, legacy drainer tests), node capacity shortage (pending-pod gate,
`TestSafetyGates`), restart (`TestRestartDuringCooldownDoesNotRescale`, `TestRestartKeepsStabilisationState`), competing
controllers (ownership tests, `TestRealAPIServer_NativeHPAConflictBlocksAllWrites`), availability floor
(`TestCostPressureScaleDownStopsAtSafetyReplicas`, bounds and `minHealthyReplicas`).
Remaining risks: findings 3-6; a Spot interruption is seen only as pods becoming unready/pending, not predicted; PDBs are
respected by the legacy drainer (Eviction API) but the horizontal controller never evicts.

## 9. Reproduce

```bash
cd engine && pip install -e '.[dev]' && pytest
cd operator
make check                                  # gofmt, vet, unit, -race, integration
KUBEBUILDER_ASSETS=<dir> make envtest       # real apiserver + etcd
go run ./cmd/simbench                       # benchmark tables
cd .. && hack/e2e-kind.sh && hack/e2e-kca.sh   # kind end-to-end (docker, kind, kubectl)
```

## 10. Roadmap to production hardening

1. Run against a real KubeCost in a non-critical namespace in `dryRun`, compare `status.cost` with the KubeCost UI for a full
   business cycle; fix any schema or unit mismatch.
2. Decide finding 3: let non-safety, *demand-driven* scale-ups and scale-downs proceed while the cost signal is unstable
   (cost then only restrains, never gates), accepting that the budget guardrail cannot cap headroom for up to one cost window.
   The simulator can score the change before it ships.
3. Least-privilege RBAC profile for `KubeCostAutoscaler`-only installs (finding 5).
4. Request-rate / latency / error-rate demand signals for `KubeCostAutoscaler` (custom/external metrics API) so cost can
   never trade against an SLO that CPU does not see (finding 6).
5. Image signing (cosign), SBOM and provenance in `release.yml`; Trivy image scan in CI.
6. First tagged release (`docs/RELEASING.md`), then a published, versioned install and rollback rehearsal.
7. Load and soak tests on a cluster, and a measured (not simulated) comparison against an HPA with real latency percentiles.
