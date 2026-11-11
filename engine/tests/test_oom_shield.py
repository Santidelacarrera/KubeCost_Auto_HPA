import pytest
from kubecost_engine.oom_shield import OOMProtectionShield, PROGRESSIVE_STAGES


def test_oom_shield_healthy_state():
    shield = OOMProtectionShield()
    dec = shield.evaluate_memory_risk(
        current_usage_mib=1200.0,
        current_request_mib=2048,
        current_limit_mib=4096,
        usage_history_mib=[1180.0, 1190.0, 1200.0],
    )
    assert not dec.is_at_risk
    assert dec.risk_level == "NONE"
    assert dec.progressive_stage == "NORMAL"
    assert dec.action == "hold"


def test_oom_shield_critical_inflation():
    shield = OOMProtectionShield()
    # Memory at 3700 MiB out of 4096 MiB limit (90.3% utilization) with fast climb
    dec = shield.evaluate_memory_risk(
        current_usage_mib=3700.0,
        current_request_mib=2048,
        current_limit_mib=4096,
        usage_history_mib=[3100.0, 3400.0, 3700.0],
        max_allowed_limit_mib=8192,
    )
    assert dec.is_at_risk is True
    assert dec.risk_level == "CRITICAL"
    assert dec.progressive_stage in ("CRITICAL", "EMERGENCY")
    assert dec.action == "inflate_limit"
    assert dec.recommended_limit_mib > 4096
    assert dec.recommended_request_mib > 2048
    assert "CRITICAL OOM SHIELD" in dec.reason
    assert dec.dmem_dt_mib_per_sec > 0
    assert dec.ema_dmem_dt_mib_per_sec > 0


def test_oom_shield_overprovisioned_rightsize():
    shield = OOMProtectionShield()
    # Usage is stably 300 MiB out of 2048 MiB requested
    dec = shield.evaluate_memory_risk(
        current_usage_mib=300.0,
        current_request_mib=2048,
        current_limit_mib=4096,
        usage_history_mib=[300.0, 300.0, 300.0],
    )
    assert not dec.is_at_risk
    assert dec.action == "rightsize_down"
    assert dec.recommended_request_mib < 1000


def test_oom_shield_progressive_stages_and_node_capacity_fallback():
    shield = OOMProtectionShield()

    # WATCH stage: ~70% usage, stable
    watch_dec = shield.evaluate_memory_risk(
        current_usage_mib=2900.0,
        current_request_mib=2048,
        current_limit_mib=4096,
        usage_history_mib=[2890.0, 2895.0, 2900.0],
    )
    assert watch_dec.progressive_stage == "WATCH"
    assert watch_dec.action == "hold"

    # WARNING stage: ~80% usage
    warn_dec = shield.evaluate_memory_risk(
        current_usage_mib=3300.0,
        current_request_mib=2048,
        current_limit_mib=4096,
        usage_history_mib=[3200.0, 3250.0, 3300.0],
    )
    assert warn_dec.progressive_stage == "WARNING"
    assert warn_dec.is_at_risk is True
    assert warn_dec.action == "inflate_limit"

    # EMERGENCY stage with constrained node capacity -> horizontal fallback
    emerg_dec = shield.evaluate_memory_risk(
        current_usage_mib=3950.0,
        current_request_mib=2048,
        current_limit_mib=4096,
        usage_history_mib=[2500.0, 3200.0, 3950.0],
        node_available_mib=128,  # Insufficient node memory for full vertical expansion
    )
    assert emerg_dec.progressive_stage == "EMERGENCY"
    assert emerg_dec.requires_horizontal_fallback is True
    assert emerg_dec.action == "horizontal_scale_out_fallback"
    assert emerg_dec.recommended_limit_mib == 4096 + 128
