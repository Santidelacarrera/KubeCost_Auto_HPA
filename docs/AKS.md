# Azure Kubernetes Service (AKS)

**Status: validated partially. Pending validation for production.**

## What was run on a real AKS cluster

| What | Result | When / where |
|---|---|---|
| Read-only checks, recommend, approve, apply under real traffic, SLO observation, forced rollback | 14/15, twice. The failing check was "a node is removed", see below | AKS 1.36.5, free tier, 2 x Standard_D2as_v4, 2026-11-10 ([LAB.md](LAB.md) result 14) |
| Opt-in applier controller, all 8 scenarios in one run | 27/27 | 1 x Standard_D4as_v4 ([LAB.md](LAB.md) result 15) |
| Helm install, upgrade, rollback, uninstall | 4/4 | same |
| Node released by the AKS cluster autoscaler after the change | One of two nodes: 0.096 USD/h less **at list price**. Needed the autoscaler profile `skip-nodes-with-system-pods=false` | [LAB.md](LAB.md) result 17 |
| Read-only features of 2026-11-15/16: constrained node analysis with a real managed disk, manual plan, fleet report | 6/6 after one defect fixed (a disk in a region without zones was reported as immovable) | AKS 1.36.5, 2026-11-16 ([LAB.md](LAB.md) result 29) |
| OpenCost as the cost source for two hours | Fresh data throughout; live contract passed | [LAB.md](LAB.md) result 18 |
| Kubecost 2.8.7 free tier as the cost source | Stopped serving allocation data about 10 minutes after its first data, in every run. The client failed closed. Use OpenCost | [LAB.md](LAB.md) results 16, 18 |

## What was not run on AKS

* An Azure Cost Management **FOCUS export**. The adapter handles Azure's documented `ResourceId` shape and scale-set naming,
  tested with fixtures only. This is the main open item for AKS.
* Azure Monitor managed Prometheus as the metrics source.
* Any real Spot eviction.
* More than a few hours of continuous operation.
* A saving reconciled with an invoice.

## Permissions

In the cluster: the roles in [RBAC.md](RBAC.md). Nothing AKS-specific.

In Azure, **the product needs none**. It never calls an Azure API and needs no managed identity. For your own steps:

| Your step | Azure role | Used by |
|---|---|---|
| Get cluster credentials | `Azure Kubernetes Service Cluster User Role` | `az aks get-credentials` |
| Create a cost export | `Cost Management Contributor` on the scope | Cost Management, Exports, FOCUS dataset |
| Read the exported files | `Storage Blob Data Reader` on the storage account | Download the CSV |
| Change the autoscaler profile | `Azure Kubernetes Service Contributor Role` | `az aks update --cluster-autoscaler-profile ...` |

## Node operations: who does what

| Operation | Who does it |
|---|---|
| Say which nodes could be emptied, and why not | `rightsize nodes` (read-only) |
| Remove a node | The AKS cluster autoscaler. This product never deletes a node or scales a node pool |
| Let the autoscaler remove a node that holds system pods | You: `skip-nodes-with-system-pods=false` in the autoscaler profile. With the AKS default, a node with any non-DaemonSet `kube-system` pod is never removed, and on small clusters that is every node |
| Cordon and evict a Spot node being evicted | The operator's Spot drain, only with `observeOnly: false`. It reacts to the `kubernetes.azure.com/scalesetpreempted` taint. Never triggered by a real eviction in a test |
| Change VM size, node pool count or limits | You (`az aks nodepool ...`). Out of scope for the product |

## Cost data

1. Cost Management, Exports: create an export with the **FOCUS** dataset for the subscription or resource group, to a storage
   account.
2. Download the CSV for the period.
3. `rightsize cost --billing-format focus --billing-file <csv>`, and
   `rightsize reconcile --before <csv> --after <csv> --scope <node resource group>` ([FINOPS.md](FINOPS.md)).

AKS puts node VMs in a separate node resource group (`MC_<group>_<cluster>_<region>`); that name is the natural `--scope`.

The first real Azure export will be the first real test of the Azure side of the adapter. Expect to report a defect.

## Run the validation yourself

```bash
bash hack/aks-validate.sh
```

It needs `az login` with a subscription, **creates a cluster and a resource group, and deletes them at the end**. It costs
what two small VMs cost for about an hour. Read the script before running it; `KEEP=1` keeps the cluster.
