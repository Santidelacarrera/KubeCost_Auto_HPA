# Validating on EKS, GKE and AKS, and with real billing (a runbook for YOU to run)

**Status: NOT RUN.** I (the assistant) cannot create cloud clusters or read billing data: that needs your cloud account and credentials, and entering credentials is something I will not do.
Everything below is a procedure plus a read-only script (`hack/cloud-validate.sh`, tested against the local lab only). Cloud CLI commands are *templates I did not execute*; check each against the
provider's current documentation before running it. Nothing here can be declared validated until you run it and keep the evidence file.

## 0. Ground rules

* Use a **disposable or staging** cluster and a namespace you own. Do not point this at production first.
* The validation script is **read-only**. Applying a change is a separate step you take with `rightsize apply --execute --context <explicit>` (docs/SAFETY.md), on a non-critical workload, with a policy file.
* Budget the cost of the test cluster and tear it down. Keep the evidence file (`.cloud-evidence/<ctx>-<time>.md`); review it before sharing (it has workload and node names).

## 1. What the cluster needs

| Need | Why | Check |
|---|---|---|
| Prometheus scraping cAdvisor (kubelet) and kube-state-metrics, 7+ days of history for real conclusions | the recommender reads history | the script checks the series exist and counts samples |
| `metrics.k8s.io` (metrics-server) | the KubeCostAutoscaler demand source | `kubectl top pods` |
| A workload with real traffic and a latency/error metric | to compare equivalent periods and drive an SLO query | your own app metrics |
| A user/role with `deploy/rbac/rightsize.yaml` (read, and apply per namespace only if you will change something) | least privilege | `kubectl auth can-i` |

A common way to get Prometheus + kube-state-metrics is `kube-prometheus-stack` or `prometheus-community/prometheus` (the lab used the latter, docs/COMPATIBILITY.md). Managed Prometheus offerings expose the same
`container_*` and `kube_*` series, but the query endpoint and authentication differ: pass `--prometheus-bearer-token-file` / `--prometheus-ca-file` or `PROMETHEUS_TOKEN` if yours needs them.

## 2. The read-only run (any cluster)

```bash
kubectl port-forward -n monitoring svc/<your-prometheus> 9090:80 &      # or point PROM at your endpoint
CTX=<context> CONFIRM_CTX=<context> NS=<namespace> WORKLOAD=<name> CONTAINER=<container> NODE_HOURLY_USD=<price> bash hack/cloud-validate.sh
```

It reports: cluster and cloud detected (from `spec.providerID`), whether the series exist, a `plan` with the **production defaults** (7-day window, 24 h minimum, 200 samples), a `backtest`, the node
candidate analysis and a cost report. `PASS` lines are prerequisites met; the recommendation itself is a *proposal*, not a verdict.

## 3. Real billing: use a FOCUS export

`rightsize cost --billing-format focus` reads a **FOCUS** (FinOps Open Cost and Usage Specification) CSV. The three providers document a way to produce one (sources read 2026-11-09; confirm the current state in each console):

