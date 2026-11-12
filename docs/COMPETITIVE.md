# Competitive context (sources fetched 2026-11-09)

Each statement about another product is a paraphrase of the page named next to it, read on the date above. Marketing pages describe intent, not verified behaviour:
nothing here was tested against those products, and no parity is claimed. Pages not listed were not read.

## What the sources say

| Product | Source | What the page says (paraphrased) |
|---|---|---|
| StormForge | https://docs.stormforge.io/docs/recommendations/ | Recommendations are point-in-time suggestions for CPU/memory requests and limits, HPA target utilisation, Java heap and node affinity; machine learning produces three candidates (savings, reliability, balanced - the default). During a default 7-day learning period recommendations can be applied on demand but not auto-deployed. The Applier uses server-side apply and mutating webhooks, staggers rollouts, watches the result for 5 minutes by default, and on an unhealthy workload (for example CrashLoopBackOff) rolls back and marks the recommendation failed; rollback can be disabled; drift reconciliation is optional. |
| Cast AI | https://docs.cast.ai/docs/getting-started | Describes an all-in-one platform for Kubernetes automation, optimisation, security and cost management: cost monitoring reports, autoscaling, Spot instance automation and bin packing, container right-sizing from actual usage, connection through a CLI, console or Terraform. The page does not describe a read-only phase. |
| Kubecost | https://www.kubecost.com/ redirects to https://www.apptio.com/?src=kc-com | Now presented as IBM Kubecost (Apptio): spend visibility by cluster, team, namespace and workload, reconciliation with cloud bills, flagging of over-provisioned workloads, automation such as request sizing and namespace turn-down, a free tier and paid tiers; the page claims typical customers save 30-50% (a vendor claim, not verified here). |
| Harness | https://www.harness.io/blog/kubernetes-cost-optimization | A general article: right-size requests from several weeks of usage, use HPA/VPA/Cluster Autoscaler or Karpenter, put fault-tolerant work on spot capacity, and allocate cost per namespace with consistent labels; common leaks are over-provisioned requests, idle non-production, orphaned volumes, unallocated capacity. |

## This repository against those descriptions

| Capability | State here (verified level) |
|---|---|
| CPU/memory request recommendations | Implemented; one candidate (no savings/reliability/balanced variants); untuned defaults; lab-verified once (docs/LAB.md) |
| Learning-period gate before applying | Implemented as abstention rules (minimum samples, window, staleness); production defaults 24 h / 200 samples, not 7 days |
| Apply, watch, roll back on unhealthy workload | Implemented and lab-verified: restart, OOM (unit-tested), availability, replica drift, SLO query, rollout timeout; default observation 5 minutes (same order as the StormForge default quoted above, by coincidence of design, not by comparison testing) |
| Approval step | `rightsize approve` stamps the exact ID and the approver identity reported by the API server; the policy can restrict approvers; no UI |
| Cost allocation and bill reconciliation | Library with synthetic fixtures; no real billing; no UI |
| Spot automation, cluster autoscaling, bin-packing execution | Not provided (Spot drain exists in the legacy controller; bin-packing is a candidate report only) |
| Multi-cluster / fleet / SaaS control plane | Not provided |
| GitOps | Detection and refusal, and `rightsize propose` (a commit on a new local branch; docs/GITOPS.md); no pull-request integration |

## Where this design is different (and how far that has been shown)

* Every recommendation carries its formula, inputs and abstention reasons, and a recommendation is refused rather than guessed when data is missing (unit-tested; one real-Prometheus run).
* Policy is a separate fail-closed component with a human approval of the exact recommendation ID (lab-verified denials).
* Rollback was exercised on a real cluster, and the audit trail records actor, previous values and reasons.
* Estimated, billed and allocated cost are kept apart in the types.

These are design properties, not market advantages: no customer, benchmark or cost-saving result supports them. The honest gap list is docs/ROADMAP.md.
