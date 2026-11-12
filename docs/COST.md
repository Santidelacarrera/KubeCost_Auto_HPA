# Cost methodology

Status: `internal/cost` and `internal/capacity` are **IMPLEMENTED BUT NOT VERIFIED against a real bill**. They are unit-tested with synthetic
fixtures (clearly labelled in the tests). No cloud billing API is called anywhere in this repository and no price is fetched.

## Three kinds of cost, never mixed

| Kind | Meaning | Where it comes from |
|---|---|---|
| **Estimated** | a model: unit price x quantity | prices someone supplies (flags, a price list). `cost.EstimateNodeCost` |
| **Billed** | real money | a billing source through the `cost.Adapter` interface (today: a provider-neutral CSV, `cost.FileAdapter`) |
| **Allocated** | a billed or estimated node cost split across namespaces by a rule | `cost.Allocate`; the report states its basis |

Every `Report` carries its `Basis` (`billed` or `estimated`) and `Source`. A report built from estimates must never be presented as a bill.

## Allocation rule (v1)

1. Node cost for the window (billed item prorated by overlap, or hourly price x hours).
2. Each pod's share of its node = `w x cpuRequest/cpuAllocatable + (1-w) x memRequest/memAllocatable`, with `w = 0.5` by default (a convention, configurable, echoed in the report).
3. Sum of pod shares per namespace; the unrequested remainder of every node is **`__idle__`**.
4. **Shared** namespaces (for example `kube-system`) are removed and redistributed over the other namespaces in proportion to their cost; if none is left they stay in `__shared__`.
5. Billed items that match no node (load balancers, disks, a node with no capacity data) are **`__unallocated__`**, never hidden in a namespace.

Requests above allocatable are scaled down so shares never exceed 100%. Usage is not used for allocation (requests are what the scheduler reserves).

## Reconciliation

`Total` (all billed/estimated input) is compared with `Accounted` (sum of every bucket). `Reconciled` is true when `|Total - Accounted| / Total` is within the tolerance (default 1%).
By construction the difference is only rounding; the check exists to catch bugs and missing data. It does **not** prove the allocation is "fair", only that nothing was lost or invented.

## Savings: three different numbers

* **Potential** (`rightsizing recommendation`): requests delta x a unit price x 730 h. An estimate that assumes the freed requests are reclaimed.
* **Observed** (`cost.ObservedSavings`): change in the allocated cost per hour of a namespace between two equivalent windows, normalised by hours.
  If request volumes are given it also reports cost per request and warns when traffic fell by more than 20%.
  It is **allocated** cost: it is not an infrastructure saving unless a node was actually released.
* **Billed**: only when both reports are `billed`. It is the change in the namespace share of billed node cost; no per-namespace invoice exists.
  Mixed bases refuse to compare.

## Resource efficiency vs capacity vs cost reduction

`internal/capacity` checks whether the pods of a node fit on the other nodes (first-fit decreasing on requests, receiving nodes capped at 85% requested).
Lowering requests is *resource efficiency*; fitting the same pods on fewer nodes is *capacity optimisation*; only releasing a node is *infrastructure cost reduction*.
The tool reports removable nodes as **candidates** with their estimated hourly price, plus stranded (fragmented) capacity. It ignores pod affinity/anti-affinity, topology
spread, taints and volume zones, and says so in the report. It never drains or removes anything.

## Budgets and deviation alerts

`rightsize cost --budget shop=10,dev=2` compares each namespace's allocated cost for the window with its budget and flags `over`, always carrying the report basis: an alert on **estimated** cost is a warning, not an invoice.
`--baseline-report earlier.json --deviation-pct 20` lists namespaces whose cost **per hour** moved more than the threshold between two reports (windows of different length compare; mixed bases refuse).
Both are unit-tested and ran on the lab with an estimated basis; nothing was compared with a real bill.

## FOCUS billing exports

`rightsize cost --billing-file export.csv --billing-format focus [--focus-cost-column BilledCost|EffectiveCost]` reads a FOCUS CSV **as the provider delivers it**:

* Columns: `ResourceId`, the cost column, `ChargePeriodStart`, `ChargePeriodEnd`, and if present `ResourceName`, `BillingCurrency`, `x_CurrencyConversionRate`, `ChargeCategory`.
* Only `Usage` rows become cost; credits, taxes, purchases and adjustments are listed as skipped in the notes. Google puts credits inside `x_Credits` on the Usage row, so its `BilledCost` is before credits (stated in the notes).
* **Currency**: USD rows are read as is. A row billed in another currency is converted with **the export's own per-row `x_CurrencyConversionRate`** (Google: billing-currency units per USD), exactly (rational arithmetic); without that column a non-USD row is refused. No exchange rate is ever invented. The notes say how many rows were converted and that the invoice is in the billing currency.
* **Timestamps**: RFC3339 (Azure writes seven fractional digits), BigQuery's CSV form `2026-10-30 17:00:00` (optionally ` UTC`), zone-less ISO and bare dates, all UTC. `11/01/2026` is refused (month/day is ambiguous).
* **Matching to nodes**: by the last segment of `ResourceId` (AWS instance id, GCE name, Azure VM name), then of `ResourceName` (Google's FOCUS export has a numeric instance id in `ResourceId` and the instance name in `ResourceName`), then **Azure VM scale sets**: AKS node pools are billed per scale set, so an item whose `ResourceId` is a scale set is split across that scale set's current nodes by allocatable CPU (the parts sum exactly). Nodes removed during the period are not in that split; the total is kept. A bare instance number (`/virtualMachines/0`) is never a match key. The rest is `__unallocated__`.

Checked against a **real** export: Google's native FOCUS export in BigQuery, written by `bq --format=csv` (2026-11-11, an account billed in CLP; fixture `operator/internal/cost/testdata/gcp-native-focus-raw.csv`, project numbers replaced). That export held no GKE usage rows yet (the billing export had no data after 2026-10-31), so node matching on real Google rows is still unverified. Azure and AWS: the column names and Azure's documented `ResourceId` shape only, not a real export.

## What is not done

* No provider API calls: a FOCUS CSV (or the simple `resource,cost_usd,start,end` CSV) is read; Parquet must be converted to CSV first.
* No discounts, reservations, savings plans, spot price history or amortisation; credits and refunds must be handled before export (negative lines are rejected).
* No usage-based or GPU/storage/network allocation; no per-pod or per-controller breakdown (namespace only).
* Not validated against a real invoice. Any statement about accuracy would be invented.
