"""SLO-aware in-place resize recommendations."""
import math
from dataclasses import dataclass


@dataclass(frozen=True)
class ResizeDecision:
    action: str  # "shrink" | "grow" | "hold"
    cpu_millicores: int
    reason: str
    ratio: float = 0.0  # p99 / SLO
    limit: str | None = None  # "max" | "min" when the bound prevented a change


def recommend_resize(
    cpu_millicores: int,
    p99_ms: float,
    slo_p99_ms: float,
    *,
    shrink_below: float = 0.5,
    grow_above: float = 0.9,
    shrink_step: float = 0.8,
    grow_step: float = 1.25,
    min_millicores: int = 50,
    max_millicores: int = 8000,
    cpu_usage_millicores: float | None = None,
    target_utilization: float = 0.7,
    cost_over_target: bool = False,
) -> ResizeDecision:
    """Shrink when p99 is well under the SLO, grow when it nears it, else hold.

    Safety: a shrink never goes below ``usage / target_utilization`` so the pod
    keeps CPU headroom over what it actually consumes. When ``cost_over_target``
    is set (cost per request above the configured ceiling) shrinking is also
    allowed inside the hold band, as long as p99 is not close to the SLO.
    """
    if slo_p99_ms <= 0:
        raise ValueError("slo_p99_ms must be positive")
    if not 0 < target_utilization <= 1:
        raise ValueError("target_utilization must be in (0, 1]")
    ratio = p99_ms / slo_p99_ms
    if cpu_usage_millicores is not None:
        min_millicores = max(min_millicores, int(math.ceil(cpu_usage_millicores / target_utilization)))
        min_millicores = min(min_millicores, max_millicores)
    if ratio >= grow_above:
        new = min(max_millicores, int(round(cpu_millicores * grow_step)))
        if new > cpu_millicores:
            return ResizeDecision("grow", new, f"p99/SLO={ratio:.2f} >= {grow_above}", ratio)
        return ResizeDecision("hold", cpu_millicores, "at max size", ratio, "max")
    if ratio <= shrink_below or (cost_over_target and ratio < grow_above):
        new = max(min_millicores, int(round(cpu_millicores * shrink_step)))
        if new < cpu_millicores:
            return ResizeDecision("shrink", new, f"p99/SLO={ratio:.2f}, cost_over_target={cost_over_target}", ratio)
        return ResizeDecision("hold", cpu_millicores, "at min size", ratio, "min")
    return ResizeDecision("hold", cpu_millicores, f"p99/SLO={ratio:.2f} within band", ratio)


def recommend_memory(
    mem_mib: int,
    usage_mib: float,
    *,
    min_mib: int = 64,
    max_mib: int = 16384,
    grow_above: float = 0.85,
    shrink_below: float = 0.5,
    grow_step: float = 1.25,
    shrink_step: float = 0.9,
    headroom: float = 1.3,
) -> ResizeDecision:
    """Memory is OOM-sensitive, so it is asymmetric: grow fast, shrink slowly.

    Shrinks move at most 10% per decision and never below ``usage * headroom``.
    The ``cpu_millicores`` field of the returned decision carries MiB.
    """
    if mem_mib <= 0 or usage_mib < 0:
        raise ValueError("memory request must be positive and usage non-negative")
    util = usage_mib / mem_mib
    if util >= grow_above:
        new = min(max_mib, int(math.ceil(mem_mib * grow_step)))
        if new > mem_mib:
            return ResizeDecision("grow", new, f"memory usage {util:.0%} >= {grow_above:.0%}", util)
        return ResizeDecision("hold", mem_mib, "memory at max size", util, "max")
    if util <= shrink_below:
        floor = max(min_mib, int(math.ceil(usage_mib * headroom)))
        new = max(floor, int(round(mem_mib * shrink_step)))
        if new < mem_mib:
            return ResizeDecision("shrink", new, f"memory usage {util:.0%} <= {shrink_below:.0%}", util)
        return ResizeDecision("hold", mem_mib, "memory at floor", util, "min")
    return ResizeDecision("hold", mem_mib, f"memory usage {util:.0%} within band", util)


