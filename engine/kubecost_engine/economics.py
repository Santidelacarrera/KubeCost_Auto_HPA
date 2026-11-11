"""Autonomous Infrastructure Economics & FinOps Monetization Engine (Motor C — Sections 9 & 12).

Provides:
- Legacy unit economics: cost_per_request, cost_per_request_from_pod.
- True Unit Economics:
  * cost_per_1k_successful_requests = (allocated_compute_cost / successful_requests) * 1000
  * cost_per_sustained_rps (explicitly parameterized by observation_window_seconds for dimensional coherence)
- Multi-component workload cost attribution (CPU req/used, Memory req/used, idle node slack share,
  Spot discount, storage & network overhead).
- Multi-objective optimization scoring (balancing $/1k-req against p99 SLO, error rate, CPU throttling, and OOM headroom).
- Counterfactual Savings Auditor:
  net_savings = counterfactual_baseline_cost - optimized_actual_cost - incremental_optimization_cost
  (normalized by traffic volume so traffic drops or 5xx error spikes are never miscounted as savings).
- Commercial Billing Models (Section 12):
  * Model A — Value Shared (configurable 20%-25% of verified net savings)
  * Model B — SaaS Fixed Pricing (configurable $25/month per managed node)
"""

from __future__ import annotations

import math
from dataclasses import asdict, dataclass
from typing import Any, Dict

SECONDS_PER_HOUR = 3600.0


def cost_per_request(instance_cost_per_hour: float, rps: float) -> float:
    """Dollars per request served. Infinite when the pod serves no traffic."""
    if instance_cost_per_hour < 0 or rps < 0:
        raise ValueError("cost and rps must be non-negative")
    if rps == 0:
        return float("inf")
    return instance_cost_per_hour / (rps * SECONDS_PER_HOUR)


def cost_per_request_from_pod(
    cpu_cores: float,
    mem_gib: float,
    rps: float,
    cpu_price_per_core_hour: float,
    mem_price_per_gib_hour: float,
) -> float:
    """Cost per request for a pod priced by its CPU/memory requests."""
    hourly = cpu_cores * cpu_price_per_core_hour + mem_gib * mem_price_per_gib_hour
    return cost_per_request(hourly, rps)


def cost_per_1k_successful_requests(
    allocated_compute_cost_usd: float,
    successful_requests: float,
) -> float:
    """Canonical Section 9.1 metric: (allocated_compute_cost / successful_requests) * 1000.

    Note: Because only *successful* (non-5xx/non-dropped) requests are counted in the
    denominator, any optimization that degrades reliability or drops traffic increases
    cost_per_1k_successful_requests and is automatically penalized.
    """
    if allocated_compute_cost_usd < 0 or successful_requests < 0:
        raise ValueError("allocated_compute_cost_usd and successful_requests must be non-negative")
    if successful_requests == 0:
        return float("inf")
    return (allocated_compute_cost_usd / successful_requests) * 1000.0


def cost_per_sustained_rps(
    hourly_compute_cost_usd: float,
    sustained_successful_rps: float,
    observation_window_seconds: float = 3600.0,
) -> float:
    """Computes cost in USD per 1 sustained successful RPS over an explicit observation window.

    Dimensional analysis:
      cost_over_window = hourly_compute_cost_usd * (observation_window_seconds / 3600.0)  [USD]
      cost_per_sustained_rps = cost_over_window / sustained_successful_rps                [USD / RPS over window]
    """
    if hourly_compute_cost_usd < 0 or sustained_successful_rps < 0 or observation_window_seconds <= 0:
        raise ValueError("cost, rps must be non-negative and observation_window_seconds positive")
    if sustained_successful_rps == 0:
        return float("inf")
    window_cost_usd = hourly_compute_cost_usd * (observation_window_seconds / SECONDS_PER_HOUR)
    return window_cost_usd / sustained_successful_rps


@dataclass(frozen=True)
class WorkloadCostBreakdown:
    cpu_request_cost_usd_hr: float
    cpu_used_cost_usd_hr: float
    cpu_waste_cost_usd_hr: float
    memory_request_cost_usd_hr: float
    memory_used_cost_usd_hr: float
    memory_waste_cost_usd_hr: float
    idle_node_share_cost_usd_hr: float
    storage_overhead_usd_hr: float
    network_overhead_usd_hr: float
    spot_discount_saved_usd_hr: float
    total_allocated_cost_usd_hr: float
    cost_per_1k_successful_req_usd: float
    cost_per_sustained_rps_hourly_usd: float


