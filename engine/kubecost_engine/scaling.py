"""SLO-aware in-place resize recommendations."""
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
) -> ResizeDecision:
    """Shrink when p99 is well under the SLO, grow when it nears it, else hold."""
    if slo_p99_ms <= 0:
        raise ValueError("slo_p99_ms must be positive")
    ratio = p99_ms / slo_p99_ms
    if ratio >= grow_above:
        new = min(max_millicores, int(round(cpu_millicores * grow_step)))
        if new > cpu_millicores:
            return ResizeDecision("grow", new, f"p99/SLO={ratio:.2f} >= {grow_above}")
        return ResizeDecision("hold", cpu_millicores, "at max size")
    if ratio <= shrink_below:
        new = max(min_millicores, int(round(cpu_millicores * shrink_step)))
        if new < cpu_millicores:
            return ResizeDecision("shrink", new, f"p99/SLO={ratio:.2f} <= {shrink_below}")
        return ResizeDecision("hold", cpu_millicores, "at min size")
    return ResizeDecision("hold", cpu_millicores, f"p99/SLO={ratio:.2f} within band")
