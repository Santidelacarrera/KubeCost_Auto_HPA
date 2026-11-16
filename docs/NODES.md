# Node optimisation

Lowering a container's requests saves nothing by itself. Money is saved when a node is released. This product **analyses**
which nodes could be released and **never releases one**: it does not drain, cordon or delete nodes for consolidation, and it
never changes a node pool. The cluster autoscaler of GKE or AKS removes a node once requests no longer need it.

The one exception is the Spot drain, which reacts to an interruption the provider has already announced (last section).

## The analysis

```bash
rightsize nodes --context <ctx> --node-hourly-usd 0.096
```

For each node it reports the requested share of CPU and memory, whether its pods would fit on the other nodes below a target
utilisation (default 85% of allocatable, on requests), where each pod would go, and the reason when the node cannot be emptied.

### What it takes into account

| Constraint | How |
|---|---|
| CPU and memory capacity | Requests against allocatable, first-fit decreasing, up to `--target-utilisation` |
| Earlier moves in the same run | A node that received pods from a removed node is fuller for the next candidate |
| DaemonSet pods | Ignored: they go with the node |
| Pods without a controller | Block the node: nothing would recreate them |
| `hostPath` volumes, hostname `nodeSelector` | Block the node |
| Persistent volumes | A pod with a zonal volume may only move inside that zone. A volume whose zone is unknown blocks the node |
| Taints and tolerations | A pod is not placed on a node whose `NoSchedule`/`NoExecute` taint it does not tolerate |
| `nodeSelector` | The receiving node must carry the labels |
| Cordoned nodes | Receive nothing |
| Required anti-affinity on the hostname | Two pods of the same group are not placed on one node |
| PodDisruptionBudget | A budget that allows no disruption **now** blocks the node; one with room is reported |
| Zones | The last node of a zone is kept when the cluster spans several zones |
| The last node | Never removable |
| Pending and pressure | Unschedulable pending pods and nodes under memory, disk or PID pressure are listed. With either, the report says not to remove nodes |

### What it does not evaluate

Required **node affinity** expressions and required **pod affinity** are not evaluated: a pod with either is treated as
immovable, which can only make the answer more conservative. **Preferred** affinities, **topology spread constraints** and
ephemeral-storage requests are ignored, which can make it optimistic. The report states this in its caveats.

Reading a volume's zone needs `list persistentvolumes` (in `deploy/rbac/rightsize.yaml`). Without it every pod with a claim is
treated as immovable.

The result is a **candidate**, priced with a number you supply. It is not a saving until the node is gone and the bill falls
([FINOPS.md](FINOPS.md)).

Verification: the requests-only analysis was compared with a real drain on a two-node cluster, in both the "fits" and "does
not fit" cases ([LAB.md](LAB.md) result 7). The scheduling constraints added on 2026-11-15 were then run on real GKE and AKS
clusters, which found two defects on the first run, both fixed with a test: static pods owned by their Node (kube-proxy on
GKE) blocked every node, and the zone of a GKE persistent disk was not read because its CSI driver uses its own key
([LAB.md](LAB.md) result 26). The constrained analysis has not been compared with a real drain.

## The manual plan

```bash
rightsize nodes --context <ctx> --plan
```

adds, for each candidate node in order, the preconditions, the two `kubectl` commands (cordon, drain), what to verify before
the next node and how to undo. It is a proposal for a person: nothing in the product executes it. It is empty, with the
reason, while pods are pending as unschedulable or a node reports pressure. Re-run it between nodes: every drain changes
where pods are. Why the product stops here: [adr/0004-node-consolidation-is-a-plan-not-an-action.md](adr/0004-node-consolidation-is-a-plan-not-an-action.md).

## Several clusters

```bash
rightsize fleet --contexts gke-prod,aks-prod
```

prints one read-only report over the clusters you name: Kubernetes version, provider, nodes, zones, requested capacity,
node candidates, pending pods, recommendation phases, and totals over the reachable ones. An unreachable cluster is
reported, not fatal. It reads through your own kubeconfig contexts; nothing is installed and no cluster talks to another.

## Who removes a node

| Provider | Mechanism | Notes |
|---|---|---|
| GKE | Cluster autoscaler of the node pool | Observed: 3 to 2 nodes after a rightsizing change ([GKE.md](GKE.md)) |
| AKS | Cluster autoscaler | Needs `skip-nodes-with-system-pods=false` on small clusters ([AKS.md](AKS.md)) |
| Any other cluster | Whatever autoscaler you run, or you | Not validated |

## Why there is no automatic consolidation

Draining a node moves every workload on it. Doing that safely needs disruption budgets, scheduling constraints, storage and
zone availability to be right for every pod, and a way back when a pod does not come up elsewhere. The analysis above is the
first half. The second half would be a controller with its own policy, approval and rollback, in the same shape as the
rightsizing applier. It is not built, and it will not be added as a side effect of another feature.

The `KubeCostAutoscaler.spec.nodeBinPacking` field, which did nothing, was removed in v0.2.0.

## Spot interruption drain

With `operator.enableSpotDrain: true` and observe-only **off**, the operator cordons a Spot node that carries an interruption
taint and evicts its pods through the Eviction API, which honours PodDisruptionBudgets.

* Signals: `cloud.google.com/impending-node-termination` (GKE), `kubernetes.azure.com/scalesetpreempted` (AKS), or the
  annotation `autoscaling.kubecost.io/spot-interruption=true` set by your own tooling.
* It refuses when no other node is ready or when a pod has no controller, and retries evictions blocked by a budget until
  `--drain-timeout` (default 2 minutes).
* It only reacts to a node that is already being taken away. It never chooses a node to remove.
* Tested against a real API server (envtest) with a PodDisruptionBudget. **Never triggered by a real interruption.**
  Experimental.