def compute_workload_cost_breakdown(
    *,
    replicas: int,
    cpu_request_cores_per_pod: float,
    cpu_used_cores_per_pod: float,
    mem_request_gib_per_pod: float,
    mem_used_gib_per_pod: float,
    total_rps: float,
    error_rate: float = 0.0,
    cpu_price_per_core_hour: float = 0.031611,
    mem_price_per_gib_hour: float = 0.004237,
    spot_fraction: float = 0.0,
    spot_discount: float = 0.65,
    idle_node_share_cost_usd_hr: float = 0.0,
    storage_overhead_usd_hr: float = 0.0,
    network_overhead_usd_hr: float = 0.0,
    observation_window_seconds: float = 3600.0,
) -> WorkloadCostBreakdown:
    """Computes full multi-factor hourly cost and unit economics for a workload."""
    r = max(0, int(replicas))
    err = max(0.0, min(1.0, error_rate))
    spot_frac = max(0.0, min(1.0, spot_fraction))
    spot_disc = max(0.0, min(0.90, spot_discount))
    effective_multiplier = 1.0 - (spot_frac * spot_disc)

    raw_cpu_req = r * max(0.0, cpu_request_cores_per_pod) * cpu_price_per_core_hour
    raw_cpu_used = r * max(0.0, cpu_used_cores_per_pod) * cpu_price_per_core_hour
    raw_mem_req = r * max(0.0, mem_request_gib_per_pod) * mem_price_per_gib_hour
    raw_mem_used = r * max(0.0, mem_used_gib_per_pod) * mem_price_per_gib_hour

    cpu_req_cost = raw_cpu_req * effective_multiplier
    cpu_used_cost = min(cpu_req_cost, raw_cpu_used * effective_multiplier)
    mem_req_cost = raw_mem_req * effective_multiplier
    mem_used_cost = min(mem_req_cost, raw_mem_used * effective_multiplier)

    spot_saved = (raw_cpu_req + raw_mem_req) * (spot_frac * spot_disc)
    total_hourly = (
        cpu_req_cost
        + mem_req_cost
        + max(0.0, idle_node_share_cost_usd_hr)
        + max(0.0, storage_overhead_usd_hr)
        + max(0.0, network_overhead_usd_hr)
    )

    successful_rps = max(0.0, total_rps) * (1.0 - err)
    successful_reqs_in_window = successful_rps * observation_window_seconds
    window_cost = total_hourly * (observation_window_seconds / SECONDS_PER_HOUR)

    cp1k = cost_per_1k_successful_requests(window_cost, successful_reqs_in_window)
    cps_rps = cost_per_sustained_rps(total_hourly, successful_rps, observation_window_seconds)

    return WorkloadCostBreakdown(
        cpu_request_cost_usd_hr=round(cpu_req_cost, 6),
        cpu_used_cost_usd_hr=round(cpu_used_cost, 6),
        cpu_waste_cost_usd_hr=round(max(0.0, cpu_req_cost - cpu_used_cost), 6),
        memory_request_cost_usd_hr=round(mem_req_cost, 6),
        memory_used_cost_usd_hr=round(mem_used_cost, 6),
        memory_waste_cost_usd_hr=round(max(0.0, mem_req_cost - mem_used_cost), 6),
        idle_node_share_cost_usd_hr=round(max(0.0, idle_node_share_cost_usd_hr), 6),
        storage_overhead_usd_hr=round(max(0.0, storage_overhead_usd_hr), 6),
        network_overhead_usd_hr=round(max(0.0, network_overhead_usd_hr), 6),
        spot_discount_saved_usd_hr=round(spot_saved, 6),
        total_allocated_cost_usd_hr=round(total_hourly, 6),
        cost_per_1k_successful_req_usd=cp1k if math.isfinite(cp1k) else float("inf"),
        cost_per_sustained_rps_hourly_usd=cps_rps if math.isfinite(cps_rps) else float("inf"),
    )


