"""SLO-aware in-place resize recommendations."""
import math
from dataclasses import dataclass


@dataclass(frozen=True)
class ResizeDecision:
    action: str  # "shrink" | "grow" | "hold"
    cpu_millicores: int
    reason: str


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
            return ResizeDecision("grow", new, f"p99/SLO={ratio:.2f} >= {grow_above}")
        return ResizeDecision("hold", cpu_millicores, "at max size")
    if ratio <= shrink_below or (cost_over_target and ratio < grow_above):
        new = max(min_millicores, int(round(cpu_millicores * shrink_step)))
        if new < cpu_millicores:
            return ResizeDecision("shrink", new, f"p99/SLO={ratio:.2f}, cost_over_target={cost_over_target}")
        return ResizeDecision("hold", cpu_millicores, "at min size")
    return ResizeDecision("hold", cpu_millicores, f"p99/SLO={ratio:.2f} within band")
