# FinOps: costs, savings and reconciliation

Detail of the allocation model and the billing adapters: [COST.md](COST.md). This page is the overview and the rule that
governs every number the product prints.

## The rule: four levels of saving

A saving is always printed with its level. A lower level is never reworded into a higher one.

| Level | What stands behind it | Where it comes from | What it is not |
|---|---|---|---|
| **1. Estimated** | A recommendation. Requests delta x a unit price you supplied x 730 h | Every recommendation (`PotentialMonthlyUSD`) | Nothing was changed. It assumes the freed requests are reclaimed |
| **2. Projected** | The change was applied and kept (verified, not rolled back) | `rightsize report`, metric `kubecost_recommendation_kept_estimated_monthly_usd` | No cost was measured. A node may not have been released |
| **3. Observed** | Cost measured in two equivalent windows, before and after, on one basis: allocated cost, or node-hours at list price | `rightsize cost --baseline-report`, the node-hours measurement of the validation scripts | Not an invoice. List price ignores discounts, credits and commitments |
| **4. Reconciled** | Two provider billing exports for equivalent periods, in one currency, show the invoiced (or amortised) usage fell | `rightsize reconcile` | It does not prove the cause: demand must have been equivalent in both periods |

Where the project stands today:

* Levels 1 and 2: produced routinely.
* Level 3: measured three times, on test workloads, all at list price: GKE 0.067 USD/h, AKS 0.096 USD/h
  ([LAB.md](LAB.md) results 12, 17), and the GKE soak, 2 nodes to 1 after five days of history (result 27).
* Level 4: **never achieved.** No saving has been confirmed against an invoice.

## What is reported

| Report | Command / metric | Basis |
|---|---|---|
| Cost by namespace, by workload; idle and unallocated | `rightsize cost` | Estimated (node price x time) or billed (billing file) |
| Cost per replica, per CPU core, per GiB | OpenCost allocation through the operator; unit prices are chart values | Estimated |
| Cost per request / per 1k requests | `kubecost_hpa_cost_per_request_usd`; `rightsize cost` with request counts | Estimated |
| Budgets per namespace, and status against them | `rightsize cost --budget ns=usd,...` | Estimated or billed |
| Deviation alerts between two periods | `rightsize cost --baseline-report <report> --deviation-pct` | Same basis in both |
| Waste: requested and unused capacity, stranded capacity on nodes that cannot be removed | Recommendations (requests vs usage), `rightsize nodes` | Requests |
| Before / after an optimisation | `rightsize cost --baseline-report` (observed), `rightsize reconcile` (bills) | Observed / reconciled |
| Acceptance, rollback ratio, kept reductions | `rightsize report`, `kubecost_recommendation_*` | Process metrics + level 2 |
| Spend trend and spend forecast | **Not implemented.** Graph `kubecost_autoscaler_hourly_cost_usd` in your own Prometheus | — |

## Reconciling with the bill

```bash
rightsize reconcile --before october.csv --after november.csv --scope <cluster or resource group name>
```

Both files are **FOCUS** exports as the provider delivers them ([GKE.md](GKE.md), [AKS.md](AKS.md)). The command reads two files
and contacts nothing. It prints the level, what is missing for the next level, and the reconciliation.

How each item of a bill is treated:

| Item | Treatment |
|---|---|
| **Currency** | Totals stay in the billing currency. Two bills in different currencies are **not compared**: no exchange rate is invented. A file that mixes currencies is refused |
| **Billing period** | Taken from the charge periods of the rows in scope. Everything is compared per day. Periods that overlap, that are in the wrong order, or whose lengths differ by more than 10% are **not compared** |
| **Tax** | `ChargeCategory = Tax` rows are totalled and shown apart, per day, before and after. Never part of the saving |
| **Discounts** | Negotiated and sustained-use discounts are inside `BilledCost`. The list-price difference (`ListCost`) is shown next to it, so you see how much of a usage reduction the discounts already covered |
| **Credits** | `Credit` rows are shown apart and excluded. When credits absorb the usage (an account on promotional credits), the invoice does not fall: the result is **observed** at list price, **not reconciled**, and the note says so |
| **Commitments** | A `Purchase` row in either period distorts `BilledCost`. The comparison switches to `EffectiveCost` (amortised) when both exports have it, and says so; otherwise it warns |
| **Shared costs** | Not split. Use `--scope` so the comparison covers the cluster's resources only; what the filter excluded is totalled as out of scope. Without a scope the note warns that unrelated resources move the result |
| **Unattributable** | Usage rows with no `ResourceId` are totalled. Above 10% of usage, the note says that share cannot be attributed to the cluster |

Flags: `--simulated` labels hand-made or generated files, which can then never reach "reconciled". `--applied=false` or
`--kept=false` records that the optimisation was not applied or not kept, which caps the level at "estimated".

Status: unit-tested, and run on two real consecutive billing days of a GKE cluster as an A/A control, where it found no saving,
as it must ([LAB.md](LAB.md) result 26). **No real before/after pair with an invoice that fell has been reconciled yet**: that is
the next FinOps milestone. `hack/gcp-focus-day.sh` writes one day of Google billing in the shape this command reads.

## Cost connectors

| Source | State | Notes |
|---|---|---|
| OpenCost allocation API | Verified live for 2 h on AKS and in the lab | The recommended live cost signal |
| Kubecost allocation API | Contract verified; Kubecost 2.8.7 free tier stalled in four runs | Not recommended as installed |
| Google Cloud FOCUS billing export (CSV from BigQuery) | Verified on one real day | Billing currency converted with Google's own rate column |
| Azure Cost Management FOCUS export (CSV) | Unit-tested against the documented shape | Pending a real export |
| Simple CSV (`resource,cost_usd,start,end`) | Unit-tested | For any other source you convert yourself |
| Direct calls to a provider's billing API | Not implemented | Deliberate: an export is auditable and needs no cloud credential in the cluster |

The seam for a new provider is `cost.Adapter` (`Billed(ctx, start, end)`), plus a parser to `BillingItem`. An adapter must
return real billing data or an error, never an invented number. AWS is out of scope for this phase.

## What not to claim

* A requests reduction is not a saving until a node is released.
* A list-price figure is not what you pay.
* A lower bill is not caused by the optimisation unless demand was equivalent. Compare request volume first; `rightsize cost`
  prints cost per request for that reason.
* The ROI calculator on the web page and `scripts/calculate_roi.py` are what-if models on numbers you type. They are not
  evidence.
