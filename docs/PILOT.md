# Pilot guide

This guide is for a platform team that wants to evaluate KubeCost_Auto_HPA in a **non-production** cluster first.
It states what is verified, what is not, and the exit criteria before anyone should rely on it.

**Status:** the operator is not validated against a live KubeCost installation and has not been run on a real
workload cluster. Treat any pilot as an evaluation until the checks in §8 are complete.

## 1. Architecture (short)

* `KubeCostAutoscaler` (cluster-scoped operator controller) reads demand from `metrics.k8s.io`, cost from the KubeCost
  allocation API (`GET /model/allocation`, aggregated per controller), and writes replicas through the
  `deployments/scale` subresource. Optional in-place memory and CPU resize goes through `pods/resize`.
* The decision engine is pure Go (`internal/policy`): given demand, cost, bounds and history it returns one decision
  with its reasons. Its outcome is written to status and events; nothing else reads or writes the target.
* A single writer per target: if a native HPA or another autoscaler owns the workload, the operator does not write
  and reports a conflict.
* Failure states freeze scaling. A native HPA can be installed as a fallback only when resource metrics work, and it
  is removed on recovery.

Details: `docs/KUBECOST_AUTOSCALER.md`. Threats: `docs/THREAT_MODEL.md`.

## 2. Requirements and compatibility

| Component | Tested in CI | Evidence |
|---|---|---|
| Kubernetes | 1.33 (CI: kind 1.33 with metrics-server) | `e2e-kind`, `e2e-kca` in CI; envtest uses kube-apiserver 1.33.4 |
| metrics-server | Required for demand | `e2e-kca` |
| KubeCost / OpenCost allocation API | Shape from OpenCost source | **Not validated against a live installation.** Only fixtures and a simulated KubeCost in CI |
| Redis | Optional (cache, TLS supported) | `TestRedisDownDegradesGracefully` |
| Go (build) | 1.26 | `operator/go.mod` |

**Versions of KubeCost are not yet certified.** Record the exact KubeCost chart and app version in your pilot report.

## 3. Install (reproducible, non-production)

```bash
# 1. Create the namespace and the KubeCost token secret (key: token). Do not commit it.
kubectl create namespace kubecost-auto-hpa
kubectl -n kubecost-auto-hpa create secret generic kubecost-token --from-file=token=./kubecost-token.txt

# 2. Install with the optional controllers you need. Values are validated by values.schema.json.
helm upgrade --install kubecost-auto-hpa deploy/helm/kubecost-auto-hpa -n kubecost-auto-hpa \
  --set kubecost.url=http://kubecost-cost-analyzer.kubecost:9090 \
  --set auth.kubecost.secretName=kubecost-token   # key defaults to "token"

# 3. Confirm the RBAC you actually granted before going further.
helm template kubecost-auto-hpa deploy/helm/kubecost-auto-hpa | less   # review ClusterRole rules
```

The chart key names above are taken from `deploy/helm/kubecost-auto-hpa/values.yaml`; verify them against your chart
version with `helm template` before installing. `docs/RBAC.md` lists every verb and why it exists.

Start every target with `spec.dryRun: true` (as in `operator/config/samples/autoscaling_v1alpha1_kubecostautoscaler.yaml`)
and read `status.lastDecision` for at least two full cost windows before setting it to `false`.

## 4. KubeCost configuration

* Use one KubeCost installation per pilot cluster, reachable over HTTP(S) from the operator namespace.
* Create a token with read-only allocation access. The operator never needs write access to KubeCost.
* Prefer HTTPS with a certificate the operator trusts. The client refuses URLs with embedded credentials or query
  strings and never follows redirects.
* Multi-cluster KubeCost: set `spec.cluster`. Without it, an ambiguous match fails closed.
* Expect `CostNoData` for workloads younger than half of the cost window; this is by design.

## 5. Permissions

The operator's ClusterRole is generated from kubebuilder markers and mirrored in the chart. The restricted-user
envtest proves that a user bound only to that role can do what the operator needs and nothing else on the
resources tested. Re-run `make test-all` after any RBAC change. See `docs/RBAC.md`.

Namespace-scoped installs are not supported yet.

