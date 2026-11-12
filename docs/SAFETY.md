# Safety model: policy, approval, execution, rollback, audit

Design: [ADR-0002](adr/0002-policy-gated-execution-with-rollback.md). Verified on a real Kubernetes 1.33.4 lab cluster (docs/LAB.md); not on a managed cloud cluster.

## What can change a workload

Only `rightsize apply --execute --context <name>` (a person runs it; Deployments, StatefulSets and DaemonSets), and only when **all** of these hold:

1. the policy file names the namespace (`allowNamespaces`); an empty policy authorises nothing;
2. the kill switch is off, the workload is not denied or excluded by label, and the recommendation is not an abstention;
3. confidence >= `minConfidence` (default Medium), each change <= `maxChangeFraction` (default 50%), inside min/max bounds;
4. the time is inside a maintenance window (if any are defined) and the rate limit allows it;
5. the workload is not managed by Argo CD / Flux (unless `allowGitOps`), no VPA in Auto mode targets it, and no HPA scales it on CPU when the CPU request changes (unless `allowCPUWithHPA`);
6. a human approved the exact recommendation ID with `rightsize approve` (it stamps `approved-recommendation`, `approved-by` = the username the API server reports through SelfSubjectReview, and `approved-at`); on by default, `requireApproval: false` turns it off explicitly, and `approvers: [...]` restricts who may approve;
7. the live requests still equal the ones the recommendation was computed for (otherwise it is stale), and no limit would be lowered or newly created;
8. the recommendation ID was not applied before and was not rolled back before.

Any API error or unknown fact while gathering these denies the change.

## Policy file

```yaml
allowNamespaces: [shop]              # required
allowWorkloads: ["shop/Deployment/*"]  # optional globs, namespace/Kind/name
denyWorkloads: ["shop/Deployment/billing"]
denyLabels: {autoscaling.kubecost.io/exclude: ""}   # "" = any value
maxChangeFraction: 0.5
minCPU: 0.05
maxCPU: 8
minMem: 67108864
minConfidence: Medium                # Low | Medium | High
windows: [{days: [Sat, Sun], start: "02:00", end: "05:00"}]   # UTC; end <= start wraps midnight
maxChanges: 1
per: 1h
# requireApproval: true   allowGitOps: false   allowCPUWithHPA: false   killSwitch: false
```

Unknown keys are rejected (strict parsing).

## Procedure

```bash
# 1. read-only recommendation (rightsize plan also reads HPA / VPA / exclusion label from the cluster; cmd/recommend only needs Prometheus)
go run ./cmd/rightsize plan --context my-staging --prometheus-url http://prometheus:9090 --namespace shop --workload api --container app > rec.json
# 2. see what the policy would say (dry run, changes nothing)
go run ./cmd/rightsize apply --recommendation rec.json --policy policy.yaml --context my-staging
# 3. a human reviews rec.json and approves that exact ID (the identity comes from the API server, not from a flag)
go run ./cmd/rightsize approve --recommendation rec.json --context my-staging
# 4. controlled change: apply, observe, roll back automatically on degradation
go run ./cmd/rightsize apply --recommendation rec.json --policy policy.yaml --context my-staging --execute --observe-for 15m
# manual rollback at any time
go run ./cmd/rightsize rollback --namespace shop --workload api --context my-staging
```

## Rollback triggers (`execute.Judge`)

OOMKilled container, restarts above `--max-restarts` (default 0), fewer available replicas than desired, unready pods, replica count changed during observation
(for example an HPA reacting), SLO breached (`--slo-prometheus-url` + `--slo-breach-query`), rollout not complete within `--rollout-timeout`.
If observation itself fails, the change is rolled back (fail safe). The rollback restores the saved values, reads them back to verify, and waits for the rollout.
A failed rollback is an error and an audit event: a human must act.

## Audit

One JSON object per line (stderr always, plus `--audit-file`): `decision`, `dry_run`, `applied`, `verified`, `degraded`, `rolled_back`, `apply_failed`, `rollback_failed`, `inspect_failed`,
each with time, actor, recommendation ID, target and details (previous values, reasons). No secrets are written. The audit file is only as tamper-proof as its storage;
ship it to append-only storage in production. The previous state is also kept in annotations on the Deployment.

