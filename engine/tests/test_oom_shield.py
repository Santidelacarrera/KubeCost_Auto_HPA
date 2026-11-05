import pytest
from kubecost_engine.oom_shield import OOMProtectionShield


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
    assert dec.action == "inflate_limit"
    assert dec.recommended_limit_mib > 4096
    assert dec.recommended_request_mib > 2048
    assert "CRITICAL OOM SHIELD" in dec.reason


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