## 6. Failure policy

| Condition | Behaviour | Visible as |
|---|---|---|
| KubeCost unreachable, timeout, 5xx/429 | Retry (≤ 3 attempts), then freeze; a safety scale-up is still allowed when demand requires it | `Degraded`, condition reason `CostTransient` |
| KubeCost 401/403 | No retry; freeze | `CostAuth` |
| Malformed response or drifted field | No retry; freeze, never read as zero | `CostMalformed` |
| Sample older than `maxMetricAge` or window not covered | Freeze | `CostStale` |
| Workload not in cost data | Freeze | `CostNoData` |
| Resource metrics missing | Hold; fallback HPA only if its preconditions hold | `DemandUnavailable` |
| Kubernetes API errors | Nothing written; retry with backoff | Events, `reconcile` error metrics |
| Future stabilisation timestamp | Replaced with now once, window bounded | Status updated |

A freeze holds replicas where they are. The only write allowed during a freeze is an authorised safety scale-up
when demand is already past the safety ceiling (`TestFreezeStillAllowsAuthorisedSafetyScaleUp`).

## 7. Pause, recovery and rollback

* **Pause:** `kubectl annotate kubecostautoscaler <name> autoscaling.kubecost.io/pause=true`. The controller keeps
  computing and reporting decisions, forces dry-run, and writes nothing. Verified by unit and envtest tests.
  **Pause does not undo earlier scaling.** Replicas stay where they are.
* **Resume:** remove the annotation. No restart is needed. Stabilisation state is kept in status, so a resume
  does not bypass cooldowns.
* **Rollback of a single target:** delete the `KubeCostAutoscaler`. The finalizer restores the template resources of
  the target (requests and limits changed by in-place resize). Then set the Deployment's replicas as needed.
* **Rollback of the operator:** `helm rollback kubecost-auto-hpa <revision>`; CRDs are not removed by Helm, so
  objects survive a rollback.
* **Emergency stop of the whole operator:** scale the operator Deployment to zero. Targets keep their last replica
  count; no controller writes during that time.

## 8. Pilot exit criteria (before any production decision)

- [ ] Integration test `go test -tags kubecost_live -run Live ./internal/kubecost` passes against your KubeCost
      version, with the version recorded.
- [ ] `make test-all` passes on the exact commit being piloted.
- [ ] Dry-run decisions for ≥ 2 cost windows reviewed against what a human would have done.
- [ ] Pause and resume exercised on the pilot cluster; observed behaviour matches §7.
- [ ] Alerts (`docs/OBSERVABILITY.md`) fire in a deliberate KubeCost outage.
- [ ] Image scan, SBOM and signature verified on the image you actually deploy (`docs/SUPPLY_CHAIN.md`).
- [ ] Benchmark on the pilot's own traces (`docs/BENCHMARK_PROCEDURE.md`). Simulator results are not savings
      estimates for your cluster.

## 9. Known limitations

* No live KubeCost validation (see §2).
* Demand is CPU and memory from `metrics.k8s.io` only; custom or external metrics are not supported.
* Cluster-wide permissions only.
* Pause is a stop, not a rollback.
* The simulator is synthetic; no production latency or cost measurement exists yet.
* SLOs are proposed, not measured (`docs/OBSERVABILITY.md`).

## 10. Support

Report vulnerabilities as described in `SECURITY.md`. For functional issues, open a GitHub issue with the operator
version, the KubeCost version, the `KubeCostAutoscaler` status and the controller logs with secrets removed.

## 11. Live KubeCost validation (staging) — procedure

Run this against a **staging** KubeCost only, with a read-only token, and record the results. Nothing in this section
has been run against a real KubeCost yet.

**Prerequisites**
- A staging KubeCost you are authorised to query, reachable from the machine running the test (for example through
  `kubectl port-forward -n <kubecost-ns> svc/kubecost-cost-analyzer 9090`).
- A token with read-only allocation access. Create it outside the repository and load it from the environment or a
  secret manager. Never write it into the repo, a shell history, or a CI log.
- A Deployment in that cluster that has been running for at least one cost window (the test does not create it).
- Go 1.26 and this repository at the commit being validated.

