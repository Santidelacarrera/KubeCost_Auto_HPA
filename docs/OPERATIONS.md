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