def recommend_replicas(
    replicas: int,
    min_replicas: int,
    max_replicas: int,
    cpu: ResizeDecision,
    *,
    cpu_usage_millicores: float | None = None,
    cpu_millicores: int = 0,
    configured_min_millicores: int = 0,
    target_utilization: float = 0.7,
) -> tuple[int, str]:
    """Horizontal step, only once vertical room is exhausted.

    +1 replica when p99 is near the SLO and CPU is already at its maximum.
    -1 replica when latency has slack, CPU is at its configured minimum and the
    remaining pods can absorb the load below ``target_utilization``.
    """
    if min_replicas < 1 or max_replicas < min_replicas:
        raise ValueError("require 1 <= min_replicas <= max_replicas")
    if cpu.limit == "max" and cpu.action == "hold" and replicas < max_replicas and cpu.ratio >= 0.9:
        return replicas + 1, f"cpu at max and p99/SLO={cpu.ratio:.2f}: scale out"
    if (replicas > min_replicas and cpu.ratio <= 0.5 and configured_min_millicores
            and cpu_millicores <= configured_min_millicores):
        if cpu_usage_millicores is None:
            return replicas, "no cpu usage data: not scaling in"
        projected = cpu_usage_millicores * replicas / (replicas - 1)
        if projected <= cpu_millicores * target_utilization:
            return replicas - 1, "cpu at minimum with latency slack: scale in"
    return replicas, "replicas unchanged"


@dataclass(frozen=True)
class ArbitrationResult:
    strategy: str  # "SCALE_HORIZONTALLY" | "RESIZE_VERTICALLY" | "COORDINATED_BOTH" | "NO_OP" | "WAIT_MORE_INFO"
    recommended_replicas: int
    recommended_cpu_millicores: int
    recommended_memory_mib: int
    expected_hourly_cost_delta_usd: float
    risk_score: float
    confidence: float
    conflict_detected: bool
    justification: str