**Variables** (all read at runtime; none has a secret default)

| Variable | Required | Default | Meaning |
|---|---|---|---|
| `KUBECOST_LIVE_URL` | yes | none | Base URL, `http(s)` only, no credentials or query. Without it the test is **skipped**. |
| `KUBECOST_LIVE_NAMESPACE` | yes | none | Namespace of the target workload. |
| `KUBECOST_LIVE_CONTROLLER` | yes | none | Workload name. |
| `KUBECOST_LIVE_TOKEN` | yes for secured installs | empty | Bearer token, from the runtime secret store. |
| `KUBECOST_LIVE_KIND` | no | `deployment` | Controller kind. |
| `KUBECOST_LIVE_WINDOW` | no | `1h` | Allocation window. |
| `KUBECOST_LIVE_MAX_AGE` | no | `5m` | Freshness bound; equals the CRD default `failurePolicy.maxMetricAge`. |

```bash
# 1. Record the exact KubeCost version and how you reach it (chart and app).
helm list -n <kubecost-ns>
# 2. Run the live test. Use your secret manager to provide the token; do not paste it inline.
cd operator
KUBECOST_LIVE_URL="http://127.0.0.1:9090" \
KUBECOST_LIVE_NAMESPACE="<namespace>" KUBECOST_LIVE_CONTROLLER="<deployment>" \
KUBECOST_LIVE_TOKEN="${KUBECOST_LIVE_TOKEN:?load from the secret store}" \
go test -tags kubecost_live -run Live -v -count=1 ./internal/kubecost/
```

**Pass criteria (all required):** the test reports `PASS`; the log line `sample:` shows a positive `hourly`, a
`windowEnd` within `KUBECOST_LIVE_MAX_AGE`, and `minutes` covering at least half the window. Record the KubeCost
version, the access method and the sample line (no token).

Interpretation:

| Result | Meaning | Action |
|---|---|---|
| `SKIP` | `KUBECOST_LIVE_URL` not set. **Not a pass.** | Set the variables and rerun. |
| `FAIL ... connection refused` / timeout | The URL is not reachable from this machine. | Fix the port-forward or network path; not a product result. |
| `FAIL: the documented contract did not hold` | The real response differs from `operator/internal/kubecost/testdata/README.md`. | Do not pilot. Save the response with secrets removed and compare field by field. |
| `FAIL ... HTTP 401/403` | Token rejected or missing permission. | Check the token scope. Do not retry in a loop. |
| `FAIL: a running workload must have a positive hourly cost` | Cost is zero or missing for a running workload. | Check aggregation (`aggregate=controller`) and the cluster name. |
| `FAIL: sample failed freshness/coverage validation` | Data is older than `KUBECOST_LIVE_MAX_AGE` or covers less than half the window. | Expected freeze behaviour; decide whether the staging lag is acceptable. |
| `PASS` | The contract holds for this installation, version and workload. | Record it. It does not validate other versions. |

Also record: HTTP status codes seen, response time, whether the token was accepted, and the KubeCost version. Do not
paste the token or the full response if it contains internal names you cannot share.

The live test checks the response shape and the cost arithmetic. It does **not** test 429/503 behaviour, timeouts or
outages; those are covered with fixtures and local servers only (`internal/kubecost`).

## 12. Validation status and how to close each gap

