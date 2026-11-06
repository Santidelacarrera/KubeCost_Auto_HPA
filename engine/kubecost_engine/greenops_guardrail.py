"""GreenOps Carbon-Aware Engine & Nexus Lab FinOps Budget Guardrail.

1. GreenOps Carbon Engine:
   - Tracks regional grid carbon intensity (`gCO2eq / kWh`), PUE, and active
     hardware power draw (CPU + Memory + GPU Watts).
   - Computes Scope-2 emissions (`gCO2eq / 1k Requests` and `kgCO2eq / month`)
     and recommends low-carbon regional/AZ workload shifting.
2. Nexus Lab FinOps Budget Guardrail (`CloudCost_Optimizer_FinOps_Guardrail` &
   `CloudForge_FinOps` integration):
   - Enforces tenant-level monthly budget caps with 80% early warning,
     end-of-month burn-rate forecasting, and deterministic enforcement modes:
     `NORMAL` (<80%), `ALERT` (80%-95%), `DEGRADE` (95%-100%: force Spot/MIG
     compression), and `BLOCK` (>100%: halt non-SLO horizontal expansion).
"""

from __future__ import annotations

from dataclasses import asdict, dataclass
from typing import Dict, List


@dataclass(frozen=True)
class RegionCarbonProfile:
    region: str
    cloud: str
    gco2_per_kwh: float
    pue: float
    renewable_pct: float


CARBON_GRID_CATALOG: Dict[str, RegionCarbonProfile] = {
    "us-east-1": RegionCarbonProfile("us-east-1", "aws", 379.0, 1.15, 38.0),
    "us-west-2": RegionCarbonProfile("us-west-2", "aws", 118.0, 1.11, 86.0),
    "eu-north-1": RegionCarbonProfile("eu-north-1", "aws", 19.0, 1.09, 98.0),
    "sa-east-1": RegionCarbonProfile("sa-east-1", "aws", 68.0, 1.14, 89.0),
    "us-central1": RegionCarbonProfile("us-central1", "gcp", 340.0, 1.10, 64.0),
    "europe-north1": RegionCarbonProfile("europe-north1", "gcp", 24.0, 1.08, 97.0),
    "eastus": RegionCarbonProfile("eastus", "azure", 365.0, 1.16, 42.0),
    "swedencentral": RegionCarbonProfile("swedencentral", "azure", 17.0, 1.08, 99.0),
}


@dataclass
class GreenOpsCarbonReport:
    current_region: str
    recommended_green_region: str
    current_gco2_per_kwh: float
    recommended_gco2_per_kwh: float
    cluster_power_kw: float
    optimized_power_kw: float
    current_monthly_kg_co2: float
    optimized_monthly_kg_co2: float
    monthly_co2_saved_kg: float
    co2_reduction_pct: float
    gco2_per_1k_requests: float
    equivalent_trees_planted_year: float

    def to_dict(self) -> Dict[str, object]:
        return asdict(self)


def evaluate_greenops_carbon(
    current_region: str = "us-east-1",
    active_cpu_cores: float = 48.0,
    reclaimed_cpu_cores: float = 18.0,
    active_memory_gib: float = 192.0,
    reclaimed_memory_gib: float = 64.0,
    active_gpus: int = 2,
    gpu_mig_savings_fraction: float = 0.50,
    rps: float = 840.0,
) -> GreenOpsCarbonReport:
    profile = CARBON_GRID_CATALOG.get(current_region, CARBON_GRID_CATALOG["us-east-1"])
    same_cloud_regions = [
        r for r in CARBON_GRID_CATALOG.values() if r.cloud == profile.cloud
    ] or list(CARBON_GRID_CATALOG.values())
    greenest = min(same_cloud_regions, key=lambda r: r.gco2_per_kwh)

    # Empirical datacenter power model: ~12W per active vCPU core, ~0.4W per GiB RAM, ~350W per GPU
    raw_watts = (active_cpu_cores * 12.0) + (active_memory_gib * 0.4) + (active_gpus * 350.0)
    opt_cores = max(2.0, active_cpu_cores - reclaimed_cpu_cores)
    opt_mem = max(4.0, active_memory_gib - reclaimed_memory_gib)
    opt_gpu_watts = active_gpus * 350.0 * max(0.15, 1.0 - gpu_mig_savings_fraction)
    opt_watts = (opt_cores * 12.0) + (opt_mem * 0.4) + opt_gpu_watts

    cluster_kw = round((raw_watts * profile.pue) / 1000.0, 4)
    optimized_kw = round((opt_watts * profile.pue) / 1000.0, 4)

    current_monthly_kg = round((cluster_kw * 730.0 * profile.gco2_per_kwh) / 1000.0, 2)
    # Optimized combines right-sizing power drop + optional green region shift for batch/stateless replicas
    blended_gco2 = (profile.gco2_per_kwh * 0.65) + (greenest.gco2_per_kwh * 0.35)
    optimized_monthly_kg = round((optimized_kw * 730.0 * blended_gco2) / 1000.0, 2)

    saved_kg = round(max(0.0, current_monthly_kg - optimized_monthly_kg), 2)
    reduction_pct = round(
        (saved_kg / max(current_monthly_kg, 0.01)) * 100.0, 2
    )
    requests_per_hour = max(rps * 3600.0, 1.0)
    hourly_gco2 = optimized_kw * blended_gco2
    gco2_per_1k_req = round(hourly_gco2 / (requests_per_hour / 1000.0), 4)
    # 1 mature tree absorbs ~21 kg CO2 per year
    trees_year = round((saved_kg * 12.0) / 21.0, 1)

    return GreenOpsCarbonReport(
        current_region=profile.region,
        recommended_green_region=greenest.region,
        current_gco2_per_kwh=profile.gco2_per_kwh,
        recommended_gco2_per_kwh=greenest.gco2_per_kwh,
        cluster_power_kw=cluster_kw,
        optimized_power_kw=optimized_kw,
        current_monthly_kg_co2=current_monthly_kg,
        optimized_monthly_kg_co2=optimized_monthly_kg,
        monthly_co2_saved_kg=saved_kg,
        co2_reduction_pct=reduction_pct,
        gco2_per_1k_requests=gco2_per_1k_req,
        equivalent_trees_planted_year=trees_year,
    )


