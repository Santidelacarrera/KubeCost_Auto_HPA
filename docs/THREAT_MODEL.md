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
| T11 | Compromised or swapped container image | Non-root image; local build, Trivy scan (0 findings on 6 OS + 64 Go packages), SBOM (72 components), cosign sign/verify with keyless Sigstore OIDC on GHCR. See `docs/SUPPLY_CHAIN.md` | verified (v0.1.0 published on GHCR and verified via Cosign) |
| T12 | Over-broad GitHub Actions token | `permissions: contents: read` by default, release jobs request more only where needed | verified by workflows run in CI/CD (Runs 42093064012 & 42100038053) |
| T13 | Operator deleted while the target is still scaled | Finalizer restores template resources before removal | verified (`TestFinalizerRestoresTemplateResourcesOnDelete`, `TestFinalizerNeverBlocksDeletion`) |
| T14 | Cost data from another cluster is used | `spec.cluster` disambiguates and ambiguous matches fail closed | verified (`TestClusterDisambiguation`) |
| T15 | Metrics backend unreachable or slow stalls reconciliation | Bounded timeouts, classified failures, freeze and recovery | verified (failure-class tests); **open**: no measured p99 under real load |
| T16 | Cross-tenant BOLA / IDOR via spoofed `X-Tenant-ID`, query `client_id` / `tenant_id`, or resource IDs (`cluster_id`, `command_id`, `event_id`, `decision_id`) | Centralized `_verify_tenant_access` in `engine/kubecost_engine/security.py`; identity derived exclusively from verified `kc_live_*`/`kch_live_*` API keys or admin tokens; per-resource ownership maps (`_cluster_owners`, `_command_owners`, `_event_owners`, `_decision_owners`) in `saas.py` | verified (`test_14_1_cross_tenant_isolation_bola_idor`) |
| T17 | Unauthenticated access to `/v1/*` routes or long-lived leaked API keys | Mandatory authentication on 100% of `/v1/*` GET/POST/PUT/DELETE/PATCH routes; API keys stored as SHA-256 hashes, verified via `hmac.compare_digest`, 90-day default expiration (`expires_in_days=90`), revocation and atomic rotation (`POST /v1/auth/keys/rotate`) | verified (`test_14_2_unauthenticated_access_blocked_on_all_v1_routes`, `test_14_3_api_key_lifecycle_expiration_revocation_rotation_and_audit`) |
| T18 | Multi-worker rate-limit bypass, IP spoofing via `X-Forwarded-For`, or unbounded daily evaluation spend | `DistributedRateLimiter` with atomic Redis Lua scripts (`REDIS_URL`), SQLite WAL counter persistence across restarts, fail-closed `HTTP 503` in production if Redis is down, `TRUST_PROXY_HEADERS` gating, and `RATE_LIMIT_DAILY_EVAL_QUOTA=10000` daily eval cap | verified (`test_14_4_rate_limiting_429_headers_daily_quota_and_persistence`, `test_14_4b_production_redis_fail_closed_and_proxy_ip_protection`) |
| T19 | Forged, replayed, or duplicate Stripe billing webhooks, or fallback webhook secret in production | Removed all fallback secrets; `process_stripe_webhook_event` fails closed with `HTTP 503` when `STRIPE_WEBHOOK_SECRET` is unset, verifies `Stripe-Signature` HMAC-SHA256 + 300s timestamp window (`HTTP 401`), and deduplicates `event.id` in SQLite (`stripe_webhook_events`) | verified (`test_14_6_stripe_webhook_hardening_fail_closed_hmac_replay_and_idempotency`) |
| T20 | Memory exhaustion (`DoS`), `NaN`/`Infinity` math poisoning, or injection via malformed identifiers | `_MAX_BODY_BYTES = 1 << 20` (`1 MiB`, `HTTP 413`) on all POST routes including `/v1/cluster/binpack`, strict `math.isfinite` + range checks, regex identifier validation, sanitized error responses, and HTTP security headers (`nosniff`, `Cache-Control: no-store`, `CSP frame-ancestors`) | verified (`test_14_5_input_validation_nan_inf_identifiers_1mib_binpack_and_headers`) |

## 4. Residual risks

1. **Live KubeCost is not validated.** A real response that differs from the OpenCost-shaped contract makes the operator freeze rather than scale. This is the main adoption risk.
2. **Pause is not a rollback.** It stops future writes; it does not restore replicas changed before the pause.
3. **Cluster-wide permissions.** The operator needs write access to `deployments/scale` and `pods/resize` cluster-wide. Namespace-scoped installs are not supported (`docs/RBAC.md`).
4. **Single writer assumption.** Two installations of the operator that target the same workload are not supported beyond the ownership checks above; this is not tested under concurrent multi-replica leader changes.

## 5. Reporting

Report vulnerabilities privately as described in `SECURITY.md` (GitHub private vulnerability reporting). Do not open a public issue.

## 6. Supply-chain controls (status)

* Images: non-root, built with `ldflags` version metadata (`operator/cmd/version.go`).
* SBOM (CycloneDX via `cyclonedx-gomod`), Trivy scan and cosign signing are validated in `.github/workflows/supply-chain.yml` and `release.yml`.
* Official release `v0.1.0` published on GHCR (multi-arch images & Helm chart OCI) cryptographically signed with Sigstore Cosign.