| Item | Status | Evidence / how to close |
|---|---|---|
| Automated tests (`make test-all`, race, envtest, integration) | **Validated** | Local run and GitHub `envtest`, `operator`, `engine`, `manifests` jobs on the PR head. |
| Helm install, upgrade, rollback, uninstall on a real apiserver (no kubelet) | **Validated** on kube-apiserver 1.33.4 + etcd | `hack/helm-lifecycle.sh` with `KUBECONFIG` pointing at a disposable cluster. No pods run in this mode. |
| Helm lifecycle with pods (`hack/helm-rehearsal.sh`, kind) | **Pending.** Not run in the sandbox: kind nodes cannot start pod sandboxes there (nested cgroup v1). GitHub `e2e-kind` on the PR head is still in progress; **not approved until it passes** | Run on a host where `kind create cluster` works, with `NODE_IMAGE=kindest/node:v1.33.4` set explicitly (the script default is v1.33.1). |
| Live KubeCost contract (`kubecost_live`) | **Pending.** No instance or token available | Section 11. Without `KUBECOST_LIVE_URL` the test is skipped, not passed. Verified in this round: unreachable URL → FAIL with the connection error. |
| 429/503/`Retry-After`, concurrency cap, stale or missing cost | **Validated with fixtures and local servers only** | `TestRetryAfterHonouredAndCapped`, `TestInFlightRequestsAreBounded`, `TestFailureClassesAllFreeze`, `TestKubeCostOutageFreezesThenRecovers`. Not against KubeCost. |
| Pause, no writes while paused, no revert on resume | **Validated on a real apiserver**, with a negative control | `TestRealAPIServer_PauseWritesNothingAndResumeRevertsNothing`. |
| Latency p50/p95/p99 under controlled load | **Pending.** Needs a cluster with real traffic and a real KubeCost | `docs/BENCHMARK_PROCEDURE.md`. No numbers are reported in this repo. |
| Image scan, SBOM, signing | Local only | `docs/SUPPLY_CHAIN.md`. Keyless signing and provenance are **not** validated. |

**Pilot entry criteria.** Move from development to a controlled pilot in staging when all of these hold:
1. The live KubeCost test passes against the staging version, with the version recorded.
2. `hack/helm-rehearsal.sh` passes on kind, including upgrade and rollback.
3. The pause and rollback steps in section 7 are exercised once on the pilot cluster.
4. A staging benchmark exists with recorded p50, p95, p99 and error rate, and the team has accepted the thresholds.

## 13. Operational checklist for external validation

Do **not** mark any item done until its evidence is recorded. Items marked "blocked" need an owner outside this repo.

### A. Live KubeCost in an authorised staging cluster (blocked: needs URL and token)
Prerequisites: §11 variables; a workload running for at least one cost window; port-forward or network path.
1. `helm list -n <kubecost-ns>` → record chart and app version.
2. Run the command in §11 with the token from the secret store.
3. Accept only if: `PASS`, `sample:` shows positive hourly cost, `windowEnd` within `KUBECOST_LIVE_MAX_AGE`, coverage ≥ 50 %.
4. Record: version, access method, the `sample:` line. No token, no full response.

### B. Helm with pods on kind (blocked: needs a host where kind starts pods)
Prerequisites: Docker, kind, kubectl, helm; cgroups compatible with kind (not nested cgroup v1 in a sandbox).
```bash
NODE_IMAGE=kindest/node:v1.33.4 hack/helm-rehearsal.sh
```
Accept only if the script ends with `helm rehearsal passed` and prints `ok:` for install, upgrade (2 revisions),
rollback (dashboard removed) and uninstall (CRDs kept). A failure prints the pods and events of the release namespace.

### C. CI on the PR head (in progress at the time of writing)
`e2e-kind` and `e2e-kca` are **not approved** until both report `success` on the head commit. `engine`, `manifests`,
`envtest` and `operator` have passed on the head `fd17850` run `40559690710`; check the head again before reporting.

### D. Security and supply chain
- `govulncheck ./...` in `operator/` needs access to `vuln.go.dev`. Blocked in the sandbox (`Forbidden`). Accept only when the output says "No vulnerabilities found" or lists findings you have triaged.
- Trivy on a registry-pushed image of the release candidate, with the vulnerability database from the official source.
- Keyless signing and provenance: only through the release workflow with GitHub OIDC. Not validated.

### E. RBAC (decision needed from the owner)
Accept or reject cluster-wide permissions (`docs/RBAC.md`, "Essential permissions"). Any reduction needs `make test-all` and §B both passing afterwards.

### F. Benchmark (blocked: needs a cluster with load and KubeCost)
`docs/BENCHMARK_PROCEDURE.md`. Report p50, p95, p99 and error rate only from a run you executed; the simulator is not a measurement.

### G. Licence (decision needed from the owner)
Choose the copyright holder line (`docs/LICENSING.md` §1). Not decided here.

### Go or no-go for preproduction
Go only when A and B pass with recorded evidence, `make test-all` is green on the same commit, and E and G are decided.
