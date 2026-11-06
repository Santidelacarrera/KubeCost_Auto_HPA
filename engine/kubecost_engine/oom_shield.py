"""OOM Protection & Memory Headroom Shield Module.

Prevents and mitigates container crashes (OOMKilled exit code 137) by calculating
smoothed memory consumption velocity (dMem/dt = ΔMemory / ΔTime), acceleration,
estimating time-to-exhaustion with confidence margins, and progressively adjusting
cgroups memory requests/limits or coordinating horizontal fallback when node capacity
is insufficient.

Note: Sub-second instantaneous allocation spikes cannot be intercepted with 100%
certainty by user-space control loops; this engine provides probabilistic early
mitigation and progressive headroom expansion.
"""

from dataclasses import dataclass
from typing import List, Optional


PROGRESSIVE_STAGES = ("NORMAL", "WATCH", "WARNING", "CRITICAL", "EMERGENCY")


@dataclass(frozen=True)
class OOMShieldDecision:
    is_at_risk: bool
    risk_level: str  # Legacy compatibility: "NONE", "LOW", "ELEVATED", "CRITICAL", "EMERGENCY"
    current_usage_mib: float
    current_limit_mib: int
    recommended_limit_mib: int
    recommended_request_mib: int
    headroom_percentage: float
    estimated_seconds_to_oom: Optional[float]
    action: str  # "hold", "inflate_limit", "rightsize_down", "horizontal_scale_out_fallback"
    reason: str
    # Extended Fase 2 telemetry & progressive risk fields (with defaults for backward compatibility)
    progressive_stage: str = "NORMAL"  # "NORMAL", "WATCH", "WARNING", "CRITICAL", "EMERGENCY"
    risk_score: float = 0.0  # 0.0 to 1.0
    dmem_dt_mib_per_sec: float = 0.0
    ema_dmem_dt_mib_per_sec: float = 0.0
    acceleration_mib_per_sec2: float = 0.0
    working_set_mib: float = 0.0
    rss_mib: float = 0.0
    requires_horizontal_fallback: bool = False
    confidence: float = 0.85


