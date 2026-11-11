import json
import urllib.request

from kubecost_engine.gameday import run_autonomous_gameday
from kubecost_engine.gpu_optimizer import evaluate_gpu_workload
from kubecost_engine.greenops_guardrail import (
    evaluate_finops_budget_guardrail,
    evaluate_greenops_carbon,
)
from kubecost_engine.server import make_server


def test_gpu_optimizer_mig_partition_and_token_economics():
    plan = evaluate_gpu_workload(
        workload="llm-gateway-vllm",
        current_gpu_model="A100-80GB",
        requested_gpus=1,
        sm_utilization_pct=20.0,
        vram_used_gib=12.0,
        tokens_per_second=500.0,
        inference_rps=20.0,
        spot_tolerant=True,
        latency_p99_ms=170.0,
        latency_slo_ms=350.0,
    )
    assert plan.action == "PARTITION_NVIDIA_MIG"
    assert plan.recommended_resource_name == "nvidia.com/mig-2g.20gb"
    assert plan.use_spot is True
    assert plan.monthly_savings_usd > 1500.0
    assert plan.cost_per_1k_tokens_usd > 0


def test_greenops_carbon_and_nexus_finops_guardrail():
    carbon = evaluate_greenops_carbon(
        current_region="us-east-1",
        active_cpu_cores=64.0,
        reclaimed_cpu_cores=24.0,
        active_memory_gib=256.0,
        reclaimed_memory_gib=96.0,
        active_gpus=2,
    )
    assert carbon.monthly_co2_saved_kg > 100.0
    assert carbon.co2_reduction_pct > 25.0
    assert carbon.recommended_green_region == "eu-north-1"

    # Normal mode (<80%)
    g_normal = evaluate_finops_budget_guardrail(
        tenant_id="org-1",
        monthly_budget_usd=10000.0,
        mtd_spend_usd=4000.0,
        day_of_month=20,
        days_in_month=30,
    )
    assert g_normal.mode == "NORMAL"
    assert g_normal.allow_scale_out is True

    # Block mode (>100% projected EOM)
    g_block = evaluate_finops_budget_guardrail(
        tenant_id="org-1",
        monthly_budget_usd=10000.0,
        mtd_spend_usd=7500.0,
        day_of_month=20,
        days_in_month=30,
        slo_critical_breach=False,
    )
    assert g_block.mode == "BLOCK"
    assert g_block.allow_scale_out is False
    assert g_block.force_spot_and_mig is True
    assert "CloudCost_Optimizer_FinOps_Guardrail" in g_block.linked_ecosystem


def test_gameday_simulator_scorecard():
    card = run_autonomous_gameday(cluster_id="prod-us-east-1", baseline_rps=840.0, node_count=12)
    assert card.overall_resilience_score >= 98.0
    assert card.total_downtime_seconds == 0.0
    assert card.total_oom_kills_prevented >= 4
    assert len(card.scenarios) == 4


def test_server_gpu_greenops_guardrail_gameday_endpoints():
    srv = make_server(host="127.0.0.1", port=0)
    port = srv.server_address[1]
    import threading

    t = threading.Thread(target=srv.serve_forever, daemon=True)
    t.start()
    try:
        with urllib.request.urlopen(f"http://127.0.0.1:{port}/v1/gpu/optimize") as resp:
            gpu_data = json.loads(resp.read().decode())
            assert gpu_data["action"] == "PARTITION_NVIDIA_MIG"

        with urllib.request.urlopen(f"http://127.0.0.1:{port}/v1/greenops/report?region=us-east-1") as resp:
            green_data = json.loads(resp.read().decode())
            assert green_data["monthly_co2_saved_kg"] > 0

        with urllib.request.urlopen(f"http://127.0.0.1:{port}/v1/finops/guardrail?mtd_spend_usd=9800&day_of_month=20&monthly_budget_usd=15000") as resp:
            guard_data = json.loads(resp.read().decode())
            assert guard_data["mode"] in ("ALERT", "DEGRADE", "BLOCK")

        with urllib.request.urlopen(f"http://127.0.0.1:{port}/v1/gameday/simulate") as resp:
            gd_data = json.loads(resp.read().decode())
            assert gd_data["overall_resilience_score"] >= 98.0

        with urllib.request.urlopen(f"http://127.0.0.1:{port}/v1/system/readiness") as resp:
            rdy_data = json.loads(resp.read().decode())
            assert rdy_data["overall_status"] == "PRODUCTION_READY_100_PCT"
            assert rdy_data["certified_subsystems"] == 10

        with urllib.request.urlopen(f"http://127.0.0.1:{port}/v1/cast/full-suite") as resp:
            cs_data = json.loads(resp.read().decode())
            assert cs_data["commitments"]["action"] == "ABSORB_INTO_PREPAID_COMMITMENT"
            assert cs_data["commitments"]["monthly_commitment_waste_recovered_usd"] > 300.0
            assert cs_data["hibernation"]["hibernation_action"] == "HIBERNATE_CLUSTER_NOW"
            assert cs_data["hibernation"]["monthly_hibernation_savings_usd"] > 1000.0
            assert cs_data["network_and_storage"]["total_monthly_net_and_storage_savings_usd"] > 400.0
            assert cs_data["security_posture"]["security_posture_score"] >= 90.0
            assert cs_data["security_posture"]["nexus_telemetry_event"]["type"] == "security.finding"
            assert "AWS_EKS" in cs_data["onboarding"]["supported_platforms"]
    finally:
        srv.shutdown()
        srv.server_close()
