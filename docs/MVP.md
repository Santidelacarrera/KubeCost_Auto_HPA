# The path to an MVP

Written 2026-11-16. Feature status is in [STATUS.md](STATUS.md); this page answers one question: what is the smallest thing a
first customer could use and pay attention to, and how far is it.

## The MVP

**An observe-only rightsizing and cost report for GKE and AKS, with an assisted apply for teams that want it.**

A platform team installs one Helm chart, changes nothing in their cluster, and after a week gets: which workloads ask for more
than they use and by how much, why each recommendation says what it says, which nodes could go, and what that is worth at
their prices. If they trust it, they approve changes one at a time and the product applies them, watches them and rolls back.

This is narrower than the earlier pitch on purpose. It leaves out automatic scaling of replicas, predictive scaling, Spot
handling, node consolidation and any savings guarantee: none of those is needed for a first customer to get value, and each
needs evidence that does not exist yet.

## What the MVP needs, and where each item stands

| # | Need | State |
|---|---|---|
| 1 | Install with one command, safe by default | **Done.** Helm chart and Kustomize base start in observe-only; verified on the lab and AKS |
| 2 | Recommendations a person can check | **Done.** Formula, statistics, vetoes, risks, insights in every record |
| 3 | Run on GKE and AKS | **Done for the CLI path** on both. Chart and applier: AKS and lab only, **not GKE** |
| 4 | Apply with approval and automatic rollback | **Done.** Exercised on GKE and AKS. On 2026-11-16 a change planned on five days of real history was applied and kept on GKE, and the autoscaler released one of two nodes |
| 5 | A cost view the customer can reconcile | **Partly.** Allocation works against OpenCost; reconciliation works on real Google billing days; **no real before/after pair, no Azure export** |
| 6 | A view over several clusters | **Done, minimal.** `rightsize fleet` (read-only report) |
| 7 | Something a person can read | **Done, minimal.** `rightsize render` turns the outputs of `plan`, `nodes`, `fleet` and `reconcile` into one Markdown report (an example from the real GKE cluster is in `docs/reports/evidence/2026-11-16-rendered-report.md`). The web console is still a demo with sample data |
| 8 | Defaults a customer can trust | **Not done.** Never checked on weeks of real traffic. This is the item that most limits the MVP |
| 9 | A way to tell a paying tier from a free one | **Done, untested in a cluster.** Signed licence files, opt-in enforcement |
| 10 | Published release | **Not done.** v0.1.0 is published and old; everything since is unreleased |

## What is left, in the order to do it

Things the project can do alone:

1. **Publish a release** (v0.2.0) with everything since v0.1.0, after the dry-run and the Helm rehearsal. Needs the owner's
   go-ahead because it publishes images and a chart.
2. ~~A report people can read~~ **done on 2026-11-16**: `rightsize render`. Next: one command that runs `plan` for every
   workload of a namespace and renders the result, so the weekly report is one line.
3. **The chart and the applier on GKE**, after the soak ends, so both providers have the same coverage.
4. **Run the licence enforcement in a cluster** (lab), and the benchmark harness on GKE or AKS.
5. **Remove the dead weight**: the unused Go packages, the `features.*` values, the three ignored CRD fields, the sample
   endpoints of the engine. An MVP should not carry code nobody runs.

Things that need someone else:

6. **One design partner with a real cluster.** Two weeks in observe-only gives the real history that items 8 and 5 need:
   backtest the defaults, validate the growth forecast, and get a first before/after billing pair. Nothing substitutes for
   this, and it is the single most valuable next step.
7. **An account that is not on credits**, or a partner's export, to reconcile one saving with an invoice.
8. **An Azure Cost Management FOCUS export**, to validate the Azure adapter.
9. **Vendor accounts** (CAST AI, ScaleOps) if a competitive benchmark is wanted. It is not needed for the MVP.

## What the MVP must not promise

A savings percentage, a guarantee, parity with any other product, AWS, unattended automation, or compliance attestations.
What it can promise is in the last paragraph of [COMMERCIAL.md](COMMERCIAL.md).

## Maturity after the MVP list

Items 1 to 5 keep the product at Beta with a usable MVP. Items 6 to 8 are what turn it into a Release Candidate.
