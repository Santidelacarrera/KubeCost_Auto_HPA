# ADR-0004: node consolidation is a plan a person follows, not an action the product takes

Date: 2026-11-16. Status: amended by the assisted consolidation workflow documented below.

The current release supports an explicitly invoked executor for approved plans covering opt-in
nodes. An assisted plan can authorize bounded stateless eviction under a matching PDB.
Autonomous execution remains outside this decision. See [operating instructions](../OPERATIONS.md#assisted-node-consolidation).

## Context

Rightsizing lowers requests. Money is saved only when a node is released. On GKE and AKS the cluster autoscaler does that by
itself once requests allow it: it was observed on both (docs/LAB.md results 12, 17 and 26). The question was whether the product
should also drain nodes itself, to consolidate faster or where no autoscaler runs.

Draining a node moves every workload on it. Getting it wrong takes services down. The analysis (`rightsize nodes`) is a
simulation on requests: it honours taints, selectors, zonal volumes, anti-affinity, disruption budgets and zones, and it does
not evaluate preferred affinities or topology spread constraints. On its first run against a real GKE cluster it was wrong
twice (static pods, the CSI zone key). A simulation that was wrong twice on day one is not a basis for evicting pods unattended.

## Decision

1. The operator does not automatically cordon, drain or delete nodes to consolidate. Its Spot drain reacts to a node the
   provider is already taking away. The separate assisted executor requires an approved plan.
2. `rightsize nodes --plan` prints an ordered, manual plan for the candidate nodes: preconditions, the two `kubectl` commands,
   what to verify before the next node, and how to undo. It refuses while pods are pending as unschedulable or nodes report
   pressure.
3. On GKE and AKS the recommended path stays: rightsize, then let the cluster autoscaler release the node.

## What an automatic consolidation would need (not built)

A controller of its own, shaped like the rightsizing applier: a `NodeConsolidation` object published by the analysis, approved
by a different user, applied under a policy (windows, one node at a time, a rate limit, excluded node pools), observed (pending
pods, availability, an SLO query), and rolled back by uncordoning and stopping at the first failed check. Before it is built:
the analysis must be compared with real drains on GKE and AKS across many node shapes, and topology spread constraints must be
evaluated or the affected pods excluded.

## Consequences

* A cluster without an autoscaler gets a plan, not a saving. That is slower and it is safe.
* `KubeCostAutoscaler.spec.nodeBinPacking`, a field no code read, was removed in v0.2.0.
