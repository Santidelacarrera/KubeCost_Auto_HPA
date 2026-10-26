# Engineering report: validation, security and release preparation

Status words: **VERIFIED** (run here, result checked), **UNVERIFIED** (implemented, but not exercised against the real
thing), **PENDING** (not done), **BLOCKED** (needs infrastructure or authorisation that does not exist here). Nothing was
pushed, merged, tagged, published or deployed in this pass; the default branch is unchanged.

## 1. Executive summary

* Verified: reduced RBAC with allow-list tests and with the real reconcilers/manager run against a real kube-apiserver **as a user holding only that
  role**; KubeCost client contract and negative tests against fixtures; a corrected simulator comparing the HPA and the engine on identical traces; new
  operational metrics; a Go SBOM; a signing mechanism check; an offline release rehearsal; a real latent release bug found and fixed.
* Not verified against the real thing: **KubeCost itself** (live test written, never run), **any latency/error behaviour**, the whole tagged-release path
  (image SBOM/provenance, keyless signing, chart push), the kind Helm rehearsal, image vulnerability scanning.
* Biggest finding: the shipped cost policy saves money by running with less headroom (benchmarks, section 4); a conservative setting removes the headroom penalty
  and, in simulation, the savings along with it.

## 2. Changes and files

| Area | Change | Files |
|---|---|---|
| RBAC | unused verbs removed; Helm role synced and conditioned on Spot draining; allow-list tests; restricted-user envtest | `operator/internal/controller/*_controller.go` markers, `config/rbac/role.yaml`, `deploy/helm/.../templates/rbac.yaml`, `operator/internal/rbaccheck/`, `internal/controller/envtest_test.go`, `manager_envtest_test.go`, `kca_envtest_test.go`, `docs/RBAC.md` |
| KubeCost | contract fixtures, negative/bounds tests, live test (tagged) | `operator/internal/kubecost/testdata/`, `contract_test.go`, `live_test.go` |
| Engine | experimental unstable-cost modes (off, simulator only); external replica changes mark the cost unstable | `internal/policy/policy.go`, `api/v1alpha1/kubecostautoscaler_types.go` (+ regenerated CRD), `kubecostautoscaler_controller.go` |
| Simulator | identical traces, real evaluation periods, disturbances, acceptance criteria, generated doc block | `operator/internal/sim/`, `cmd/simbench/`, `docs/BENCHMARKS.md` |
| Observability | 7 new metrics, 4 alerts, SLO proposals, benchmark procedure | `autoscaler_telemetry.go`, `kubecostautoscaler_controller.go`, `resmetrics.go`, `extras.yaml`, `docs/OBSERVABILITY.md`, `docs/BENCHMARK_PROCEDURE.md` |
| Demand signals | analysis only | `docs/DEMAND_SIGNALS.md` |
| Supply chain / release | version injection, `--version`, build_info, SBOM/provenance/signing steps, hardened permissions, chart-push fix, rehearsal scripts, manual workflow | `operator/Dockerfile`, `operator/cmd/version.go`, `.github/workflows/{ci,release,supply-chain}.yml`, `hack/release-dry-run.sh`, `hack/helm-rehearsal.sh`, `docs/SUPPLY_CHAIN.md`, `docs/RELEASING.md` |

## 3. Tests run (this sandbox, final tree)

```text
gofmt -l .                                      no output
go vet ./... ; go vet -tags "envtest integration" ./... ; go vet -tags kubecost_live ./internal/kubecost    clean
go test -race -count=1 -p 2 ./...               all 15 packages ok
go test -tags integration -count=1 ./...        ok (real Python engine)
KUBEBUILDER_ASSETS=... go test -tags "envtest integration" -count=1 ./internal/controller/...   ok, 83 s (real kube-apiserver 1.33.4 + etcd)
REQUIRE_HELM=1 go test ./internal/rbaccheck      4 tests ok (generated role, Helm role, Helm without Spot draining, leader election)
pytest (engine)                                  27 passed
helm lint / helm template (defaults, all optional features, schema negative)    ok
kustomize build (default, local, e2e, e2e-kca)   ok
actionlint                                       clean
PromQL parse of the 13 alert expressions (real Prometheus parser)   13 ok (syntax only)
hack/release-dry-run.sh v0.1.0                   passes
```

Negative controls (code or role deliberately broken, then restored): removing `pods/resize` fails the manager test with `forbidden`; removing
`deployments/scale` fails the scale test; reverting the pressure fix, the pause line, the recovery metric, the external-change detection and the
ownership checks each fail their tests. Baseline before changes: the same suites passed on the PR #31 head; GitHub CI on that head is green (12 checks).
**GitHub CI has not run on this pass's changes.**

## 4. Benchmarks and assumptions

