# Rollback

Every change this product makes records what was there before, on the object it changed. Rolling back does not depend on the
operator, a database or the audit log being available.

## What can be rolled back, and how

| What changed | Automatic rollback | Manual rollback |
|---|---|---|
| A workload's requests, by `rightsize apply` or the applier | During the observation window, on: container restarts, OOM kills, lost availability, fewer ready replicas, the SLO query firing, rollout timeout | `rightsize rollback --context <ctx> --namespace <ns> --workload <name>` |
| A recommendation that was approved but not yet applied | — | Remove the approval, or set `killSwitch: true` in the policy |
| Pod resources resized in place by a `KubeCostHPA` | The controller converges back when the SLO is at risk | Delete the `KubeCostHPA`: its finalizer returns the pods to the template's resources. Or set `dryRun: true` |
| Replicas set by a `KubeCostAutoscaler` | Stabilisation and safety scale-up | `autoscaling.kubecost.io/pause=true`, then `kubectl scale` |
| A change proposed through GitOps | Your GitOps controller | Revert the commit or close the pull request |
| The chart | — | `helm rollback` ([UPGRADE.md](UPGRADE.md)) |

## Automatic rollback of a rightsizing change

After an apply, the executor watches the workload for the observation window (CLI flag, or `applier-max-observe`, 15 minutes by
default in the operator). It rolls back when any of these is true:

* a container of the workload restarted, or was OOM-killed;
* available replicas fell below what they were;
* the rollout did not finish in time;
* the SLO query you named returns a breach.

The rollback restores the recorded requests and limits, then **verifies** that the object carries them. A rollback that
cannot be verified is reported as failed, not as done. Write conflicts during a rollout are retried: giving up on them left a
degraded change in place once, on GKE, and that defect is fixed ([LAB.md](LAB.md) result 12).

Time the operator cannot see the cluster (an API-server outage) is not counted against the rollout timeout.

Exercised for real: restart trigger, SLO trigger on a synthetic signal and on a real latency histogram, rollback after
`kill -9` of the tool, on the lab, GKE and AKS. The OOM trigger is unit-tested only.

## Manual rollback

```bash
rightsize rollback --context <ctx> --namespace <namespace> --workload <name> --kind Deployment
```

It restores the recorded state and waits for the rollout (`--wait`, 5 minutes by default). There is no dry-run for a rollback:
naming the context explicitly is the confirmation. It works after the tool or the operator was killed mid-change, because the
previous state is an annotation on the workload.

For a multi-container recommendation, all containers of the record are rolled back together.

## Stop everything

| Scope | How | Takes effect |
|---|---|---|
| All applies | `killSwitch: true` in the policy ConfigMap | Next evaluation, no restart |
| All writes of the operator | `helm upgrade --set operator.observeOnly=true` | After the rollout |
| One autoscaler | `kubectl annotate kubecostautoscaler <name> autoscaling.kubecost.io/pause=true` | Immediately |
| One workload, permanently | The opt-out label, or `denyLabels` / `denyWorkloads` in the policy | Next evaluation |

Stopping does not undo. Changes already kept stay until you roll them back.

## Limits

* A rollback restores requests and limits. It does not restore replicas that an HPA changed in the meantime.
* If someone edits the workload's resources between the apply and the rollback, the rollback restores the recorded values over
  that edit. The audit record shows both.
* After `helm uninstall`, the recorded previous state remains on the workloads; `rightsize rollback` still works.
* Node-level actions have no rollback because the product takes none, except the Spot drain, which cannot be undone: the node
  is going away.
