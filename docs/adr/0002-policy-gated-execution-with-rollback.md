# ADR-0002: Policy-gated execution with recorded state and automatic rollback

Status: accepted, implemented 2026-11-09 (`internal/safety`, `internal/execute`, `cmd/rightsize`). Verified against a real
Kubernetes 1.33.4 lab cluster only (see docs/LAB.md); not verified on EKS/GKE/AKS.

## Context

ADR-0001 produces recommendations and cannot change anything. Applying one is the dangerous step: it must be impossible to
change a workload without an explicit authorisation, a failed or partial run must be recoverable, and a change that degrades the workload must be undone.

## Decision

1. **Separate the decision from the act.** `internal/safety` is a pure, fail-closed function `Evaluate(policy, recommendation, facts, now, recent) -> allowed + reasons`.
   An empty policy authorises nothing; unknown facts deny; every denial lists its reasons. It never computes a number and the recommender never sees it.
2. **Policy content:** global kill switch, namespace allow list (required), workload allow/deny globs, exclusion labels, per-step change cap, min/max bounds,
   minimum confidence, UTC maintenance windows, rate limit, mandatory human approval (annotation holding the exact recommendation ID, on by default),
   GitOps-managed workloads refused unless allowed, CPU change refused when an HPA scales on CPU, VPA in Auto mode refused,
   an already-applied or previously rolled-back ID refused.
3. **Execution is a library driven by a CLI (`rightsize`), not a controller.** Nothing changes unless a human runs `rightsize apply --execute --context <explicit>`.
   Default is dry-run. A controller that applies on its own would add a long-running writer with broad RBAC before we have evidence the numbers are good.
4. **State lives on the object.** The previous container resources, the applied ID, the actor and the time are written as annotations on the Deployment
   (not the pod template, so they do not cause a rollout). A crashed tool can still be rolled back with `rightsize rollback`. Updates use the object's resourceVersion:
   a concurrent change makes the update fail instead of being overwritten.
5. **Rollback is a pure rule plus a verified action.** `Judge` flags: OOMKilled, any restart (configurable), fewer available replicas than desired, unready pods,
   replica count changing during observation, SLO breached (optional Prometheus query), rollout not complete in time. The rollback restores the saved values,
   reads them back to verify, and waits for the rollout. A failed rollback is returned as an error and audited.
6. **Audit:** append-only JSONL events (decision, dry_run, applied, verified, degraded, rolled_back, apply_failed, rollback_failed) with actor and recommendation ID.
7. **Limits are never lowered** in v1 and no new limit is introduced; a stale recommendation (live requests differ from those it was computed for) is refused.

## Alternatives considered

* **Controller applying approved recommendations:** rejected for now (see 3); revisit after real-workload evidence.
* **In-place pod resize (`pods/resize`):** avoids a rollout but the change is lost at the next rollout and is not a durable desired state; the Deployment template is the source of truth.
* **PR-based change for GitOps workloads:** the right answer for Argo CD/Flux; not implemented. For now such workloads are refused.
* **Approvals through a CRD or an external system:** more surface; an annotation naming the exact ID is auditable and needs no new component.

## Consequences and known limits

* Changing the template causes a normal rolling update; there is no canary: for a Deployment the rolling update with `maxUnavailable` is the only staging, and a bad change is caught by the observation window.
* The rate limit counts namespaces' workloads that carry a recent `last-applied-at`; several changes to the same workload in the window count once.
* The observation window is short by design in the lab; production values must be chosen per workload (a nightly batch needs hours, not minutes).
* `Judge` cannot see latency or errors unless an SLO query is supplied; without one only Kubernetes-level signals are watched.
* Deployments, StatefulSets and DaemonSets (added the same day; OnDelete update strategies are refused because a template change would never roll out). Approval is stamped with the identity the API server reports (SelfSubjectReview) and the policy can restrict approvers.
