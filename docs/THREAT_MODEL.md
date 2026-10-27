# Threat model

Scope: the KubeCost_Auto_HPA operator (`KubeCostAutoscaler`, legacy `KubeCostHPA`), its decision engine, its Helm
chart and its CI/release pipeline. Out of scope: KubeCost, Prometheus, Redis and the cluster itself, except where the
operator depends on them.

Status legend: **verified** = covered by a test that runs in CI or locally; **assumed** = relied on, not tested here;
**open** = known gap.

## 1. Assets

| Asset | Why it matters |
|---|---|
| Replica count of target Deployments | Scaling errors cause outages (too few) or cost (too many). |
| Pod resource requests (in-place resize) | Wrong requests cause throttling or OOM. |
| KubeCost API credentials (bearer token file) | Read access to cost data; must not leak. |
| Cluster state of other workloads | The operator must not touch what it does not own. |
| Release artifacts (images, chart) | A tampered artifact runs with the operator's RBAC. |

## 2. Trust boundaries and actors

* **Cluster users with write access to `KubeCostAutoscaler` objects.** They choose the target, bounds and policy. Trusted to configure, not to bypass ownership checks.
* **KubeCost / metrics backends.** Untrusted for correctness: responses are validated and fail closed (`internal/kubecost`, contract tests). Verified against fixtures only (**open**: live KubeCost, see `docs/PILOT.md`).
* **Other controllers** (a native HPA, another `KubeCostAutoscaler`, a human running `kubectl scale`). The operator detects ownership conflicts and external replica changes. Verified in envtest (`TestRealAPIServer_NativeHPAConflictBlocksAllWrites`, `TestOnlyOneOfTwoAutoscalersOwnsTheTarget`, `TestReplicaChangeByAnotherActorMakesTheCostSignalUnstable`).
* **The release pipeline.** Trusted only through its controls (section 6).

## 3. Threats and controls

| # | Threat | Control | Status |
|---|---|---|---|
| T1 | Stale or missing cost data drives a scale-down | Sample age limit, coverage ≥ 50 % of the window, freeze on unavailable cost | verified (unit + envtest + sim) |
| T2 | Malformed or drifted KubeCost response is read as zero cost | Strict decoding, typed errors, `drift_namespace_is_number.json` fixture | verified (contract tests) |
| T3 | Bearer token sent to another host through a redirect | Redirects never followed; token only from a file; URL may not embed credentials | verified (`TestCredentialsNeverAppearInErrors`, `TestParseBaseURL`) |
| T4 | Token written to logs or status | Error messages carry no headers; checked in the manager test log | verified for logs in envtest; **assumed** for every future log line |
| T5 | Query storm against KubeCost | 20 s per-workload cache (errors included), 4 in-flight requests, ≤ 3 attempts with backoff, `Retry-After` honoured up to 30 s | verified (unit, `-race`) |
| T6 | Controller fights a native HPA or another controller | Ownership check blocks writes; fallback HPA is owned and removed on recovery | verified (envtest) |
| T7 | Runaway scaling from a bad signal | Min/max bounds, cooldown, stabilisation windows, budget guardrail, dry-run mode | verified (unit, sim) |
| T8 | Stabilisation state corrupted or set in the future freezes scaling | Future timestamps replaced with now and persisted once | verified (`TestFutureStabilisationTimestampCannotFreezeScaling`) |
| T9 | Operator used to modify unrelated workloads | Minimal RBAC generated from markers; a user bound only to that ClusterRole is tested for allowed and denied verbs | verified (envtest restricted user, `internal/rbaccheck`) |
| T10 | Operator cannot be stopped quickly during an incident | Pause annotation `autoscaling.kubecost.io/pause="true"` forces dry-run, no writes; removing it resumes without restart | verified in unit and against a real apiserver (`TestRealAPIServer_PauseWritesNothingAndResumeRevertsNothing`, with a negative control that fails when the pause check is removed); **open**: pause does not revert past changes |
| T11 | Compromised or swapped container image | Non-root image; local build, Trivy scan (0 findings on 6 OS + 64 Go packages), SBOM (72 components), cosign sign/verify with an ephemeral key. See `docs/SUPPLY_CHAIN.md` | **open**: keyless signing and provenance not validated; the release image was never published or scanned in the registry |
| T12 | Over-broad GitHub Actions token | `permissions: contents: read` by default, release jobs request more only where needed | verified by reading workflows and `actionlint`; **open**: not run on GitHub by this change |
| T13 | Operator deleted while the target is still scaled | Finalizer restores template resources before removal | verified (`TestFinalizerRestoresTemplateResourcesOnDelete`, `TestFinalizerNeverBlocksDeletion`) |
| T14 | Cost data from another cluster is used | `spec.cluster` disambiguates and ambiguous matches fail closed | verified (`TestClusterDisambiguation`) |
| T15 | Metrics backend unreachable or slow stalls reconciliation | Bounded timeouts, classified failures, freeze and recovery | verified (failure-class tests); **open**: no measured p99 under real load |

## 4. Residual risks

1. **Live KubeCost is not validated.** A real response that differs from the OpenCost-shaped contract makes the operator freeze rather than scale. This is the main adoption risk.
2. **Pause is not a rollback.** It stops future writes; it does not restore replicas changed before the pause.
3. **Cluster-wide permissions.** The operator needs write access to `deployments/scale` and `pods/resize` cluster-wide. Namespace-scoped installs are not supported (`docs/RBAC.md`).
4. **Single writer assumption.** Two installations of the operator that target the same workload are not supported beyond the ownership checks above; this is not tested under concurrent multi-replica leader changes.

## 5. Reporting

Report vulnerabilities privately as described in `SECURITY.md` (GitHub private vulnerability reporting). Do not open a public issue.

## 6. Supply-chain controls (status)

* Images: non-root, built with `ldflags` version metadata (`operator/cmd/version.go`).
* SBOM (CycloneDX via `cyclonedx-gomod`), Trivy scan and cosign signing are defined in `.github/workflows/supply-chain.yml` and `release.yml`. **None of them has run against a published image yet.** See `docs/SUPPLY_CHAIN.md` for the exact state.
* No release has been published.