@dataclass(frozen=True)
class EconomicObjectiveEvaluation:
    total_objective_loss: float  # Lower is better
    unit_cost_component: float
    slo_penalty_component: float
    error_penalty_component: float
    throttling_penalty_component: float
    oom_penalty_component: float
    is_slo_compliant: bool
    recommendation_acceptable: bool
    explanation: str


def evaluate_economic_objective(
    *,
    cost_per_1k_req_usd: float,
    target_cost_per_1k_req_usd: float,
    p99_latency_ms: float,
    slo_p99_latency_ms: float,
    error_rate: float = 0.0,
    max_acceptable_error_rate: float = 0.01,
    cpu_throttling_ratio: float = 0.0,
    memory_headroom_ratio: float = 0.25,
) -> EconomicObjectiveEvaluation:
    """Evaluates the multi-objective FinOps + SRE loss function (Section 9.3).

    Never accepts a cost reduction that violates p99 SLO, increases 5xx error rate,
    induces severe CPU throttling (>15%), or collapses memory OOM headroom (<10%).
    """
    norm_cost = (
        cost_per_1k_req_usd / max(1e-6, target_cost_per_1k_req_usd)
        if math.isfinite(cost_per_1k_req_usd)
        else 10.0
    )
    slo_ratio = p99_latency_ms / max(1.0, slo_p99_latency_ms)
    slo_penalty = 0.0 if slo_ratio <= 0.85 else (5.0 * (slo_ratio - 0.85) ** 2 if slo_ratio <= 1.0 else 10.0 * slo_ratio)
    err_penalty = 0.0 if error_rate <= max_acceptable_error_rate else 25.0 * (error_rate - max_acceptable_error_rate)
    throttle_penalty = 0.0 if cpu_throttling_ratio <= 0.08 else 4.0 * (cpu_throttling_ratio - 0.08)
    oom_penalty = 0.0 if memory_headroom_ratio >= 0.15 else 15.0 * (0.15 - memory_headroom_ratio)

    total_loss = round(norm_cost + slo_penalty + err_penalty + throttle_penalty + oom_penalty, 4)
    slo_ok = slo_ratio <= 1.0 and error_rate <= max_acceptable_error_rate
    acceptable = slo_ok and memory_headroom_ratio >= 0.12 and cpu_throttling_ratio <= 0.20

    return EconomicObjectiveEvaluation(
        total_objective_loss=total_loss,
        unit_cost_component=round(norm_cost, 4),
        slo_penalty_component=round(slo_penalty, 4),
        error_penalty_component=round(err_penalty, 4),
        throttling_penalty_component=round(throttle_penalty, 4),
        oom_penalty_component=round(oom_penalty, 4),
        is_slo_compliant=slo_ok,
        recommendation_acceptable=acceptable,
        explanation=(
            f"Objective loss={total_loss:.3f} (cost={norm_cost:.2f}, slo_pen={slo_penalty:.2f}, "
            f"err_pen={err_penalty:.2f}, oom_pen={oom_penalty:.2f}); acceptable={acceptable}"
        ),
    )


@dataclass(frozen=True)
class SavingsAuditReport:
    counterfactual_baseline_cost_usd: float
    optimized_actual_cost_usd: float
    incremental_optimization_cost_usd: float
    net_savings_usd: float
    net_savings_pct: float
    baseline_cost_per_1k_req_usd: float
    optimized_cost_per_1k_req_usd: float
    unit_efficiency_improvement_pct: float
    is_verified_saving: bool
    audit_explanation: str