| Provider | What the sources say | Source |
|---|---|---|
| AWS | Data Exports for FOCUS 1.0 is generally available (since November 2024); exports go to S3 daily as Parquet or CSV; created in the Billing and Cost Management console (Data Exports, standard export, "FOCUS 1.0 with AWS columns"); the first delivery can take up to 24 hours. A newer FOCUS 1.2 table also exists. | [announcement](https://aws.amazon.com/about-aws/whats-new/2024/11/aws-billing-cost-management-data-exports-focus-1-0), [creating a standard export](https://docs.aws.amazon.com/cur/latest/userguide/dataexports-create-standard.html) |
| Azure | Cost Management exports can produce a FOCUS dataset (actual and amortised in one dataset); in the portal: Cost Management + Billing, Exports, create, template "Cost and usage (FOCUS)". Some portal screens still label it preview. | [FOCUS on Azure](https://focus.finops.org/get-started/microsoft/), [exports tutorial](https://learn.microsoft.com/azure/cost-management-billing/costs/tutorial-export-acm-data) |
| Google Cloud | A BigQuery view based on FOCUS (needs Detailed Billing Export and Price Exports enabled); the release notes also list a native FOCUS export to BigQuery in preview. You export the result to CSV yourself. | [BigQuery view](https://cloud.google.com/blog/topics/cost-management/new-bigquery-cloud-billing-view-based-on-focus), [release notes](https://docs.cloud.google.com/billing/docs/release-notes) |

Things to know before trusting a number:

* **Format:** the parser reads CSV. AWS can deliver Parquet: convert it to CSV first (for example with DuckDB or pandas). Use a FOCUS version whose column names match: `ResourceId`, `BilledCost`, `ChargePeriodStart`, `ChargePeriodEnd`, `ChargeCategory`, `BillingCurrency`.
* **Currency:** only USD. A different `BillingCurrency` is rejected (no FX is invented).
* **What is allocated:** only `Usage` rows. Credits, taxes, purchases and adjustments are listed as *skipped* in the report notes, never silently dropped and never spread over namespaces.
* **Mapping to nodes:** by the last segment of the billed `ResourceId` against the node name or the last segment of `spec.providerID` (`aws:///zone/i-...`, `gce://project/zone/name`, Azure VM or VMSS instance path).
  Anything unmatched (load balancers, disks, a node you did not list, data transfer) is **`__unallocated__`**. A large unallocated share means the namespace totals are *not* the whole bill.
* **Not validated:** the parser was tested with hand-written fixtures that follow the specification's column names, not with a real export. Your first real file is the real test: keep the report, and compare its total with the invoice by hand.
* **Reconciliation** only proves nothing was lost or invented inside the allocation; it does not prove the allocation is fair (docs/COST.md).

## 4. Creating a small test cluster (templates, not executed)

Pick the smallest managed cluster that gives two or three nodes, and delete it afterwards. Examples of the shape of the commands:

```bash
# EKS (eksctl)         eksctl create cluster --name rs-test --nodes 2 --node-type t3.medium      ...   eksctl delete cluster --name rs-test
# GKE                  gcloud container clusters create rs-test --num-nodes 2 --machine-type e2-medium ... gcloud container clusters delete rs-test
# AKS                  az group create -n rs-test -l <region>; az aks create -g rs-test -n rs-test --node-count 2 ... az group delete -n rs-test
```

Then install Prometheus (section 1), deploy a real workload with traffic, wait for history, run the read-only script, and only then decide whether to try one `rightsize apply` in a staging namespace.
The lab scripts (`hack/lab-e2e.sh`, `lab-gitops.sh`, `lab-applier.sh`, ...) are written for the minikube lab and refuse other contexts unless `ALLOW_CTX=1`; adapting them to a cloud cluster is possible but is not done or tested.

## 5. What counts as "validated on a managed cluster"

All of these, with the evidence file kept: the series exist with at least 7 days of history; `plan` does not abstain for a workload with real traffic; its explanation matches what you know about the workload;
a `backtest` on that workload keeps the exceed fraction under your target with the settings you will use; for one staging workload an approved apply stays healthy through a full business cycle (not 5 minutes), or is rolled back;
the cost report's billed total is within your tolerance of the invoice for the same period. Until then, state it as **not validated on EKS/GKE/AKS**.

**Status (2026-11-10):** a two-hour run on a real GKE 1.36.4 cluster passed the mechanics (`hack/gke-validate.sh`, docs/LAB.md result 12: read-only checks, apply, rollback after a fix, a node removed by the autoscaler). It does **not** meet this section: no 7 days of history, no business cycle, no invoice. AKS 1.36.5 was run twice (docs/LAB.md result 14): mechanics pass, no node saving measured. EKS was not run. A seven-day GKE soak aimed at this section started 2026-11-10 (`hack/gke-soak.sh`, docs/LAB.md result 13, synthetic traffic, billing exports enabled); it has no result until its day-7 report.