@dataclass
class FinOpsBudgetGuardrailDecision:
    tenant_id: str
    monthly_budget_usd: float
    mtd_spend_usd: float
    projected_eom_spend_usd: float
    utilization_pct: float
    projected_utilization_pct: float
    mode: str  # NORMAL | ALERT | DEGRADE | BLOCK
    allow_scale_out: bool
    force_spot_and_mig: bool
    alert_triggered: bool
    dedup_key: str
    linked_ecosystem: List[str]
    reason: str

    def to_dict(self) -> Dict[str, object]:
        return asdict(self)


def evaluate_finops_budget_guardrail(
    tenant_id: str = "org-prod-01",
    monthly_budget_usd: float = 15000.0,
    mtd_spend_usd: float = 9200.0,
    day_of_month: int = 20,
    days_in_month: int = 30,
    slo_critical_breach: bool = False,
) -> FinOpsBudgetGuardrailDecision:
    budget = max(1.0, float(monthly_budget_usd))
    day = max(1, min(int(day_of_month), int(days_in_month)))
    daily_burn = mtd_spend_usd / float(day)
    projected_eom = round(daily_burn * float(days_in_month), 2)

    utilization_pct = round((mtd_spend_usd / budget) * 100.0, 2)
    projected_pct = round((projected_eom / budget) * 100.0, 2)
    effective_pct = max(utilization_pct, projected_pct)

    if effective_pct > 100.0:
        mode = "BLOCK"
        allow_scale_out = bool(slo_critical_breach)
        force_spot = True
        alert = True
        reason = (
            f"Budget guardrail BLOCK: projected EOM ${projected_eom:.2f} ({projected_pct:.1f}%) "
            f"exceeds cap ${budget:.2f}. Non-SLO scale-out blocked; Spot/MIG enforced."
        )
    elif effective_pct >= 95.0:
        mode = "DEGRADE"
        allow_scale_out = True
        force_spot = True
        alert = True
        reason = (
            f"Budget guardrail DEGRADE: projected EOM ${projected_eom:.2f} ({projected_pct:.1f}%) "
            f">= 95% of cap ${budget:.2f}. Forcing Spot & MIG fractional sizing."
        )
    elif effective_pct >= 80.0:
        mode = "ALERT"
        allow_scale_out = True
        force_spot = False
        alert = True
        reason = (
            f"Budget guardrail ALERT (80% threshold): projected EOM ${projected_eom:.2f} "
            f"({projected_pct:.1f}%) against cap ${budget:.2f}."
        )
    else:
        mode = "NORMAL"
        allow_scale_out = True
        force_spot = False
        alert = False
        reason = (
            f"Budget guardrail NORMAL: projected EOM ${projected_eom:.2f} "
            f"({projected_pct:.1f}%) within cap ${budget:.2f}."
        )

    return FinOpsBudgetGuardrailDecision(
        tenant_id=tenant_id,
        monthly_budget_usd=round(budget, 2),
        mtd_spend_usd=round(mtd_spend_usd, 2),
        projected_eom_spend_usd=projected_eom,
        utilization_pct=utilization_pct,
        projected_utilization_pct=projected_pct,
        mode=mode,
        allow_scale_out=allow_scale_out,
        force_spot_and_mig=force_spot,
        alert_triggered=alert,
        dedup_key=f"nexus-finops-guardrail:{tenant_id}:{mode}:{day}",
        linked_ecosystem=[
            "KubeCost_Auto_HPA",
            "CloudCost_Optimizer_FinOps_Guardrail",
            "CloudForge_FinOps",
        ],
        reason=reason,
    )
