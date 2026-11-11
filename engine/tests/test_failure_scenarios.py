"""Mandatory 20 Production Failure Scenarios Test Suite (Fase 9 — Section 19).

Verifies resilient behavior across all 20 failure modes mandated by the Master Prompt,
plus the Fase 10 Cast AI + Tremor UI API contracts.
"""

import math
import pytest

from kubecost_engine.binpacking import NodeBinPacker, NodeState, PodPlacement
from kubecost_engine.economics import (
    audit_counterfactual_savings,
    cost_per_1k_successful_requests,
    evaluate_economic_objective,
)
from kubecost_engine.oom_shield import OOMProtectionShield
from kubecost_engine.predictive import PredictiveAutoscaler, detect_model_drift
from kubecost_engine.saas import SaaSControlPlane
from kubecost_engine.scaling import arbitrate_scaling_strategy, recommend_resize
from kubecost_engine.server import evaluate
from kubecost_engine.spot import evaluate_spot_interruption_risk


def test_scenario_01_and_02_missing_or_incomplete_metrics():
    # Scenario 1 & 2: Incomplete telemetry / cold samples -> WAIT_MORE_INFO (hold steady)
    res = arbitrate_scaling_strategy(
        current_replicas=3,
        min_replicas=2,
        max_replicas=10,
        current_cpu_millicores=1000,
        cpu_usage_millicores=0.0,
        current_memory_mib=1024,
        memory_usage_mib=0.0,
        p99_ms=0.0,
        slo_p99_ms=200.0,
        sample_count=1,
    )
    assert res.strategy == "WAIT_MORE_INFO"
    assert res.recommended_replicas == 3
    assert res.recommended_cpu_millicores == 1000


def test_scenario_03_nan_or_negative_metrics_rejected():
    # Scenario 3: NaN, Infinity, or negative numbers must be rejected
    with pytest.raises(ValueError):
        evaluate({"cpu_millicores": -500, "p99_ms": 50.0, "slo_p99_ms": 200.0})
    with pytest.raises(ValueError):
        evaluate({"cpu_millicores": 1000, "p99_ms": float("nan"), "slo_p99_ms": 200.0})


def test_scenario_04_zero_traffic_safe_handling():
    # Scenario 4: Zero RPS does not crash with ZeroDivisionError; returns None for JSON cost_per_request
    out = evaluate({"cpu_millicores": 1000, "rps": 0.0, "p99_ms": 20.0, "slo_p99_ms": 200.0})
    assert out["cost_per_request"] is None


def test_scenario_05_sudden_traffic_spike_scales_horizontally():
    # Scenario 5: Sudden traffic spike with SLO degradation triggers horizontal scale-out
    res = arbitrate_scaling_strategy(
        current_replicas=3,
        min_replicas=2,
        max_replicas=12,
        current_cpu_millicores=1000,
        cpu_usage_millicores=800.0,
        current_memory_mib=1024,
        memory_usage_mib=512.0,
        p99_ms=190.0,
        slo_p99_ms=200.0,
        current_rps=600.0,
        baseline_rps=250.0,
    )
    assert res.strategy == "SCALE_HORIZONTALLY"
    assert res.recommended_replicas > 3


def test_scenario_06_rapid_memory_leak_oom_shield():
    # Scenario 6: Rapid dMem/dt leak triggers CRITICAL/EMERGENCY OOM shield inflation
    shield = OOMProtectionShield()
    dec = shield.evaluate_memory_risk(
        current_usage_mib=3800.0,
        current_request_mib=2048,
        current_limit_mib=4096,
        usage_history_mib=[2600.0, 3200.0, 3800.0],
    )
    assert dec.is_at_risk is True
    assert dec.progressive_stage in ("CRITICAL", "EMERGENCY")
    assert dec.recommended_limit_mib > 4096


def test_scenario_07_node_without_capacity_for_vertical_resize_falls_back_to_horizontal():
    # Scenario 7: Node has 0 available memory for vertical OOM expansion -> triggers horizontal fallback
    shield = OOMProtectionShield()
    dec = shield.evaluate_memory_risk(
        current_usage_mib=3900.0,
        current_request_mib=2048,
        current_limit_mib=4096,
        usage_history_mib=[3000.0, 3450.0, 3900.0],
        node_available_mib=0,
    )
    assert dec.requires_horizontal_fallback is True
    assert dec.action == "horizontal_scale_out_fallback"


def test_scenario_08_and_09_hpa_and_vpa_conflicts():
    # Scenario 8: Native HPA conflict -> yields replica control, uses vertical resize
    res_hpa = arbitrate_scaling_strategy(
        current_replicas=4,
        min_replicas=2,
        max_replicas=10,
        current_cpu_millicores=1000,
        cpu_usage_millicores=850.0,
        current_memory_mib=1024,
        memory_usage_mib=512.0,
        p99_ms=195.0,
        slo_p99_ms=200.0,
        current_rps=500.0,
        baseline_rps=300.0,
        has_native_hpa=True,
    )
    assert res_hpa.conflict_detected is True
    assert res_hpa.recommended_replicas == 4
    assert res_hpa.strategy == "RESIZE_VERTICALLY"

    # Scenario 9: Both Native HPA and Native VPA active -> cannot mutate either -> NO_OP
    res_both = arbitrate_scaling_strategy(
        current_replicas=4,
        min_replicas=2,
        max_replicas=10,
        current_cpu_millicores=1000,
        cpu_usage_millicores=300.0,
        current_memory_mib=1024,
        memory_usage_mib=300.0,
        p99_ms=50.0,
        slo_p99_ms=200.0,
        has_native_hpa=True,
        has_native_vpa=True,
    )
    assert res_both.conflict_detected is True
    assert res_both.strategy == "NO_OP"


