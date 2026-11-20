# Operations guide

## Before you enable it

1. **Kubernetes ≥ 1.33** (in-place resize through `pods/resize`). Check: `kubectl api-resources | grep resize` is not enough; patch a test pod's CPU with `kubectl patch pod X --subresource resize ...`.
2. Target containers need a **CPU request** (and a memory request if `spec.memory` is used) and
   `resizePolicy: [{resourceName: cpu, restartPolicy: NotRequired}]`. The operator refuses `RestartContainer`.
3. Prometheus must expose RPS, p99 latency and per-pod CPU. If your metrics are not named `http_requests_total` /
   `http_request_duration_seconds`, set `spec.metrics.*Query` (templates may use `{{.Namespace}}` and `{{.PodRegex}}`):

   ```yaml
   spec:
     metrics:
       rpsQuery: 'sum(rate(nginx_ingress_controller_requests{namespace="{{.Namespace}}",service="{{.PodRegex}}"}[2m]))'
       p99Query: 'histogram_quantile(0.99, sum by (le)(rate(my_latency_bucket{namespace="{{.Namespace}}"}[2m]))) * 1000'
   ```
   Units: RPS = total req/s of all pods, p99 in **ms**, CPU in **millicores per pod**, memory in **MiB per pod**.
4. Do **not** put a second HPA on a Deployment that uses `spec.replicas`.

## Rolling out safely

1. Create the `KubeCostHPA` with `dryRun: true`. Read `kubectl get kchpa -o wide` and the `LastReason` in `status`: it
   states what the operator *would* do.
2. When decisions look right, set `dryRun: false`. Start with a low-risk workload and a generous `slo.p99LatencyMs`.
3. Pods that already exist are resized in place; new pods start from the Deployment template and are converged on the next
   reconcile. Deleting the `KubeCostHPA` returns the pods to the template resources (finalizer) — replica changes are not reverted.

## Recommended alerts

Enable `prometheusRule.enabled` in the Helm chart, or copy these:

| Alert | Expression | Why |
|---|---|---|
| Budget frozen | `kubecost_hpa_budget_frozen == 1` for 10m | Growth is blocked: SLO risk. |
| Spot drain failing | `increase(kubecost_spot_drains_total{result="failed"}[15m]) > 0` | Workloads may be interrupted ungracefully. |
| Operator down | `absent(up{job=~".*kubecost-auto-hpa.*"} == 1)` | Nothing is being evaluated. |
| Resize churn | `sum(increase(kubecost_hpa_resizes_total[1h])) by (namespace,name) > 12` | Flapping: widen `cooldown` or the SLO band. |

Useful panels (Grafana dashboard in `deploy/compose/grafana/dashboards`): cost per request, estimated hourly savings,
accumulated savings (`kubecost_hpa_savings_usd_total`), desired CPU, resizes by action.

## Failure behaviour

| Failure | Behaviour |
|---|---|
| Prometheus down | Last reading (≤ 15 min old) is used; otherwise **no action**, `Ready=False/NoMetrics`. |
| Engine down | No action, `Ready=False/EngineError`. |
| KubeCost down | `budget.onSpendUnavailable`: `Ignore` (default) or `FreezeGrowth`. |
| Redis down | Metrics are fetched directly and the cooldown falls back to `status.lastResizeTime`; scaling continues. |
| Operator restarts | State lives in the CR `status`; pods converge to `status.desired*` on the next reconcile. |
| Resize in flight / infeasible | The pod is skipped and retried on the next reconcile. |
| Spot node drain blocked by a PDB | Evictions are retried until `--drain-timeout`, then the node is left cordoned and retried. |

## Securing the data path

- Engine: `auth.engine.secretName` (bearer token) and `tls.engine.*` (TLS, mTLS) in the Helm chart.
- Prometheus / KubeCost: `operator.extraArgs` with `--prometheus-ca-file`, `--prometheus-bearer-token-file`, `--kubecost-*` (mount the secret with `operator.extraVolumes`).
- Redis: `--redis-tls --redis-ca-file=... [--redis-client-cert/--redis-client-key]` and `REDIS_PASSWORD`; enable `networkPolicy.enabled`.

## Upgrading Kubernetes / dependencies

`k8s.io/*` and `controller-runtime` move together and by hand (Dependabot only proposes patch updates):
bump all of them, run `make -C operator manifests`, then `make test`, `make envtest` and the `e2e-kind` CI job.

## Troubleshooting

- `Ready=False, reason=NoMetrics` → run the query printed in the message in Prometheus; check `spec.metrics`.
- `ResizeRefused` event → the container's `resizePolicy` is `RestartContainer`.
- Nothing happens and `LastReason` says *within band* → p99 is between 50 % and 90 % of the SLO: working as intended.
- Pods keep their old size after a rollout → expected until the next reconcile (≤ `interval`).