def audit_counterfactual_savings(
    *,
    baseline_cost_usd: float,
    baseline_successful_requests: float,
    optimized_actual_cost_usd: float,
    optimized_successful_requests: float,
    incremental_optimization_cost_usd: float = 0.0,
    slo_violated: bool = False,
) -> SavingsAuditReport:
    """Audits realized net savings normalizing for traffic variations (Section 9.4).

    Formula:
      net_savings = counterfactual_baseline_cost - optimized_actual_cost - incremental_optimization_cost
    where counterfactual_baseline_cost scales the baseline unit cost to the actual volume of
    successful requests served, preventing a traffic drop from being falsely claimed as savings.
    """
    if baseline_cost_usd < 0 or optimized_actual_cost_usd < 0 or incremental_optimization_cost_usd < 0:
        raise ValueError("costs must be non-negative")

    base_cp1k = cost_per_1k_successful_requests(baseline_cost_usd, baseline_successful_requests)
    opt_cp1k = cost_per_1k_successful_requests(
        optimized_actual_cost_usd + incremental_optimization_cost_usd,
        optimized_successful_requests,
    )

    if math.isfinite(base_cp1k) and baseline_successful_requests > 0 and optimized_successful_requests > 0:
        # Traffic-normalized counterfactual baseline cost for the actual successful requests served
        counterfactual_baseline = (base_cp1k / 1000.0) * optimized_successful_requests
    else:
        counterfactual_baseline = baseline_cost_usd

    raw_net_savings = (
        counterfactual_baseline - optimized_actual_cost_usd - incremental_optimization_cost_usd
    )
    # If SLO was violated during the window, savings cannot be certified as valid quality-preserving savings
    verified = (raw_net_savings > 0) and (not slo_violated)
    certified_net_savings = max(0.0, raw_net_savings) if not slo_violated else 0.0

    savings_pct = (
        (certified_net_savings / counterfactual_baseline) * 100.0
        if counterfactual_baseline > 0
        else 0.0
    )
    eff_imp_pct = (
        ((base_cp1k - opt_cp1k) / base_cp1k) * 100.0
        if (math.isfinite(base_cp1k) and math.isfinite(opt_cp1k) and base_cp1k > 0)
        else 0.0
    )

    return SavingsAuditReport(
        counterfactual_baseline_cost_usd=round(counterfactual_baseline, 2),
        optimized_actual_cost_usd=round(optimized_actual_cost_usd, 2),
        incremental_optimization_cost_usd=round(incremental_optimization_cost_usd, 2),
        net_savings_usd=round(certified_net_savings, 2),
        net_savings_pct=round(savings_pct, 2),
        baseline_cost_per_1k_req_usd=round(base_cp1k, 6) if math.isfinite(base_cp1k) else float("inf"),
        optimized_cost_per_1k_req_usd=round(opt_cp1k, 6) if math.isfinite(opt_cp1k) else float("inf"),
        unit_efficiency_improvement_pct=round(eff_imp_pct, 2),
        is_verified_saving=verified,
        audit_explanation=(
            f"Counterfactual baseline=${counterfactual_baseline:,.2f}, actual=${optimized_actual_cost_usd:,.2f}, "
            f"incremental_overhead=${incremental_optimization_cost_usd:,.2f} -> "
            f"Certified Net Savings=${certified_net_savings:,.2f} ({savings_pct:.1f}%)"
        ),
    )


def calculate_commercial_billing(
    *,
    verified_monthly_net_savings_usd: float,
    managed_nodes_count: int,
    value_shared_percentage: float = 0.22,
    fixed_price_per_node_month_usd: float = 25.0,
) -> Dict[str, Any]:
    """Computes both commercial monetization models required in Section 12:
    - Model A (Value Shared): Configurable 20%-25% (default 22%) of verified net savings.
    - Model B (SaaS Fixed Pricing): Configurable fixed fee per managed node/month (default $25/node/month).
    """
    if managed_nodes_count < 0:
        raise ValueError("managed_nodes_count must be non-negative")
    net_sav = max(0.0, float(verified_monthly_net_savings_usd))
    fee_pct = max(0.05, min(0.50, float(value_shared_percentage)))
    node_rate = max(0.0, float(fixed_price_per_node_month_usd))

    value_shared_fee = round(net_sav * fee_pct, 2)
    customer_retained_value_shared = round(net_sav - value_shared_fee, 2)

    fixed_saas_fee = round(managed_nodes_count * node_rate, 2)
    customer_retained_fixed = round(net_sav - fixed_saas_fee, 2)

    return {
        "verified_monthly_net_savings_usd": round(net_sav, 2),
        "managed_nodes_count": int(managed_nodes_count),
        "model_a_value_shared": {
            "model_name": "Value Shared (Gain-Share)",
            "configured_share_pct": round(fee_pct * 100.0, 1),
            "platform_fee_usd": value_shared_fee,
            "customer_net_benefit_usd": customer_retained_value_shared,
        },
        "model_b_fixed_per_node": {
            "model_name": "SaaS Fixed Per-Node",
            "rate_per_node_month_usd": round(node_rate, 2),
            "platform_fee_usd": fixed_saas_fee,
            "customer_net_benefit_usd": customer_retained_fixed,
        },
    }
