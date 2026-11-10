# ADR-0003: WorkloadRecommendation CRD and an opt-in applier controller

Status: accepted, implemented 2026-11-09 (`api/v1alpha1/workloadrecommendation_types.go`, `internal/controller/recommendation_applier.go`, chart `templates/applier.yaml`).
Verified on a real Kubernetes 1.33.4 lab cluster only (docs/LAB.md); not verified on a managed cloud cluster.

## Context

ADR-0001 phase 2 and ADR-0002 section 3 deliberately stopped short of two things: a cluster object that holds a recommendation, and a controller that applies one without a person
running a command. The reasons were (a) no evidence yet that the numbers are good and (b) a long-running writer with broad RBAC is a larger risk than a CLI a human starts.
(a) is still true (the defaults are untuned: see docs/RECOMMENDER.md and `rightsize backtest`). The owner asked for both anyway, so the design keeps every safety property of the CLI and adds none that weakens it.

## Decision

1. **`WorkloadRecommendation` (namespaced CRD)** stores the full recommendation JSON (`spec.record`), a display summary, the target and an optional *named* SLO query. Creating one changes nothing.
2. **Approval is a different permission from publishing.** The approval is `status.approval` {recommendationID, by, at}, written through the status subresource, so it needs `update workloadrecommendations/status` (role `kubecost-rightsize-approve`),
   while publishing needs `create workloadrecommendations` (role `kubecost-rightsize-publish`). `by` is the username the API server reports (SelfSubjectReview), not a typed name. Annotations on the workload are ignored by this path (`Executor.ApprovalOverride`).
3. **The applier is opt-in and off by default** (`--enable-recommendation-applier`, chart `operator.recommendationApplier.enabled=false`). When on, it runs the same `execute.Executor` and `safety.Evaluate` as the CLI. In addition:
   * the policy comes from a ConfigMap read on every reconcile (no restart needed for the kill switch); no ConfigMap, a malformed one or an unknown key means **Denied**, never a default policy;
   * the recommendation can only act in **its own namespace** (the record's namespace must equal the object's);
   * it honours `--observe-only` (chart default true): approved and allowed becomes phase `DryRun`, no write;
   * SLO queries are looked up by name in the same ConfigMap (`slo.<name>`); free-form PromQL in a CR is not accepted, because anyone who can create a CR must not be able to make the operator run arbitrary queries;
   * observation length is capped by `--applier-max-observe`;
   * a shutdown during observation does not roll back (stopping to look is not evidence of a problem); the phase stays `Applying` and the next process **resumes** from the state recorded on the workload, never re-applying.
4. **RBAC is separate and narrow.** The applier's permissions are not in the default operator role. With `operator.recommendationApplier.namespaces` set, write access to workloads is granted by **Roles in those namespaces only**; with an empty list it is a distinct, named ClusterRole.
   The default policy shipped in the chart authorises no namespace. Tests (`internal/rbaccheck`) fix: nothing of the applier exists by default, no cluster-wide workload write when namespaces are given, the ConfigMap read is get-by-name only.

## Alternatives considered

* **Approval as a spec field or annotation on the CR:** anyone with `update` on the object could approve their own recommendation. The status subresource gives approval its own RBAC verb.
* **A webhook that validates approvals:** replaced by native ValidatingAdmissionPolicy (CEL), which needs no component or certificates and has no availability risk beyond the API server itself.
* **Applying through server-side apply to cooperate with GitOps:** does not solve the reconciler fight; GitOps-managed workloads are refused instead (docs/GITOPS.md).
* **Free-form SLO query in the CR:** rejected (see 3).

## Consequences and known limits

* Separation of duties is enforced by the API server with three ValidatingAdmissionPolicies (published-by must be the real user, the approver must be a different real user, published-by is immutable); it needs Kubernetes 1.30+ and can be switched off. One person with two identities can still publish as one and approve as the other.
* Two workers; an observation blocks one for up to `maxObserve`. Many simultaneous approvals queue.
* Terminal phases (`Verified`, `RolledBack`, `Failed`) are not revisited; a new decision needs a new recommendation (new ID).
* The controller has not been run against more than a handful of objects, nor under API server failures beyond what the unit tests inject.
* Rate limiting counts workloads with a recent `last-applied-at` (docs/SAFETY.md known limits).