## Observe-only (global switch)

The chart installs the operator with `--observe-only=true` (`operator.observeOnly`). In this mode every resource is evaluated and
its decisions are reported (status, events, metrics) but **nothing is written**: no replica change, no in-place resize, no fallback
HPA, no Spot cordon/drain (that controller is not started). `spec.dryRun` per resource still works and is additive. To let the
operator act, review the reported decisions first, then `helm upgrade --set operator.observeOnly=false`.
Check it: `kubectl get kca -o jsonpath='{.items[*].status.conditions[?(@.type=="ScalingActive")].reason}'` should print `ObserveOnly`.
Not implemented here: per-namespace allow/deny lists, maintenance windows, approvals, canary, rollback on SLO degradation (see the report).
## Engine readiness and PostgreSQL failover

`/healthz` checks that the HTTP server is alive. `/readyz` queries the state store and
pings Redis when configured, returning 503 if either dependency fails. The mTLS chart
uses an exec probe against a separate listener bound to pod loopback.

For process failover, configure `engine.postgres.secretName` and `engine.replicas=2`.
Use a shared, durable PostgreSQL database, a verified TLS connection for remote
databases, and a direct or session-pooled connection. One engine holds the PostgreSQL
session lock. Standbys wait for it, then reload committed state before serving.
One ready pod is expected. For this configuration, check the engine Service's
ready endpoints after applying Helm changes. Helm `--wait` expects every replica
to become ready and is unsuitable for the standby Deployment. Session loss and
chart upgrades can interrupt requests while the replacement starts. This
configuration does not provide PostgreSQL failover itself or guarantee
zero-downtime upgrades. Keep backups and test recovery.

On SIGTERM the active engine stops accepting new requests, waits for requests
already in progress and then releases its database session for the standby.

Local Docker validation on 2026-11-19 also exercised SIGKILL and termination of
the active PostgreSQL session. The standby recovered persisted registration
state and accepted new writes in all three cases. Run
`KCA_FAILOVER_SMOKE=1 python -m pytest validation/test_failover_runtime.py -q -s`
to reproduce against disposable containers. This does not exercise database
server failover, network partitions, or production availability guarantees.

With bundled Redis the chart derives its DNS name from the release. With external
Redis, set `telemetry.redisUrl` explicitly if the engine should use it.

## Assisted node consolidation

Register the target under its `kube-system` namespace UID as the cluster ID. Label
eligible nodes `autoscaling.kubecost.io/consolidation-enabled=true`. At least one
ready, schedulable node must remain outside that pool. Create an assisted plan and
approve it using a second principal. Start a claim within the plan's five-minute window
using an API key with `enterprise:read` and `consolidation:execute`.

```sh
python hack/consolidate_node.py --api https://engine.example.com --tenant my-org \
  --plan PLAN_ID --node NODE --context CONTEXT --cluster-uid KUBE_SYSTEM_UID
```

Supply the key through `KCA_CONSOLIDATION_TOKEN`. The executor needs read access to
nodes, pods and the `kube-system` namespace, and permission to patch nodes. It checks
the cluster identity, current inventory and server-side dry run before claiming a
plan. The default `empty-only` strategy cordons one empty candidate and records
the result. Rerun with the same credential to reconcile an interrupted empty-node
claim.

To approve eviction, set `execution_strategy: stateless-eviction` when creating
the assisted plan. The approval covers up to five pods of healthy Deployments
with two or more replicas and the Deployment label
`autoscaling.kubecost.io/eviction-enabled=true`. Each Deployment needs a
same-named PDB whose `matchLabels` selector matches the Deployment. It must allow
one eviction and protect at least all but one replica. The executor needs access
to read Deployments, ReplicaSets and PDBs and create `pods/eviction`.
It asks the API server to dry-run each eviction, submits one at a time and waits
for a new Ready pod on another node before proceeding. It stops on budget denial,
owner or inventory changes, or a scheduling timeout and attempts to uncordon.
After an interrupted eviction, stop the original executor and rerun its command
with the same credential and `--recover`. The API verifies claim ownership
before the executor releases the plan's cordon. Recovery can be retried after
another interruption. Pods already relocated stay at their destinations.
If the node is cordoned by another actor, inspect its reservation before proceeding.
An operator can review the plan annotation and undo the cordon with
`kubectl --context CONTEXT uncordon NODE`. After inspecting the node, clear the
plan marker with `kubectl --context CONTEXT annotate node NODE autoscaling.kubecost.io/consolidation-plan-`.
The cluster autoscaler decides when to release the node. Uncordoning restores
scheduling capacity. It cannot restore a pod that was already evicted.
