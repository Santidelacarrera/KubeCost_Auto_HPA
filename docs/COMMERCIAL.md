# Commercial model

**Status: a design. Nothing here is sold today.** The product is open source under Apache-2.0 and at Beta maturity
([STATUS.md](STATUS.md)). No billing, payment or licence enforcement is implemented. Prices are indicative and have not been
tested with a customer. This page replaces the earlier commercial offer, which promised support levels, guarantees and
compliance packages that do not exist.

## Editions

Three subscription tiers, mapped to the three operating modes. The mapping is the point: a customer moves up a tier when they
are ready to let the product do more, and each step has its own safety evidence.

| | **Starter** | **Professional** | **Enterprise** |
|---|---|---|---|
| Operating mode | Observe-only | Assisted (human approval) | Automated inside authorised policies |
| Metrics and recommendations | Yes | Yes | Yes |
| Explained recommendations with evidence | Yes | Yes | Yes |
| Basic reports (estimated and projected savings, node candidates) | Yes | Yes | Yes |
| Apply with approval, automatic rollback | — | Yes | Yes |
| Budgets and deviation alerts | — | Yes | Yes |
| Change history, outcome metrics | — | Yes | Yes |
| FinOps integrations (billing exports, reconciliation) | — | Yes | Yes |
| GitOps proposals (pull requests) | — | Yes | Yes |
| Policy-authorised automation without per-change approval | — | — | Yes |
| Advanced policies (per namespace, windows, signed approvals, separation of duties) | — | — | Yes |
| Audit trail shipped to immutable storage | — | — | Yes |
| Multi-cluster management | — | — | Planned: **not built** |
| Advanced reports (reconciled savings by team) | — | — | Planned: **not built** |
| Enterprise support | — | — | Planned: **not offered** |

What exists in the code today, regardless of tier: everything except the three rows marked planned. Features that are
experimental today (automated mode, the forecast, predictive scaling) would not be sold as generally available until they
leave that state.

## What to charge for

| Model | How it works | For | Against | Verdict |
|---|---|---|---|---|
| **Per managed cluster** | Flat fee per cluster per month | Simple to quote and to meter (one operator = one cluster) | A 5-node and a 500-node cluster pay the same | Good for Starter |
| **Per node** | Fee per node per month, averaged over the month | Tracks value roughly; the buyer already counts nodes; the common model in this market | Penalises the customer for the node count the product is meant to reduce, unless billed on a baseline | **Primary model** for Professional and Enterprise |
| **Share of managed cloud spend** | A percentage of the cluster's compute bill | Scales with the customer | Needs trusted billing data; the fee falls when the product works | Only as a cap or floor |
| **Share of savings** | A percentage of savings | No upfront risk for the buyer | Requires **reconciled** savings ([FINOPS.md](FINOPS.md) level 4), which this project has never produced. Disputes over the baseline are the norm | **Not offered** until reconciliation is routine |
| **Custom enterprise plan** | Annual contract: committed node volume, support terms | Fits procurement | Needs a support organisation | Later |

Indicative list prices, to be validated with design partners, not commitments:

| Tier | Indicative price | Metering |
|---|---|---|
| Starter | Free for one cluster (it is the open-source observe-only mode); a small flat fee per additional cluster | Clusters |
| Professional | Per node per month | Monthly average node count |
| Enterprise | Per node per month, higher, with an annual minimum | Monthly average node count, committed volume |

No figure is given on purpose. The earlier documents quoted 18 and 32 USD per node per month and a "savings guaranteed or
refunded" clause. Those numbers were not derived from any cost or customer data, and a guarantee cannot be offered on savings
that have never been reconciled with an invoice.

## Entitlement and billing architecture (not implemented)

The design that the code is prepared for. None of it is built.

```
            (vendor side, not built)                         (customer cluster, exists)
  +-------------------------------+
  | Billing system                |   signed licence file     +-------------------------------+
  | (invoices, payment provider)  | -----------------------> | Operator                       |
  +-------------------------------+   offline, no call home  |  entitlement check at startup  |
            ^                                                |  tier -> allowed mode          |
            | usage report (opt-in, customer sends it)       |  node count from the API       |
            +----------------------------------------------- |  (already known locally)       |
                                                             +-------------------------------+
```

Principles:

1. **Licence as a signed file, verified offline.** A Kubernetes Secret holding a document signed with the vendor's Ed25519 key:
   customer, tier, node limit, expiry. The operator verifies the signature with a public key built into the binary. The
   signing and verification code for approvals (`internal/approval`) is the model. No call home: the product keeps working in
   an isolated network.
2. **The tier gates the mode, nothing else.** The check sits where the mode is decided today: `--observe-only`,
   `--enable-recommendation-applier` and the policy's `requireApproval`. Without a licence the product is Starter. A licence
   never weakens a safety control.
3. **Fail safe.** An expired or invalid licence drops the operator to observe-only. It never stops measuring, never rolls
   back what is running, and never blocks a rollback.
4. **Metering from what the cluster already knows.** Node count comes from the Kubernetes API. Usage reports, if a contract
   needs them, are a file the customer exports and sends, not telemetry.
5. **Billing lives outside the product.** Invoices and payment are a vendor-side system fed by the licence registry and the
   usage reports. The operator and the engine never handle payment data.

What is in the repository today, and what it is:

| Item | What it really is |
|---|---|
| `scripts/setup_stripe_billing.py` | A script that would create products and prices in a Stripe account. Only its dry-run has been run. No Stripe account is connected |
| `web/stripe-config.json` | Output of that dry-run: `environment: simulation`, mock identifiers. Not used by the product |
| Engine `GET /v1/billing/invoice` | A pricing preview from a savings figure the caller supplies. Marked `simulation: true`, `charge_created: false` |
| Engine `POST /v1/billing/stripe-webhook` | Verifies a signature and records an event. Nothing depends on it |
| Engine API keys and tenants (`/v1/auth/keys`) | Multi-tenant authentication for a hosted engine. Experimental; the chart does not use it |

None of these charges anyone. They are kept because they are tested and removing them buys nothing, and they must not be
described as a billing integration.

## What a paid offer needs first

In order. Each is also in [ROADMAP.md](ROADMAP.md).

1. A Release Candidate: recommender defaults validated on weeks of real traffic, multi-day runs on GKE and AKS, one reconciled
   saving.
2. Two or three design partners running Starter, then Professional, on real clusters. Their data sets the price.
3. The licence check (item 1 of the architecture).
4. A support process someone can actually staff, before any response time is written into a contract.
5. Legal review of the contract templates in this repository. They are templates, not reviewed by a lawyer.
6. Multi-cluster view, if Enterprise is to include it.

## What must not be said to a prospect

* That it reaches parity with, or beats, CAST AI, ScaleOps or Kubecost. No comparison has been run
  ([BENCHMARKING.md](BENCHMARKING.md)), and those products cover areas this one does not.
* A savings percentage. Two savings were observed, on test workloads, at list price.
* That savings are guaranteed.
* That there is a support SLA, a SOC 2 or ISO 27001 report, or a signed DPA.
* That it supports AWS.

What can be said: it starts in observe-only and explains every recommendation; changes need a policy and, by default, a human
approval; rollback is automatic and was exercised on real GKE and AKS clusters; every saving is labelled with its level of
evidence; and the list of what is not validated is public.