Since the ninth pass `--audit-file` is **hash-chained**: each event carries `prev` (the previous event's hash) and `hash` (SHA-256 over `prev` and the event), and a new run continues the
chain of the existing file. `rightsize audit-verify --audit-file f` reports the first line that was edited, deleted, reordered or inserted (lines written before chaining are accepted only
at the start). A file can still be truncated at its end or replaced whole by whoever can write it, so for an audit trail that must hold up, also ship the events off the machine:
`--audit-url https://collector/... [--audit-token-file t]` POSTs each event as JSON to an append-only collector (a log store, or a writer to a WORM / object-lock bucket). The sender never
blocks the command, retries each event, and reports at exit how many it could not deliver. The operator's applier does the same with `--audit-url` / `--audit-token-file`
(chart: `operator.recommendationApplier.auditURL` and `auditTokenSecret: {name, key}`; https only), in addition to the Kubernetes Events it always writes.


## Signed approvals (optional, `requireSignedApproval`)

`rightsize keygen --out alice` writes `alice.key` (Ed25519, PKCS#8, mode 0600: it stays with the approver and never enters the cluster) and `alice.pub`, and prints the key ID.
`rightsize approve --sign-key alice.key` (also with `--cr`) signs `kubecost-rightsize-approval/v1`, the recommendation ID, the target `namespace/kind/name`, the SHA-256 of the full record,
the approver's username (from SelfSubjectReview) and the approval time, and stores `<keyID>:<signature>` in `autoscaling.kubecost.io/approval-signature` (or `status.approval.signature`).

With `requireSignedApproval: true` and `approverKeys: {<keyID>: <public key, PEM or base64>}` in the policy, an approval counts only if its signature verifies with a listed key.
Refused: an unsigned approval, an unknown key, a key whose ID does not match its map entry, a forged `approved-by` or `approved-at`, and an approval of another record that reuses the ID.
For a combined (multi-container) record the signature covers the whole record. Verified: unit tests and an end-to-end test on the fake clientset (`internal/approval`, `internal/execute/chain_test.go`);
see docs/LAB.md for the run on a real cluster.

## Known limits

* The change is a normal rolling update of the workload (no canary beyond the observation window).
* The rate limit counts workloads with a recent `last-applied-at`; several changes to one workload within the window count once.
* Without an SLO query only Kubernetes-level signals are watched (no latency or error rate).
* Approval is an annotation (or `status.approval`): anyone with `update` on the object can write it, including a forged `approved-by`, unless the policy sets `requireSignedApproval` (below). `rightsize approve` takes the name from the API server, and the Kubernetes audit log of the writer is the authoritative record; restrict `update` with RBAC (`deploy/rbac/rightsize.yaml`, proven in docs/LAB.md result 4).
* The actor in the tool's own audit log is the OS user (or `--actor`); it is not authenticated. The approver identity is.
* OnDelete update strategies are refused. For DaemonSets the replica-count check is disabled (the desired count follows the node count).
* Not a controller: nothing is applied unless a person runs the command.


## The operator applier (opt-in, off by default)

Design: [ADR-0003](adr/0003-recommendation-crd-and-opt-in-applier.md). Enable with the chart value `operator.recommendationApplier.enabled=true` (flag `--enable-recommendation-applier`).

Flow: `rightsize plan --publish` creates a `WorkloadRecommendation` in the workload's namespace (changes nothing) -> a person with the `kubecost-rightsize-approve` role runs
`rightsize approve --cr --recommendation rec.json` (writes `status.approval` through the status subresource, identity from the API server) -> the operator evaluates the **same policy** as the CLI.
Phases: `Pending` (no approval), `Denied` (reasons in `status.reasons`), `DryRun` (approved and allowed, but the operator is in observe-only), `Applying`, `Verified`, `RolledBack`, `Failed`.

What gates it, in addition to everything in the CLI path:

* **Observe-only first.** The chart default is `operator.observeOnly=true`: an approved, allowed recommendation only becomes `DryRun`. Writing needs `--set operator.observeOnly=false` as a separate, deliberate step.
* **A policy ConfigMap** (`<release>-applier-policy`, key `policy.yaml`, optional `slo.<name>` keys) read on every reconcile. The chart default authorises no namespace. A missing, unreadable or malformed ConfigMap means `Denied`. The kill switch takes effect on the next look, without a restart.
* **Same-namespace rule.** The recommendation can only act on a workload in the namespace of the object that carries it.
* **Separate permissions.** Publishing (`create workloadrecommendations`) and approving (`update workloadrecommendations/status`) are different roles (`deploy/rbac/rightsize.yaml`). Annotations on the workload are ignored by this path, so forging one does nothing.
* **Narrow RBAC for the operator.** With `operator.recommendationApplier.namespaces` set, write access to workloads exists only as Roles in those namespaces; the default operator role is unchanged (tested in `internal/rbaccheck`).
* **Crash safety.** If the operator stops while a change is being observed, it does not roll back (stopping to look is not evidence of a problem); on restart it resumes from the state recorded on the workload and never applies twice.

Known limits: anyone holding both the publish and approve roles (or cluster-admin) can push a change through, so separation of duties is only as strong as your RBAC bindings; two reconcile workers, each blocked for up to `maxObserve` while a change is observed;
terminal phases are not revisited (a new decision needs a new recommendation). Verified on the lab cluster only (docs/LAB.md).


### Separation of duties enforced by the API server (applier path)

With `operator.recommendationApplier.enforceSeparationOfDuties=true` (the default when the applier is on; Kubernetes 1.30+) the chart installs three `ValidatingAdmissionPolicy` objects (CEL, `failurePolicy: Fail`, no webhook, no certificates):

1. **Creating** a `WorkloadRecommendation` requires the annotation `autoscaling.kubecost.io/published-by` to equal the authenticated user. The stamp cannot be forged (`rightsize publish` / `plan --publish` set it from the identity the API server reports).
2. **Writing `status.approval`** requires `approval.by` to equal the authenticated user, the approval to be for this exact recommendation, and the approver to be **different from the publisher**. A recommendation with no known publisher cannot be approved.
3. `published-by` **cannot be changed or removed** afterwards, not even by a cluster admin.

The operator's own status updates (phases) do not change `status.approval`, so they are not affected. Self-approval is therefore impossible for anyone, including people who hold both the publish and approve roles; a second, different person is needed. (Someone with two accounts can still use both: identity is whatever your authentication says.)
Verified on a real Kubernetes 1.33.4 cluster with users `alice` and `bob` by impersonation (docs/LAB.md result 10). Not enforced on the CLI path (`rightsize approve` without `--cr` writes an annotation on the workload).

### Per-team policies (applier)

A key `policy.<namespace>.yaml` in the policy ConfigMap **replaces** the global `policy.yaml` for that namespace (it must itself allow the namespace). A malformed team policy denies; it never falls back silently to the global one.

### Observability of the applier

Metrics (all labels are closed sets; no namespaces, names or IDs): `kubecost_recommendation_phase_transitions_total{phase}`, `kubecost_recommendation_estimated_savings_usd_total{outcome="kept|rolled_back"}` (an ESTIMATE, never a bill),
`kubecost_recommendation_apply_duration_seconds`. Alerts (with `prometheusRule.enabled`): `KubeCostRecommendationRolledBack`, `KubeCostRecommendationFailed`. Seen on a real cluster: the DryRun counter matched 60 objects; operator memory about 50 MiB with those 60 objects.

### Outages while a change is observed

If the workload (or the SLO query) cannot be read during observation, the executor keeps trying for `ObserveErrorTolerance` (2 minutes) before giving up. Unobserved time never counts as healthy time: the observation window restarts when reads work again.
If the outage outlasts the tolerance the change is rolled back as a precaution (fail safe); if that rollback also fails the object ends `Failed` and a human must look. Verified with the API server stopped for about 40 s during two observations (docs/LAB.md result 10).
