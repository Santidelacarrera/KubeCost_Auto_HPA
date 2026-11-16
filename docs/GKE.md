# Google Kubernetes Engine (GKE)

**Status: validated partially. Pending validation for production.**

## What was run on a real GKE cluster

| What | Result | When / where |
|---|---|---|
| Read-only checks (`hack/cloud-validate.sh`): provider detection, metric series, plan, backtest, node analysis, cost report | 9/9 | GKE 1.36.4, Standard, 2026-11-10 ([LAB.md](LAB.md) result 12) |
| Recommend, approve, apply with an SLO on real latency, observe, forced rollback | 11/11 after one defect was fixed (rollback gave up on write conflicts during a 6-replica rollout) | same run |
| Node released by the GKE cluster autoscaler after the change | 3 to 2 nodes: 0.067 USD/h less **at list price**, two equivalent 20-minute windows. An observed saving on a test workload, not an invoice | same run |
| Node matching on real rows of Google's FOCUS export | 2 nodes matched by `ResourceName` for one day | [LAB.md](LAB.md) result 22 |
| Seven-day soak with the FOCUS billing export | Running. Report due 2026-11-17 | [LAB.md](LAB.md) result 13 |

## What was not run on GKE

* The Helm chart and the opt-in applier controller (they were run on AKS and in the lab, not on GKE).
* GKE Autopilot. Autopilot sets and bills requests itself; do not use the rightsizing apply path there.
* Google Managed Prometheus as the metrics source.
* Any real Spot interruption.
* A saving reconciled with an invoice: the test account is on promotional credits, so its invoice is 0 and only `ListCost`
  carries information.
* More than two hours of continuous operation outside the soak.

## Permissions

In the cluster: the roles in [RBAC.md](RBAC.md). Nothing GKE-specific.

In Google Cloud, **the product needs none**. It never calls a Google Cloud API. You need, for your own steps:

| Your step | Google Cloud permission | Used by |
|---|---|---|
| Get cluster credentials | `container.clusters.get` (role `roles/container.clusterViewer`) | `gcloud container clusters get-credentials` |
| Export billing to BigQuery | Billing Account Administrator to enable the export; `roles/bigquery.dataViewer` to read it | You, in the console |
| Download the FOCUS export as CSV | `roles/bigquery.jobUser` + `dataViewer` on the dataset | `bq query --format=csv` |

Do not give the operator's service account any Google Cloud role. Workload Identity is not needed.

## Node operations: who does what

| Operation | Who does it |
|---|---|
| Say which nodes could be emptied, and why not | `rightsize nodes` (read-only) |
| Remove a node | The GKE cluster autoscaler, when requests fall. This product never deletes or resizes a node pool |
| Cordon and evict a Spot node that received a preemption notice | The operator's Spot drain, only with `observeOnly: false`. It reacts to the `cloud.google.com/impending-node-termination` taint. Never triggered by a real preemption in a test |
| Change node pool size, machine type or autoscaling limits | You (`gcloud container node-pools ...`). Out of scope for the product |

## Cost data

1. Enable the **FOCUS** billing export to BigQuery (Billing, Billing export). Google also offers the detailed export; the adapter
   reads the FOCUS one.
2. Export the period as CSV. Keep the columns as delivered.
3. `rightsize cost --billing-format focus --billing-file <csv>` allocates it to namespaces.
   `rightsize reconcile --before <csv> --after <csv> --scope <cluster name>` compares two periods ([FINOPS.md](FINOPS.md)).

Known facts from the real export: amounts come in the billing currency with Google's own conversion-rate column; `BilledCost`
is after credits (0 on a credited account); `ListCost` is before discounts and credits.

## Run the validation yourself

It creates nothing and deletes nothing; you create and delete the cluster.

```bash
CTX=<your gke context> NODE_HOURLY_USD=<list price of one node> bash hack/gke-validate.sh
```

It costs what the cluster costs while it runs. Keep the evidence file it writes.
