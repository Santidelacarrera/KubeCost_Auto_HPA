"""AI / GPU Fractional Right-Sizing & NVIDIA MIG Optimizer.

Optimizes Kubernetes GPU workloads (`nvidia.com/gpu` and NVIDIA MIG slices)
using Streaming Multiprocessor (SM) utilization, VRAM watermark, and LLM
Token / Inference Request unit economics (`$/1k Tokens` and `$/1k Inferences`).
"""

from __future__ import annotations

from dataclasses import asdict, dataclass
from typing import Dict, List


@dataclass(frozen=True)
class GPUSpec:
    model: str
    vram_gib: float
    tdp_watts: float
    hourly_ondemand_usd: float
    hourly_spot_usd: float
    supports_mig: bool


@dataclass(frozen=True)
class MIGProfile:
    resource_name: str
    compute_slices: int
    total_slices: int
    vram_gib: float

    @property
    def cost_fraction(self) -> float:
        return self.compute_slices / float(self.total_slices)


GPU_CATALOG: Dict[str, GPUSpec] = {
    "H100-80GB": GPUSpec("H100-80GB", 80.0, 700.0, 4.25, 1.65, True),
    "A100-80GB": GPUSpec("A100-80GB", 80.0, 400.0, 2.85, 0.98, True),
    "A100-40GB": GPUSpec("A100-40GB", 40.0, 400.0, 2.10, 0.72, True),
    "L4-24GB": GPUSpec("L4-24GB", 24.0, 72.0, 0.81, 0.28, False),
    "T4-16GB": GPUSpec("T4-16GB", 16.0, 70.0, 0.526, 0.16, False),
}

MIG_PROFILES_80GB: List[MIGProfile] = [
    MIGProfile("nvidia.com/mig-1g.10gb", 1, 7, 10.0),
    MIGProfile("nvidia.com/mig-2g.20gb", 2, 7, 20.0),
    MIGProfile("nvidia.com/mig-3g.40gb", 3, 7, 40.0),
    MIGProfile("nvidia.com/mig-7g.80gb", 7, 7, 80.0),
]

MIG_PROFILES_40GB: List[MIGProfile] = [
    MIGProfile("nvidia.com/mig-1g.5gb", 1, 7, 5.0),
    MIGProfile("nvidia.com/mig-2g.10gb", 2, 7, 10.0),
    MIGProfile("nvidia.com/mig-3g.20gb", 3, 7, 20.0),
    MIGProfile("nvidia.com/mig-7g.40gb", 7, 7, 40.0),
]


@dataclass
class GPUOptimizationPlan:
    workload: str
    current_gpu_model: str
    recommended_gpu_model: str
    recommended_resource_name: str
    action: str
    use_spot: bool
    sm_utilization_pct: float
    vram_used_gib: float
    allocated_vram_gib: float
    vram_headroom_pct: float
    current_hourly_usd: float
    projected_hourly_usd: float
    monthly_savings_usd: float
    cost_per_1k_tokens_usd: float
    cost_per_1k_inferences_usd: float
    reason: str

    def to_dict(self) -> Dict[str, object]:
        return asdict(self)


def evaluate_gpu_workload(
    workload: str = "llm-gateway-vllm",
    current_gpu_model: str = "A100-80GB",
    requested_gpus: int = 1,
    sm_utilization_pct: float = 22.0,
    vram_used_gib: float = 13.5,
    tokens_per_second: float = 540.0,
    inference_rps: float = 18.0,
    spot_tolerant: bool = True,
    latency_p99_ms: float = 185.0,
    latency_slo_ms: float = 350.0,
) -> GPUOptimizationPlan:
    spec = GPU_CATALOG.get(current_gpu_model, GPU_CATALOG["A100-80GB"])
    gpus = max(1, int(requested_gpus))
    current_hourly = spec.hourly_ondemand_usd * gpus

    required_vram = max(vram_used_gib * 1.25, 2.0)
    required_sm_fraction = max((sm_utilization_pct / 100.0) * 1.25, 0.10)
    slo_breached = latency_slo_ms > 0 and latency_p99_ms > latency_slo_ms

    rec_model = spec.model
    rec_resource = "nvidia.com/gpu"
    cost_fraction = 1.0
    action = "MAINTAIN_FULL_GPU"
    allocated_vram = spec.vram_gib

    if not slo_breached and gpus == 1:
        if (
            spec.supports_mig
            and required_vram <= spec.vram_gib * 0.55
            and required_sm_fraction <= 0.50
        ):
            profiles = MIG_PROFILES_80GB if spec.vram_gib >= 80.0 else MIG_PROFILES_40GB
            for p in profiles:
                if p.vram_gib >= required_vram and p.cost_fraction >= required_sm_fraction:
                    rec_resource = p.resource_name
                    cost_fraction = p.cost_fraction
                    allocated_vram = p.vram_gib
                    if p.compute_slices < p.total_slices:
                        action = "PARTITION_NVIDIA_MIG"
                    break
        elif required_vram <= 20.0 and sm_utilization_pct <= 45.0 and spec.model in ("H100-80GB", "A100-80GB"):
            l4 = GPU_CATALOG["L4-24GB"]
            rec_model = l4.model
            spec = l4
            allocated_vram = l4.vram_gib
            action = "RIGHTSIZE_GPU_FAMILY_L4"

    use_spot = bool(spot_tolerant and not slo_breached)
    base_hourly = spec.hourly_spot_usd if use_spot else spec.hourly_ondemand_usd
    if use_spot and action == "MAINTAIN_FULL_GPU":
        action = "SHIFT_GPU_TO_SPOT"

    projected_hourly = min(current_hourly, round(base_hourly * cost_fraction * gpus, 4))
    monthly_savings = round(max(0.0, (current_hourly - projected_hourly) * 730.0), 2)

    tokens_per_hour = max(tokens_per_second * 3600.0, 1.0)
    inferences_per_hour = max(inference_rps * 3600.0, 1.0)
    cost_per_1k_tokens = round(projected_hourly / (tokens_per_hour / 1000.0), 6)
    cost_per_1k_inferences = round(projected_hourly / (inferences_per_hour / 1000.0), 6)
    vram_headroom = round(max(0.0, ((allocated_vram - vram_used_gib) / max(allocated_vram, 1.0)) * 100.0), 2)

    return GPUOptimizationPlan(
        workload=workload,
        current_gpu_model=current_gpu_model,
        recommended_gpu_model=rec_model,
        recommended_resource_name=rec_resource,
        action=action,
        use_spot=use_spot,
        sm_utilization_pct=round(sm_utilization_pct, 2),
        vram_used_gib=round(vram_used_gib, 2),
        allocated_vram_gib=round(allocated_vram, 2),
        vram_headroom_pct=vram_headroom,
        current_hourly_usd=round(current_hourly, 4),
        projected_hourly_usd=projected_hourly,
        monthly_savings_usd=monthly_savings,
        cost_per_1k_tokens_usd=cost_per_1k_tokens,
        cost_per_1k_inferences_usd=cost_per_1k_inferences,
        reason=(
            f"{action}: {current_gpu_model} -> {rec_model} ({rec_resource}, spot={use_spot}) | "
            f"Saves ${monthly_savings:.2f}/mo | ${cost_per_1k_tokens:.6f}/1k tokens"
        ),
    )