Synthetic only, `docs/BENCHMARKS.md` (deterministic, generated block, checked by a test). 14 scenarios, 13 scalers, one fixed assumption set. The criteria
for acceptability were fixed before the results. Headline facts: without a cost policy the engine is not better than a matched HPA; the shipped cost policy
is -3.4% cost against it in total, cheaper in 8 scenarios and dearer in 6, with about 2240 s more above 90% utilisation (ramp, outage); the 75/80 setting
removes that headroom penalty (+90 s) at +2.6% cost; a 5 minute scale-down window and the "demand-only while unstable" mode fail the criteria. No latency,
error or throughput numbers exist.

## 5. Needs real KubeCost or external infrastructure

| Item | State |
|---|---|
| Contract against a live KubeCost/OpenCost | BLOCKED: `go test -tags kubecost_live -run Live ./internal/kubecost` with `KUBECOST_LIVE_*`; not run |
| Latency, error rate, throughput, real cost comparison | BLOCKED: `docs/BENCHMARK_PROCEDURE.md`; not run |
| Kind install/upgrade/rollback of the chart | UNVERIFIED: `hack/helm-rehearsal.sh` written, no Docker daemon here |
| Image build, image scan, image SBOM | UNVERIFIED/BLOCKED: no Docker daemon, no scanner database |
| Keyless signing, provenance, chart push on a tag | UNVERIFIED: only runs on a tag, which was not created |
| Spot draining and leader election with the restricted role | UNVERIFIED: not exercised against a real apiserver |
| Request-rate / latency demand signals | PENDING: analysis only |

## 6. Critical and high risks

1. **High:** the KubeCost integration has never touched a real KubeCost.
2. **High:** no measured latency/error behaviour anywhere; the simulator cannot show tail latency.
3. **High (for latency-sensitive workloads):** with the default 90% ceiling the cost policy tracks the ceiling during continuous change; recommended mitigation is configuration (docs/BENCHMARKS.md), the CRD defaults are unchanged.
4. **High until rehearsed:** the tagged-release path has never run; one bug (mixed-case OCI owner) was found by inspection and fixed, more may exist.
5. **Medium:** RBAC is cluster-wide and the kustomize base still grants `nodes: patch`/`pods/eviction`; the legacy controller cannot be disabled, so its permissions stay.
6. **Medium:** `pause` stops all scaling of that workload; it is a stop switch, not a degraded mode.
7. **Medium:** base images and actions are pinned by tag, not by digest/commit.

## 7. RBAC, SBOM, scanning, signing

| Item | Status |
|---|---|
| RBAC reduction (unused verbs) | VERIFIED: restricted-user tests + allow-list + negative controls (`docs/RBAC.md`) |
| "Least privilege" | NOT claimed for every install: reduced and verified for the current feature set only |
| Go SBOM (CycloneDX) | VERIFIED (generated, 62 components); not published |
| Image SBOM + provenance | UNVERIFIED (tags only) |
| Source vulnerability scan | CI govulncheck green on the PR head; BLOCKED locally (vuln.go.dev unreachable) |
| Image vulnerability scan | UNVERIFIED (workflow written, never run) |
| Signing | mechanics VERIFIED (ephemeral key, tamper negative control); keyless/registry path UNVERIFIED; **no image is signed** |

## 8. PR #31 and the repository (not changed)

PR #31 (`claude/bold-carson-5479vm` to `main`): open, mergeable state clean, 12 of 12 checks green on `978656a`; it contains the cost-pressure
oscillation fix, the pause annotation, the first simulator and the previous report. This pass's work is **uncommitted in the working tree**, on top of that
commit. `main` is at `683b9d6`; the default branch of the repository is still `claude/bold-carson-5479vm`. No tag, release, merge or close happened.

## 9. Proposed isolated-cluster test

`docs/RELEASING.md`, section "Test in an isolated cluster": kind with metrics-server and KubeCost, `hack/helm-rehearsal.sh`, a `dryRun` autoscaler, the
live KubeCost contract test, load for a full cycle, then `docs/BENCHMARK_PROCEDURE.md`, the pause, a KubeCost outage and a restart.

## 10. Checklist before publishing v0.1.0

- [ ] Review and merge (or discard) this pass's changes and PR #31; CI green on `main`.
- [ ] `hack/helm-rehearsal.sh` passes on a kind cluster.
- [ ] `supply-chain` workflow run by hand: SBOM artifact produced, image scan acceptable.
- [ ] Live KubeCost contract test passes on a real installation; a day of `dryRun` decisions reviewed.
- [ ] Decide the default headroom (75/80 vs 80/90) for the sample and documentation after real measurements.
- [ ] Confirm the action versions used in `release.yml` and `supply-chain.yml` exist (`cosign-installer`, `trivy-action`); pin by commit if desired.
- [ ] `CHANGELOG.md` moved under the version; chart `version`/`appVersion` match.
- [ ] Explicit authorisation to create the tag; after it: watch `images`, `sign`, `chart`; make the GHCR packages public; verify signatures from a clean machine.
- [ ] Choose and confirm the repository default branch.
