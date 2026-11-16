# Configuration

## The three operating modes

| Mode | What it does | How it is set | Default |
|---|---|---|---|
| **Observe-only** | Measures, recommends, reports. Writes nothing to workloads. Spot draining is off | Chart `operator.observeOnly: true` (operator flag `--observe-only`) | **On** |
| **Assisted** | A recommendation is applied only after a human approves that exact recommendation. The approver must be a different user from the publisher. The change is observed and rolled back automatically if it degrades | `operator.observeOnly: false`, `operator.recommendationApplier.enabled: true`, a policy that allows the namespace. `requireApproval` stays at its default, `true` | Off |
| **Automated** | Changes inside the policy's limits are applied without a per-change approval | Everything of Assisted, plus `requireApproval: false` in the policy | **Off**. Experimental: not validated on real traffic |

The modes nest. Automated cannot be reached by one setting: it needs observe-only turned off, the applier turned on, a
namespace allowed and the approval requirement explicitly removed. Whatever the mode, the deterministic policy below decides.
No recommendation, model output or engine answer can bypass it: the policy is evaluated in the operator, on the recommendation
record, after the recommendation was produced.

For one workload, the CLI gives the same three behaviours without the operator: `rightsize plan` (observe), `rightsize apply
--execute` with an approval (assisted), and the same with a policy that has `requireApproval: false` (automated).

### Moving from observe-only to assisted

1. Run in observe-only for at least a week. Read the recommendations (`kubectl get wrec -A`, `rightsize plan`).
2. Write the policy. Start with one non-critical namespace:

   ```yaml
   operator:
     observeOnly: false
     recommendationApplier:
       enabled: true
       namespaces: ["shop-staging"]     # RBAC to update workloads is granted only here
       policy: |
         allowNamespaces: ["shop-staging"]
         denyLabels: {"criticality": "high"}
         minConfidence: High
         maxChangeFraction: 0.3
         maxChanges: 2
         per: 1h
         windows:
           - {days: ["Tue", "Wed", "Thu"], start: "10:00", end: "16:00"}
   ```

3. `helm upgrade` with those values. Approve one recommendation (`rightsize approve --cr`), watch it reach `Verified`.

Turning the kill switch on (`killSwitch: true` in the policy ConfigMap) stops every apply without a restart.

## The policy (deterministic guardrails)

Full reference: [SAFETY.md](SAFETY.md). The zero value authorises nothing.

| Key | Default | Meaning |
|---|---|---|
| `killSwitch` | `false` | Denies everything while true |
| `allowNamespaces` | none | Only these namespaces may be changed |
| `allowWorkloads`, `denyWorkloads` | all of an allowed namespace / none | `namespace/Kind/name` globs. Deny wins |
| `denyLabels` | none | Workloads with these labels are never changed. Use it for critical workloads |
| `maxChangeFraction` | `0.5` | Largest change of one resource in one step |
| `minCPU`, `maxCPU`, `minMem`, `maxMem` | none | Absolute bounds |
| `minConfidence` | `Medium` | Lowest confidence that may be applied |
| `windows` | any time | Maintenance windows |
| `maxChanges` / `per` | 1 per `1h` | Rate limit per namespace |
| `maxRecommendationAge` | `168h` | Older recommendations are refused |
| `requireApproval` | `true` | Human approval of the exact recommendation ID |
| `approvers` | anyone with the permission | Usernames allowed to approve |
| `requireSignedApproval`, `approverKeys` | off | Ed25519-signed approvals |
| `allowGitOps` | `false` | Change a workload managed by Argo CD / Flux directly |
| `allowCPUWithHPA` | `false` | Change CPU requests under an HPA that scales on CPU |

A per-namespace policy overrides the global one: key `policy.<namespace>.yaml` in the same ConfigMap.

## Chart values

`deploy/helm/kubecost-auto-hpa/values.yaml` documents every value; `values.schema.json` rejects unknown keys.

| Value | Default | Notes |
|---|---|---|
| `operator.observeOnly` | `true` | See the modes above |
| `operator.replicas`, `operator.leaderElect` | `2`, `true` | Two replicas with leader election |
| `operator.recommendationApplier.*` | disabled | The assisted and automated modes |
| `operator.recommendationApplier.enforceSeparationOfDuties` | `true` | Admission policies: the approver is the authenticated user and is not the publisher |
| `operator.recommendationApplier.auditURL` | empty | Ship audit events to an append-only collector over https |
| `operator.enableSpotDrain` | `true` | Has no effect while `observeOnly` is true |
| `operator.enableCostAutoscaler` | `true` | The `KubeCostAutoscaler` controller |
| `operator.cpuPricePerCoreHour`, `operator.memPricePerGiBHour` | `0.031`, `0.004` | Fallback unit prices for estimates. Set your own: they are inputs, never looked up |
| `prometheus.url` | `http://prometheus-server.monitoring:80` | Required |
| `kubecost.url` | a Kubecost service address | Point it at OpenCost, or empty to disable budgets |
| `redis.enabled` | `true` | Bundled cache, or `redis.external.addr` |
| `auth.*.secretName` | empty | Bearer tokens for the engine, Prometheus and the cost source, from Secrets |
| `tls.engine.*` | empty | TLS and mTLS between operator and engine |
| `networkPolicy.enabled` | `false` | Restrict ingress to the engine and Redis; optional operator egress rules |
| `serviceMonitor.enabled`, `prometheusRule.enabled`, `grafanaDashboard.enabled` | `false` | Need the Prometheus Operator CRDs / Grafana sidecar |
| `telemetry.*` | disabled | Publishes decisions to a Redis stream you name. Nothing leaves the cluster unless you point it elsewhere |

## Operator flags

`kubecost-auto-hpa-operator --help` prints all of them. The ones that change behaviour:

| Flag | Default | Notes |
|---|---|---|
| `--observe-only` | `false` in the binary, **`true` in the chart and the Kustomize base** | Global dry-run |
| `--enable-recommendation-applier` | `false` | Needs `--applier-policy-configmap` |
| `--applier-max-observe` | `15m` | Upper bound of the observation window after a change |
| `--enable-spot-drain` | `true` | Honours `--observe-only` |
| `--enable-cost-autoscaler` | `true` | Needs `--kubecost-url` |
| `--prometheus-url`, `--kubecost-url`, `--engine-url`, `--redis-addr` | see `--help` | Backends |
| `--<backend>-ca-file`, `--<backend>-client-cert`, `--<backend>-client-key`, `--<backend>-bearer-token-file` | empty | TLS, mTLS and tokens per backend |
| `--audit-url`, `--audit-token-file`, `--audit-ca-file` | empty | Audit shipping |

## Recommender settings

Defaults: 24 h minimum window, 200 samples, CPU p95 + 15%, memory p99 + 20%, at most half of the current request per step,
10% tolerance. They are untuned starting values ([RECOMMENDER.md](RECOMMENDER.md)). Every setting is a flag of `rightsize plan`;
`rightsize backtest` replays candidate settings on your own history before you change any.

## Per-resource settings

`KubeCostAutoscaler` and `KubeCostHPA` carry their own `dryRun`, bounds, stabilisation windows, failure policy and budget:
[CRD.md](CRD.md). A resource's `dryRun: true` holds even when the operator is not in observe-only. The annotation
`autoscaling.kubecost.io/pause=true` stops one autoscaler immediately.