class OOMProtectionShield:
    """Detects sustained or accelerating heap growth and mitigates OOM termination risk."""

    def __init__(
        self,
        emergency_threshold: float = 0.94,
        critical_threshold: float = 0.88,
        warning_threshold: float = 0.78,
        watch_threshold: float = 0.68,
        min_headroom_mib: int = 256,
        headroom_multiplier: float = 1.25,
        emergency_multiplier: float = 1.40,
        ema_alpha: float = 0.45,
        sampling_interval_seconds: float = 30.0,
        safety_margin_factor: float = 0.85,
    ):
        self.emergency_threshold = emergency_threshold
        self.critical_threshold = critical_threshold
        self.warning_threshold = warning_threshold
        self.watch_threshold = watch_threshold
        self.min_headroom_mib = min_headroom_mib
        self.headroom_multiplier = headroom_multiplier
        self.emergency_multiplier = emergency_multiplier
        self.ema_alpha = max(0.05, min(1.0, ema_alpha))
        self.sampling_interval_seconds = max(1.0, sampling_interval_seconds)
        self.safety_margin_factor = max(0.5, min(1.0, safety_margin_factor))

    def compute_memory_kinematics(
        self,
        usage_history_mib: Optional[List[float]],
        sampling_interval_seconds: Optional[float] = None,
    ) -> tuple[float, float, float]:
        """Computes instantaneous dMem/dt (MiB/s), EMA-smoothed dMem/dt (MiB/s), and acceleration (MiB/s^2)."""
        dt = sampling_interval_seconds if sampling_interval_seconds and sampling_interval_seconds > 0 else self.sampling_interval_seconds
        if not usage_history_mib or len(usage_history_mib) < 2:
            return 0.0, 0.0, 0.0

        velocities: List[float] = []
        for i in range(1, len(usage_history_mib)):
            delta = usage_history_mib[i] - usage_history_mib[i - 1]
            velocities.append(delta / dt)

        inst_velocity = velocities[-1]
        ema_velocity = velocities[0]
        for v in velocities[1:]:
            ema_velocity = self.ema_alpha * v + (1.0 - self.ema_alpha) * ema_velocity

        acceleration = 0.0
        if len(velocities) >= 2:
            acceleration = (velocities[-1] - velocities[-2]) / dt

        return round(inst_velocity, 4), round(ema_velocity, 4), round(acceleration, 6)

    def evaluate_memory_risk(
        self,
        current_usage_mib: float,
        current_request_mib: int,
        current_limit_mib: int,
        usage_history_mib: Optional[List[float]] = None,
        max_allowed_limit_mib: int = 16384,
        working_set_mib: Optional[float] = None,
        rss_mib: Optional[float] = None,
        sampling_interval_seconds: Optional[float] = None,
        node_available_mib: Optional[int] = None,
    ) -> OOMShieldDecision:
        """Evaluates OOM hazard using working-set/RSS, EMA dMem/dt, and time-to-exhaustion."""
        if current_limit_mib <= 0:
            current_limit_mib = max(current_request_mib, 512)

        dt = sampling_interval_seconds if sampling_interval_seconds and sampling_interval_seconds > 0 else self.sampling_interval_seconds
        eff_working_set = working_set_mib if working_set_mib is not None else current_usage_mib
        eff_rss = rss_mib if rss_mib is not None else current_usage_mib
        # Kubernetes OOM killer tracks working_set_bytes primarily, but RSS spikes also pressure cgroups
        effective_usage_mib = max(current_usage_mib, eff_working_set, eff_rss * 0.95)

        ratio = effective_usage_mib / current_limit_mib
        headroom_pct = max(0.0, (1.0 - ratio) * 100.0)

        inst_v, ema_v, accel = self.compute_memory_kinematics(usage_history_mib, dt)
        # Use conservative positive velocity between EMA and instantaneous when accelerating
        effective_v_mib_per_sec = max(ema_v, inst_v if accel > 0 else ema_v)

        est_seconds_to_oom: Optional[float] = None
        if effective_v_mib_per_sec > 0.01:
            remaining_mib = max(0.0, current_limit_mib - effective_usage_mib)
            raw_ttf = remaining_mib / effective_v_mib_per_sec
            est_seconds_to_oom = round(raw_ttf * self.safety_margin_factor, 1)

        velocity_mib_per_interval = effective_v_mib_per_sec * dt

        # Calculate composite risk score [0.0, 1.0]
        utilization_score = min(1.0, max(0.0, (ratio - 0.40) / 0.55))
        ttf_score = 0.0
        if est_seconds_to_oom is not None:
            if est_seconds_to_oom <= 60.0:
                ttf_score = 1.0
            elif est_seconds_to_oom <= 180.0:
                ttf_score = 0.85
            elif est_seconds_to_oom <= 360.0:
                ttf_score = 0.60
            elif est_seconds_to_oom <= 600.0:
                ttf_score = 0.35
        risk_score = round(min(1.0, max(utilization_score, ttf_score)), 3)

        # Helper to check node capacity constraints before applying vertical inflation
        def _apply_node_capacity_guard(target_limit: int, target_req: int) -> tuple[int, int, bool, str]:
            needed_delta = max(0, target_limit - current_limit_mib)
            if node_available_mib is not None and needed_delta > node_available_mib:
                capped_limit = current_limit_mib + max(0, node_available_mib)
                capped_req = min(target_req, capped_limit)
                return capped_limit, capped_req, True, "horizontal_scale_out_fallback"
            return target_limit, target_req, False, "inflate_limit"

        # 1. EMERGENCY / CRITICAL OOM Risk condition
        is_emergency = ratio >= self.emergency_threshold or (
            est_seconds_to_oom is not None and est_seconds_to_oom < 45.0 and ratio >= 0.75
        )
        is_critical = ratio >= self.critical_threshold or (
            est_seconds_to_oom is not None and est_seconds_to_oom < 120.0
        )

        if is_emergency or is_critical:
            prog_stage = "EMERGENCY" if is_emergency else "CRITICAL"
            # Legacy risk_level stays "CRITICAL" for backward compatibility with existing tests
            legacy_risk = "CRITICAL"
            mult = self.emergency_multiplier if is_emergency else self.headroom_multiplier
            raw_new_limit = min(
                max_allowed_limit_mib,
                int(max(current_limit_mib * mult, effective_usage_mib + self.min_headroom_mib)),
            )
            raw_new_req = min(raw_new_limit, int(max(current_request_mib, effective_usage_mib * 1.10)))
            new_limit, new_request, horiz_fallback, action_name = _apply_node_capacity_guard(
                raw_new_limit, raw_new_req
            )
            fallback_note = (
                " Node capacity constrained; triggered horizontal scale-out fallback."
                if horiz_fallback
                else ""
            )
            return OOMShieldDecision(
                is_at_risk=True,
                risk_level=legacy_risk,
                current_usage_mib=current_usage_mib,
                current_limit_mib=current_limit_mib,
                recommended_limit_mib=new_limit,
                recommended_request_mib=new_request,
                headroom_percentage=headroom_pct,
                estimated_seconds_to_oom=est_seconds_to_oom,
                action=action_name,
                reason=(
                    f"CRITICAL OOM SHIELD ({prog_stage}): Memory at {ratio*100:.1f}% of limit with "
                    f"+{velocity_mib_per_interval:.1f}MiB velocity ({effective_v_mib_per_sec:.2f}MiB/s). "
                    f"Inflated limit to {new_limit}MiB.{fallback_note}"
                ),
                progressive_stage=prog_stage,
                risk_score=max(0.85, risk_score),
                dmem_dt_mib_per_sec=inst_v,
                ema_dmem_dt_mib_per_sec=ema_v,
                acceleration_mib_per_sec2=accel,
                working_set_mib=eff_working_set,
                rss_mib=eff_rss,
                requires_horizontal_fallback=horiz_fallback,
                confidence=0.90 if usage_history_mib and len(usage_history_mib) >= 3 else 0.75,
            )

        # 2. WARNING (Elevated) Risk condition
        if ratio >= self.warning_threshold or (est_seconds_to_oom is not None and est_seconds_to_oom < 300.0):
            raw_new_limit = min(
                max_allowed_limit_mib,
                int(max(current_limit_mib * 1.15, effective_usage_mib + self.min_headroom_mib)),
            )
            new_limit, new_request, horiz_fallback, action_name = _apply_node_capacity_guard(
                raw_new_limit, current_request_mib
            )
            return OOMShieldDecision(
                is_at_risk=True,
                risk_level="ELEVATED",
                current_usage_mib=current_usage_mib,
                current_limit_mib=current_limit_mib,
                recommended_limit_mib=new_limit,
                recommended_request_mib=new_request,
                headroom_percentage=headroom_pct,
                estimated_seconds_to_oom=est_seconds_to_oom,
                action=action_name,
                reason=(
                    f"ELEVATED OOM RISK (WARNING): Headroom dropped to {headroom_pct:.1f}%. "
                    f"Adjusting limit to {new_limit}MiB for safety."
                ),
                progressive_stage="WARNING",
                risk_score=max(0.65, risk_score),
                dmem_dt_mib_per_sec=inst_v,
                ema_dmem_dt_mib_per_sec=ema_v,
                acceleration_mib_per_sec2=accel,
                working_set_mib=eff_working_set,
                rss_mib=eff_rss,
                requires_horizontal_fallback=horiz_fallback,
                confidence=0.85,
            )

        # 3. WATCH Stage: early upward slope or moderate utilization >= watch_threshold
        if ratio >= self.watch_threshold or (est_seconds_to_oom is not None and est_seconds_to_oom < 600.0):
            return OOMShieldDecision(
                is_at_risk=False,
                risk_level="LOW",
                current_usage_mib=current_usage_mib,
                current_limit_mib=current_limit_mib,
                recommended_limit_mib=current_limit_mib,
                recommended_request_mib=current_request_mib,
                headroom_percentage=headroom_pct,
                estimated_seconds_to_oom=est_seconds_to_oom,
                action="hold",
                reason=(
                    f"WATCH: Memory utilization at {ratio*100:.1f}% with dMem/dt={ema_v:.2f}MiB/s. "
                    f"Monitoring closely before limit mutation."
                ),
                progressive_stage="WATCH",
                risk_score=max(0.35, risk_score),
                dmem_dt_mib_per_sec=inst_v,
                ema_dmem_dt_mib_per_sec=ema_v,
                acceleration_mib_per_sec2=accel,
                working_set_mib=eff_working_set,
                rss_mib=eff_rss,
                requires_horizontal_fallback=False,
                confidence=0.85,
            )

        # 4. Overprovisioned Workload: Usage is safely low (< 45% of limit) with non-positive velocity
        if ratio < 0.45 and velocity_mib_per_interval <= 0 and current_request_mib > 512:
            safe_request = max(256, int(effective_usage_mib * 1.30))
            safe_limit = max(safe_request * 2, int(effective_usage_mib * 1.60))
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
                    f"OVERPROVISIONED: Workload consumes only {effective_usage_mib:.1f}MiB of {current_request_mib}MiB requested. "
                    f"Rightsizing request to {safe_request}MiB saves cost with controlled risk."
                ),
                progressive_stage="NORMAL",
                risk_score=risk_score,
                dmem_dt_mib_per_sec=inst_v,
                ema_dmem_dt_mib_per_sec=ema_v,
                acceleration_mib_per_sec2=accel,
                working_set_mib=eff_working_set,
                rss_mib=eff_rss,
                requires_horizontal_fallback=False,
                confidence=0.90,
            )

        # 5. Steady State Normal
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
            progressive_stage="NORMAL",
            risk_score=risk_score,
            dmem_dt_mib_per_sec=inst_v,
            ema_dmem_dt_mib_per_sec=ema_v,
            acceleration_mib_per_sec2=accel,
            working_set_mib=eff_working_set,
            rss_mib=eff_rss,
            requires_horizontal_fallback=False,
            confidence=0.90,
        )