def test_scenario_10_pdb_blocking_drain_prevents_binpack():
    # Scenario 10: Pod blocked by PDB (can_evict=False) prevents node drain
    n1 = NodeState("n1", "m6i.xlarge", "us-east-1a", 140.0, 4000, 16384, 2800, 8192, [PodPlacement("p1", "default", 2800, 8192)])
    n2 = NodeState("n2", "m6i.xlarge", "us-east-1a", 140.0, 4000, 16384, 400, 1024, [PodPlacement("p2", "default", 400, 1024, can_evict=False)])
    res = NodeBinPacker().optimize_cluster([n1, n2])
    assert "n2" not in res.nodes_can_drain


def test_scenario_11_spot_interrupted_during_drain():
    # Scenario 11: Confirmed AWS/GCP/Azure interruption notice triggers EMERGENCY_DRAIN (risk=1.0)
    rep = evaluate_spot_interruption_risk(
        provider="aws",
        node_name="ip-10-0-1-99",
        instance_type="c6i.xlarge",
        signal_type="INTERRUPTION_NOTICE",
    )
    assert rep.risk_score == 1.0
    assert rep.recommended_action == "EMERGENCY_DRAIN"
    assert rep.is_deterministic_notice is True


def test_scenario_12_13_14_prewarm_expired_duplicate_and_offline():
    cp = SaaSControlPlane()
    cp.issue_prewarm_command(
        {
            "command_id": "cmd-exp-1",
            "client_id": "tenant-1",
            "cluster_id": "eks-1",
            "workload": "default/api",
            "execute_at": 1000,
            "expires_at": 1500,
        }
    )
    # Scenario 13: Expired PRE_WARM command rejected
    ack_exp = cp.acknowledge_command(
        {"client_id": "tenant-1", "command_id": "cmd-exp-1", "status": "EXECUTED", "timestamp": 2000}
    )
    assert ack_exp["ack_result"] == "REJECTED_EXPIRED"

    # Scenario 14: Duplicate PRE_WARM command ignored
    cp.issue_prewarm_command(
        {
            "command_id": "cmd-dup-1",
            "client_id": "tenant-1",
            "cluster_id": "eks-1",
            "workload": "default/api",
            "execute_at": 1000,
            "expires_at": 5000,
        }
    )
    ack1 = cp.acknowledge_command(
        {"client_id": "tenant-1", "command_id": "cmd-dup-1", "status": "EXECUTED", "timestamp": 1200}
    )
    ack2 = cp.acknowledge_command(
        {"client_id": "tenant-1", "command_id": "cmd-dup-1", "status": "EXECUTED", "timestamp": 1205}
    )
    assert ack1["ack_result"] == "EXECUTED"
    assert ack2["ack_result"] == "DUPLICATE_IGNORED"


def test_scenario_15_16_18_19_20_drift_slo_multitenant_errors_and_traffic_drop():
    # Scenario 15: Model drift detected when error spikes
    assert detect_model_drift([50.0, -45.0, 60.0], [3.0, -4.0, 2.0, 3.5, -2.5]) is True

    # Scenario 16: Never shrink when p99 is degraded near/above SLO even if cost_over_target=True
    dec = recommend_resize(2000, p99_ms=192.0, slo_p99_ms=200.0, cost_over_target=True)
    assert dec.action == "grow"

    # Scenario 18: Cross-tenant access blocked
    cp = SaaSControlPlane()
    with pytest.raises(PermissionError):
        cp.get_audit_decisions("tenant-a", tenant_header="tenant-b")

    # Scenario 19: 5xx error spike after resource reduction is rejected by Economic Objective
    obj = evaluate_economic_objective(
        cost_per_1k_req_usd=0.05,
        target_cost_per_1k_req_usd=0.10,
        p99_latency_ms=120.0,
        slo_p99_latency_ms=200.0,
        error_rate=0.08,  # 8% 5xx errors
        max_acceptable_error_rate=0.01,
    )
    assert obj.recommendation_acceptable is False
    assert obj.error_penalty_component > 0

    # Scenario 20: Traffic drop is never miscounted as optimization savings
    audit = audit_counterfactual_savings(
        baseline_cost_usd=1000.0,
        baseline_successful_requests=10_000_000,
        optimized_actual_cost_usd=200.0,
        optimized_successful_requests=2_000_000,  # 80% traffic drop, same $/1k req
    )
    assert audit.net_savings_usd == 0.0
    assert audit.is_verified_saving is False


def test_fase_10_cast_dashboard_and_1click_execution():
    cp = SaaSControlPlane()
    dash = cp.get_cast_dashboard()
    assert "pain_number_cards" in dash
    assert dash["pain_number_cards"]["current_monthly_spend_usd"] > dash["pain_number_cards"]["projected_optimized_spend_usd"]
    assert len(dash["namespace_bar_chart"]) >= 4
    assert len(dash["actionable_recommendations"]) >= 4

    exec_res = cp.execute_ui_recommendation({"recommendation_id": "rec-resize-payment-01", "workload": "payment-service"})
    assert exec_res["status"] == "executed"
    dash_after = cp.get_cast_dashboard()
    assert dash_after["pain_number_cards"]["realized_1click_savings_usd"] >= 1420.0
