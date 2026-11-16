# Known limitations and operational risks

Per-feature status is in [STATUS.md](STATUS.md). This page is what to keep in mind before relying on the product.

## Not validated

1. **The recommender's defaults are untuned.** p95 + 15% for CPU and p99 + 20% for memory were never checked against weeks of
   real traffic. On a simulated week the memory default was tight (2.9% of unseen samples exceeded it). Run `rightsize
   backtest` on your own history before applying anything.
2. **No long run.** The longest continuous runs are hours, plus a 7-day GKE soak whose report is due 2026-11-17.
3. **No saving reconciled with an invoice.** Two observed savings exist, at list price, on test workloads.
4. **Automated mode** (no per-change approval) has not been exercised under real load.
5. **Several operator replicas during an API-server outage.** The outage test ran one replica for about 40 seconds.
6. **Azure billing export.** The adapter has only seen fixtures built from Azure's documented format.
7. **GKE:** the Helm chart and the applier controller were not run there. **Autopilot** is not supported.
8. **Spot drain** was never triggered by a real interruption.
9. **DaemonSet** rightsizing is unit-tested only.

## Not implemented

* Automatic node consolidation: nothing drains, cordons or deletes a node to save money ([NODES.md](NODES.md)).
* Multi-cluster management: one operator per cluster, no central control plane. `rightsize fleet` is a read-only report over
  your kubeconfig contexts. The console's cluster tabs are samples.
* A seasonal model: weekly patterns are detected as a caveat, not modelled. The growth forecast is a straight line.
* Spend forecasting.
* Billing and payment. Licence files exist and their enforcement is opt-in and unit-tested only ([LICENSE_KEYS.md](LICENSE_KEYS.md)).
* Direct provider billing APIs: billing data comes from exports you download.
* AWS / EKS: out of scope for this phase.
* A real-time console: the web console is a demo with a verified-status panel.

## Design limits

* **Requests are not savings.** Money is saved only when the cluster autoscaler releases a node.
* **Limits are never lowered.** Recommendations lower requests, and raise a limit only when the new request needs it.
* **One container at a time or one record for all**, but always one workload per recommendation. No cross-workload planning.
* **History comes from Prometheus.** With less than 24 hours or 200 samples the recommender abstains.
* **Per-pod sizing.** The recommender sizes one pod of the workload. If an HPA changes replicas, per-pod usage changes; a CPU
  change under a CPU-based HPA is refused by default.
* **In-place resize needs Kubernetes 1.33+** and a container whose `resizePolicy` is not `RestartContainer`.
* **Approvals are only as strong as your RBAC.** Whoever can update the status subresource can approve. Use signed approvals
  and the separation-of-duties admission policies for anything that matters.
* **The node analysis is a simulation on requests.** It ignores preferred affinities and topology spread constraints.

## Operational risks

| Risk | Likelihood / impact | Mitigation |
|---|---|---|
| A memory reduction causes OOM kills after the observation window ended | Possible with bursty memory; a restart loop | Memory decreases are vetoed after any OOM kill in the window; keep `minConfidence: High`, use a 7-day window, apply memory changes in maintenance windows, and prefer CPU-only changes first |
| A CPU reduction under a growing load causes throttling days later | Possible | The growth forecast raises the target (experimental); the throttling veto blocks further decreases; re-plan weekly |
| A change is applied on a workload managed by GitOps and reverted, repeatedly | Low: refused by default | Leave `allowGitOps: false`; use `rightsize propose` |
| Two controllers fight over replicas | Low: detected and refused | Never create an HPA and a `KubeCostAutoscaler` on one Deployment |
| A stale or wrong cost signal drives scaling | Low: stale or partial data freezes | Use OpenCost; watch `kubecost_autoscaler_metric_age_seconds` |
| A forged approval | Depends on your RBAC | `requireSignedApproval`, `enforceSeparationOfDuties`, ship the audit trail off the cluster |
| The audit queue overflows under a burst | Seen in a load test above 1024 queued events | Events are dropped and counted; alert on the dropped counter |
| An upgrade without the CRDs | Operator restarts until they are applied | Apply `crds/` first ([UPGRADE.md](UPGRADE.md)) |
| Wrong unit prices make estimates meaningless | Certain if left at defaults | Set `cpuPricePerCoreHour` and `memPricePerGiBHour`, or use a cost source |
| A Kustomize overlay replaces the operator's args and silently removes `--observe-only` | Possible | Check the rendered Deployment ([INSTALL.md](INSTALL.md)) |
| Sample data on the console read as real | Was likely before 2026-11-15 | The console and the engine's sample endpoints are now labelled; do not present them as results |

## Security posture

See [THREAT_MODEL.md](THREAT_MODEL.md) and [RBAC.md](RBAC.md). In short: no wildcard RBAC and no access to Secrets in the chart;
the operator can update workloads only where you grant it; no external network access is needed. There has been **no
third-party penetration test or audit**, and there are no compliance attestations.
