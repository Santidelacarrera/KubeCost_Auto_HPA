# Safety model: policy, approval, execution, rollback, audit

Design: [ADR-0002](adr/0002-policy-gated-execution-with-rollback.md). Verified on a real Kubernetes 1.33.4 lab cluster (docs/LAB.md); not on a managed cloud cluster.

## What can change a workload

Only `rightsize apply --execute --context <name>` (a person runs it), and only when **all** of these hold:

1. the policy file names the namespace (`allowNamespaces`); an empty policy authorises nothing;
2. the kill switch is off, the workload is not denied or excluded by label, and the recommendation is not an abstention;
3. confidence >= `minConfidence` (default Medium), each change <= `maxChangeFraction` (default 50%), inside min/max bounds;
4. the time is inside a maintenance window (if any are defined) and the rate limit allows it;
5. the workload is not managed by Argo CD / Flux (unless `allowGitOps`), no VPA in Auto mode targets it, and no HPA scales it on CPU when the CPU request changes (unless `allowCPUWithHPA`);
6. a human annotated the Deployment with `autoscaling.kubecost.io/approved-recommendation=<exact recommendation ID>` (on by default; `requireApproval: false` turns it off explicitly);
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
# 1. read-only recommendation
go run ./cmd/recommend --prometheus-url http://prometheus:9090 --namespace shop --workload api --container app > rec.json
# 2. see what the policy would say (dry run, changes nothing)
go run ./cmd/rightsize apply --recommendation rec.json --policy policy.yaml --context my-staging
# 3. a human reviews rec.json and approves that exact ID
kubectl --context my-staging -n shop annotate deploy api autoscaling.kubecost.io/approved-recommendation=$(jq -r .ID rec.json)
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

## Known limits

* Deployments only; the change is a normal rolling update (no canary beyond the observation window).
* The rate limit counts workloads with a recent `last-applied-at`; several changes to one workload within the window count once.
* Without an SLO query only Kubernetes-level signals are watched (no latency or error rate).
* Approval is an annotation: anyone with `update` on the Deployment can write it. Restrict that with RBAC; there is no separate identity check.
* Identity of the actor is the OS user (or `--actor`); it is not authenticated.
* Not a controller: nothing is applied unless a person runs the command.
