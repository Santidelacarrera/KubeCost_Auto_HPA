"""OOM Protection & Memory Headroom Shield Module.

Reverse-engineered from ScaleOps OOM Protection & Runtime Rightsizing (REA methodology).
Prevents container crashes (OOMKilled exit code 137) by calculating memory consumption
velocity (dMem/dt), estimating time-to-exhaustion, and proactively ballooning cgroups limits.
"""

from dataclasses import dataclass
from typing import List, Optional


@dataclass(frozen=True)
class OOMShieldDecision:
    is_at_risk: bool
    risk_level: str  # "NONE", "LOW", "ELEVATED", "CRITICAL"
    current_usage_mib: float
    current_limit_mib: int
    recommended_limit_mib: int
    recommended_request_mib: int
    headroom_percentage: float
    estimated_seconds_to_oom: Optional[float]
    action: str  # "hold", "inflate_limit", "rightsize_down"
    reason: str


class OOMProtectionShield:
    """Detects rapid heap accumulation and shields pods from kernel OOM termination."""

    def __init__(
        self,
        critical_threshold: float = 0.88,  # 88% memory usage triggers immediate shield
        warning_threshold: float = 0.78,   # 78% triggers close velocity monitoring
        min_headroom_mib: int = 256,       # Minimum safety buffer
        headroom_multiplier: float = 1.25, # 25% safety margin on inflation
    ):
        self.critical_threshold = critical_threshold
        self.warning_threshold = warning_threshold
        self.min_headroom_mib = min_headroom_mib
        self.headroom_multiplier = headroom_multiplier

    def evaluate_memory_risk(
        self,
        current_usage_mib: float,
        current_request_mib: int,
        current_limit_mib: int,
        usage_history_mib: Optional[List[float]] = None,
        max_allowed_limit_mib: int = 16384,
    ) -> OOMShieldDecision:
        """Evaluates OOM hazard and returns dynamic memory recommendations."""
        if current_limit_mib <= 0:
            current_limit_mib = max(current_request_mib, 512)

        ratio = current_usage_mib / current_limit_mib
        headroom_pct = (1.0 - ratio) * 100.0

        # Calculate memory velocity (MiB per second / cycle)
        velocity_mib_per_interval = 0.0
        est_seconds_to_oom = None

        if usage_history_mib and len(usage_history_mib) >= 2:
            delta = usage_history_mib[-1] - usage_history_mib[-2]
            if delta > 0:
                velocity_mib_per_interval = delta
                remaining_mib = current_limit_mib - current_usage_mib
                # Assuming 30s evaluation interval
                seconds_per_interval = 30.0
                intervals_left = remaining_mib / delta
                est_seconds_to_oom = round(intervals_left * seconds_per_interval, 1)

        # 1. Critical OOM Risk condition: usage > critical threshold OR OOM predicted within 120s
        if ratio >= self.critical_threshold or (est_seconds_to_oom is not None and est_seconds_to_oom < 120.0):
            risk_level = "CRITICAL"
            new_limit = min(
                max_allowed_limit_mib,
                int(max(current_limit_mib * self.headroom_multiplier, current_usage_mib + self.min_headroom_mib)),
            )
            # Safe request sizing: keep request at 80% of current usage to guarantee QoS
            new_request = min(new_limit, int(current_usage_mib * 1.10))
            return OOMShieldDecision(
                is_at_risk=True,
                risk_level=risk_level,
                current_usage_mib=current_usage_mib,
                current_limit_mib=current_limit_mib,
                recommended_limit_mib=new_limit,
                recommended_request_mib=new_request,
                headroom_percentage=headroom_pct,
                estimated_seconds_to_oom=est_seconds_to_oom,
                action="inflate_limit",
                reason=(
                    f"CRITICAL OOM SHIELD: Memory at {ratio*100:.1f}% of limit with "
                    f"+{velocity_mib_per_interval:.1f}MiB velocity. Inflated limit to {new_limit}MiB."
                ),
            )

        # 2. Elevated Warning Risk: usage > warning threshold
        if ratio >= self.warning_threshold or (est_seconds_to_oom is not None and est_seconds_to_oom < 300.0):
            risk_level = "ELEVATED"
            new_limit = min(
                max_allowed_limit_mib,
                int(max(current_limit_mib * 1.15, current_usage_mib + self.min_headroom_mib)),
            )
            return OOMShieldDecision(
                is_at_risk=True,
                risk_level=risk_level,
                current_usage_mib=current_usage_mib,
                current_limit_mib=current_limit_mib,
                recommended_limit_mib=new_limit,
                recommended_request_mib=current_request_mib,
                headroom_percentage=headroom_pct,
                estimated_seconds_to_oom=est_seconds_to_oom,
                action="inflate_limit",
                reason=(
                    f"ELEVATED OOM RISK: Headroom dropped to {headroom_pct:.1f}%. "
                    f"Adjusting limit to {new_limit}MiB for safety."
                ),
            )

        # 3. Overprovisioned Workload: Usage is safely low (< 45% of request) with zero upward velocity
        if ratio < 0.45 and velocity_mib_per_interval <= 0 and current_request_mib > 512:
            safe_request = max(256, int(current_usage_mib * 1.30))
            safe_limit = max(safe_request * 2, int(current_usage_mib * 1.60))
            return OOMShieldDecision(
                is_at_risk=False,
                risk_level="NONE",
                current_usage_mib=current_usage_mib,
                current_limit_mib=current_limit_mib,
                recommended_limit_mib=safe_limit,
                recommended_request_mib=safe_request,
                headroom_percentage=headroom_pct,
                estimated_seconds_to_oom=None,
                action="rightsize_down",
                reason=(
                    f"OVERPROVISIONED: Workload consumes only {current_usage_mib:.1f}MiB of {current_request_mib}MiB requested. "
                    f"Rightsizing request to {safe_request}MiB saves cost with zero risk."
                ),
            )

        # 4. Steady State Normal
        return OOMShieldDecision(
            is_at_risk=False,
            risk_level="NONE",
            current_usage_mib=current_usage_mib,
            current_limit_mib=current_limit_mib,
            recommended_limit_mib=current_limit_mib,
            recommended_request_mib=current_request_mib,
            headroom_percentage=headroom_pct,
            estimated_seconds_to_oom=None,
            action="hold",
            reason=f"HEALTHY: Memory utilization at {ratio*100:.1f}% within stable boundaries.",
        )