def arbitrate_scaling_strategy(
    *,
    current_replicas: int,
    min_replicas: int,
    max_replicas: int,
    current_cpu_millicores: int,
    cpu_usage_millicores: float,
    current_memory_mib: int,
    memory_usage_mib: float,
    p99_ms: float,
    slo_p99_ms: float,
    current_rps: float = 0.0,
    baseline_rps: float = 0.0,
    has_native_hpa: bool = False,
    has_native_vpa: bool = False,
    sample_count: int = 5,
    min_cpu_millicores: int = 100,
    max_cpu_millicores: int = 8000,
    min_memory_mib: int = 128,
    max_memory_mib: int = 16384,
) -> ArbitrationResult:
    """Arbitrates between horizontal replica scaling and vertical in-place resizing."""
    if sample_count < 3 or slo_p99_ms <= 0:
        return ArbitrationResult(
            strategy="WAIT_MORE_INFO",
            recommended_replicas=current_replicas,
            recommended_cpu_millicores=current_cpu_millicores,
            recommended_memory_mib=current_memory_mib,
            expected_hourly_cost_delta_usd=0.0,
            risk_score=0.15,
            confidence=0.35,
            conflict_detected=has_native_hpa or has_native_vpa,
            justification="Insufficient telemetry samples or invalid SLO target.",
        )

    slo_ratio = p99_ms / slo_p99_ms
    cpu_util = cpu_usage_millicores / max(1, current_cpu_millicores)
    mem_util = memory_usage_mib / max(1, current_memory_mib)
    rps_ratio = (current_rps / baseline_rps) if baseline_rps > 0 else 1.0

    def _hourly_cost(r: int, cpu_m: int, mem_m: int) -> float:
        return r * ((cpu_m / 1000.0) * 0.031611 + (mem_m / 1024.0) * 0.004237)

    base_cost = _hourly_cost(current_replicas, current_cpu_millicores, current_memory_mib)

    # Situación A: Traffic surge + p99 degradation -> Horizontal Scale-Out
    if (rps_ratio >= 1.20 and slo_ratio >= 0.85) or (slo_ratio >= 1.05 and current_replicas < max_replicas):
        if not has_native_hpa and current_replicas < max_replicas:
            target_replicas = min(max_replicas, max(current_replicas + 1, int(math.ceil(current_replicas * min(1.5, max(rps_ratio, slo_ratio))))))
            if not has_native_vpa and (cpu_util >= 0.92 or mem_util >= 0.88):
                target_cpu = min(max_cpu_millicores, int(round(current_cpu_millicores * 1.20)))
                target_mem = min(max_memory_mib, int(round(current_memory_mib * 1.25))) if mem_util >= 0.88 else current_memory_mib
                return ArbitrationResult(
                    strategy="COORDINATED_BOTH",
                    recommended_replicas=target_replicas,
                    recommended_cpu_millicores=target_cpu,
                    recommended_memory_mib=target_mem,
                    expected_hourly_cost_delta_usd=round(_hourly_cost(target_replicas, target_cpu, target_mem) - base_cost, 4),
                    risk_score=0.75,
                    confidence=0.88,
                    conflict_detected=has_native_hpa or has_native_vpa,
                    justification=f"Situación A (Coordinated): Traffic surge ({rps_ratio:.2f}x) + SLO ratio {slo_ratio:.2f} + high pod saturation.",
                )
            return ArbitrationResult(
                strategy="SCALE_HORIZONTALLY",
                recommended_replicas=target_replicas,
                recommended_cpu_millicores=current_cpu_millicores,
                recommended_memory_mib=current_memory_mib,
                expected_hourly_cost_delta_usd=round(_hourly_cost(target_replicas, current_cpu_millicores, current_memory_mib) - base_cost, 4),
                risk_score=0.55,
                confidence=0.90,
                conflict_detected=has_native_hpa or has_native_vpa,
                justification=f"Situación A: Sustained traffic surge ({rps_ratio:.2f}x) with p99/SLO={slo_ratio:.2f}; scaling horizontally.",
            )
        elif not has_native_vpa:
            target_cpu = min(max_cpu_millicores, int(round(current_cpu_millicores * 1.25)))
            return ArbitrationResult(
                strategy="RESIZE_VERTICALLY",
                recommended_replicas=current_replicas,
                recommended_cpu_millicores=target_cpu,
                recommended_memory_mib=current_memory_mib,
                expected_hourly_cost_delta_usd=round(_hourly_cost(current_replicas, target_cpu, current_memory_mib) - base_cost, 4),
                risk_score=0.60,
                confidence=0.82,
                conflict_detected=has_native_hpa or has_native_vpa,
                justification="Horizontal scaling owned by native HPA or at max_replicas; assisting via in-place vertical resize.",
            )

    # Situación B: Comfortable latency + low per-instance resource utilization -> Vertical Rightsize Down
    if slo_ratio <= 0.55 and cpu_util <= 0.50 and not has_native_vpa:
        safe_cpu_floor = max(min_cpu_millicores, int(math.ceil(cpu_usage_millicores / 0.70)))
        target_cpu = max(safe_cpu_floor, int(round(current_cpu_millicores * 0.80)))
        target_mem = current_memory_mib
        if mem_util <= 0.45:
            safe_mem_floor = max(min_memory_mib, int(math.ceil(memory_usage_mib * 1.30)))
            target_mem = max(safe_mem_floor, int(round(current_memory_mib * 0.90)))
        if target_cpu < current_cpu_millicores or target_mem < current_memory_mib:
            return ArbitrationResult(
                strategy="RESIZE_VERTICALLY",
                recommended_replicas=current_replicas,
                recommended_cpu_millicores=target_cpu,
                recommended_memory_mib=target_mem,
                expected_hourly_cost_delta_usd=round(_hourly_cost(current_replicas, target_cpu, target_mem) - base_cost, 4),
                risk_score=0.20,
                confidence=0.92,
                conflict_detected=has_native_hpa or has_native_vpa,
                justification=f"Situación B: Comfortable latency (p99/SLO={slo_ratio:.2f}) and low CPU util ({cpu_util:.0%}); vertical rightsize down preserving {current_replicas} replicas.",
            )

    return ArbitrationResult(
        strategy="NO_OP",
        recommended_replicas=current_replicas,
        recommended_cpu_millicores=current_cpu_millicores,
        recommended_memory_mib=current_memory_mib,
        expected_hourly_cost_delta_usd=0.0,
        risk_score=0.10,
        confidence=0.95,
        conflict_detected=has_native_hpa or has_native_vpa,
        justification="Workload operating within target SLO and efficiency band.",
    )

