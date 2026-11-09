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

## What is not done

* No provider adapters (CUR/FOCUS/GCP/Azure exports must be reduced to the CSV columns `resource,cost_usd,start,end` by the user).
* No discounts, reservations, savings plans, spot price history or amortisation; credits and refunds must be handled before export (negative lines are rejected).
* No usage-based or GPU/storage/network allocation; no per-pod or per-controller breakdown (namespace only).
* Not validated against a real invoice. Any statement about accuracy would be invented.
