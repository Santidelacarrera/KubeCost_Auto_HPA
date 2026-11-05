"""Node Bin-Packing & Workload Consolidation Optimizer.

Reverse-engineered from ScaleOps Context-Aware Pod Placement & Bin-Packing (REA methodology).
Analyzes cluster node fragmentation, identifies underutilized nodes, calculates consolidation
feasibility, and generates drain/eviction targets to enable cloud node termination (Karpenter/CA).
"""

from dataclasses import dataclass, field
from typing import Dict, List, Optional


@dataclass
class PodPlacement:
    name: str
    namespace: str
    cpu_request_millicores: int
    memory_request_mib: int
    can_evict: bool = True


@dataclass
class NodeState:
    name: str
    instance_type: str
    zone: str
    monthly_cost_usd: float
    total_cpu_millicores: int
    total_memory_mib: int
    allocated_cpu_millicores: int
    allocated_memory_mib: int
    pods: List[PodPlacement] = field(default_factory=list)

    @property
    def cpu_utilization_pct(self) -> float:
        return (self.allocated_cpu_millicores / self.total_cpu_millicores) * 100.0 if self.total_cpu_millicores > 0 else 0.0

    @property
    def memory_utilization_pct(self) -> float:
        return (self.allocated_memory_mib / self.total_memory_mib) * 100.0 if self.total_memory_mib > 0 else 0.0

    @property
    def available_cpu_millicores(self) -> int:
        return max(0, self.total_cpu_millicores - self.allocated_cpu_millicores)

    @property
    def available_memory_mib(self) -> int:
        return max(0, self.total_memory_mib - self.allocated_memory_mib)


@dataclass(frozen=True)
class RelocationPlan:
    pod_name: str
    namespace: str
    from_node: str
    to_node: str
    cpu_millicores: int
    memory_mib: int


@dataclass(frozen=True)
class ConsolidationResult:
    cluster_efficiency_before_pct: float
    cluster_efficiency_after_pct: float
    total_nodes: int
    nodes_can_drain: List[str]
    monthly_savings_usd: float
    relocation_plans: List[RelocationPlan]
    reason: str


class NodeBinPacker:
    """Simulates intelligent pod bin-packing to identify drainable nodes."""

    def __init__(self, target_node_utilization: float = 0.85, max_eviction_ratio: float = 0.25):
        self.target_node_utilization = target_node_utilization
        self.max_eviction_ratio = max_eviction_ratio

    def optimize_cluster(self, nodes: List[NodeState]) -> ConsolidationResult:
        """Finds underutilized nodes whose pods can fit into existing nodes with headroom."""
        if not nodes:
            return ConsolidationResult(
                cluster_efficiency_before_pct=0.0,
                cluster_efficiency_after_pct=0.0,
                total_nodes=0,
                nodes_can_drain=[],
                monthly_savings_usd=0.0,
                relocation_plans=[],
                reason="Empty cluster provided",
            )

        total_cpu = sum(n.total_cpu_millicores for n in nodes)
        total_alloc_cpu = sum(n.allocated_cpu_millicores for n in nodes)
        eff_before = (total_alloc_cpu / total_cpu * 100.0) if total_cpu > 0 else 0.0

        # Sort nodes: lowest utilization first (best candidates for draining)
        sorted_nodes = sorted(nodes, key=lambda n: n.cpu_utilization_pct)
        target_pool = [n for n in sorted_nodes if n.cpu_utilization_pct >= 40.0]

        drainable_nodes: List[str] = []
        relocations: List[RelocationPlan] = []
        monthly_savings = 0.0

        # Attempt to drain lowest-utilized nodes (< 35% utilization)
        for candidate in sorted_nodes:
            if candidate.cpu_utilization_pct >= 35.0:
                continue

            # Check if all pods are evictable
            if any(not p.can_evict for p in candidate.pods):
                continue

            # Simulate bin-packing pods into target_pool
            candidate_fits = True
            temp_relocations = []
            
            # Temporary capacity tracker for target pool
            avail_cpu = {n.name: n.available_cpu_millicores for n in target_pool}
            avail_mem = {n.name: n.available_memory_mib for n in target_pool}

            for pod in candidate.pods:
                placed = False
                for target in target_pool:
                    if target.name == candidate.name:
                        continue
                    if (
                        avail_cpu[target.name] >= pod.cpu_request_millicores
                        and avail_mem[target.name] >= pod.memory_request_mib
                    ):
                        avail_cpu[target.name] -= pod.cpu_request_millicores
                        avail_mem[target.name] -= pod.memory_request_mib
                        temp_relocations.append(
                            RelocationPlan(
                                pod_name=pod.name,
                                namespace=pod.namespace,
                                from_node=candidate.name,
                                to_node=target.name,
                                cpu_millicores=pod.cpu_request_millicores,
                                memory_mib=pod.memory_request_mib,
                            )
                        )
                        placed = True
                        break
                if not placed:
                    candidate_fits = False
                    break

            if candidate_fits and temp_relocations:
                drainable_nodes.append(candidate.name)
                relocations.extend(temp_relocations)
                monthly_savings += candidate.monthly_cost_usd

        # Calculate efficiency after consolidation
        remaining_nodes = len(nodes) - len(drainable_nodes)
        remaining_total_cpu = sum(
            n.total_cpu_millicores for n in nodes if n.name not in drainable_nodes
        )
        eff_after = (total_alloc_cpu / remaining_total_cpu * 100.0) if remaining_total_cpu > 0 else eff_before

        reason = (
            f"ScaleOps Bin-Packing: identified {len(drainable_nodes)} drainable nodes. "
            f"Relocating {len(relocations)} pods boosts cluster packing density from "
            f"{eff_before:.1f}% to {eff_after:.1f}%, saving ${monthly_savings:,.2f}/mo."
        )

        return ConsolidationResult(
            cluster_efficiency_before_pct=round(eff_before, 1),
            cluster_efficiency_after_pct=round(eff_after, 1),
            total_nodes=len(nodes),
            nodes_can_drain=drainable_nodes,
            monthly_savings_usd=round(monthly_savings, 2),
            relocation_plans=relocations,
            reason=reason,
        )
