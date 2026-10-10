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
