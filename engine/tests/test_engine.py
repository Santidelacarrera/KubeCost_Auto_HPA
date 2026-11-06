import math
import pytest
from kubecost_engine import (
    BudgetGuardrail, SpotCandidate, cost_per_request, cost_per_request_from_pod,
    is_spot_eligible, recommend_resize,
)


def test_cost_per_request():
    assert cost_per_request(3.6, 1) == pytest.approx(0.001)
    assert math.isinf(cost_per_request(1.0, 0))
    with pytest.raises(ValueError):
        cost_per_request(-1, 1)


def test_cost_from_pod():
    assert cost_per_request_from_pod(2, 4, 10, 0.03, 0.005) == pytest.approx(0.08 / 36000)


def test_resize():
    assert recommend_resize(1000, 50, 200).action == "shrink"
    assert recommend_resize(1000, 50, 200).cpu_millicores == 800
    assert recommend_resize(1000, 190, 200).action == "grow"
    assert recommend_resize(1000, 120, 200).action == "hold"
    assert recommend_resize(50, 10, 200).action == "hold"
    assert recommend_resize(8000, 199, 200).action == "hold"


def test_budget():
    g = BudgetGuardrail(100)
    assert g.allows("grow")
    g.record(100)
    assert g.frozen and not g.allows("grow") and g.allows("shrink")


def test_spot():
    assert is_spot_eligible(SpotCandidate(True, 3, True))
    assert not is_spot_eligible(SpotCandidate(True, 1, True))
    assert not is_spot_eligible(SpotCandidate(True, 3, True, stateful=True))


from kubecost_engine import recommend_memory, recommend_replicas  # noqa: E402


def test_memory_asymmetric():
    assert recommend_memory(1000, 900).action == "grow"
    assert recommend_memory(1000, 900).cpu_millicores == 1250
    s = recommend_memory(1000, 100)
    assert s.action == "shrink" and s.cpu_millicores == 900  # 10% step, floor 130
    assert recommend_memory(200, 90, min_mib=200).action == "hold"  # min floor
    assert recommend_memory(1000, 600).action == "hold"
    assert recommend_memory(16384, 16000).action == "hold"
    with pytest.raises(ValueError):
        recommend_memory(0, 1)


def test_replicas_out_only_when_cpu_maxed():
    maxed = recommend_resize(8000, 195, 200)
    assert maxed.limit == "max"
    assert recommend_replicas(3, 1, 10, maxed)[0] == 4
    assert recommend_replicas(10, 1, 10, maxed)[0] == 10
    assert recommend_replicas(3, 1, 10, recommend_resize(1000, 195, 200))[0] == 3  # can still grow vertically


def test_replicas_in_requires_headroom():
    low = recommend_resize(100, 20, 200, min_millicores=100)
    assert low.limit == "min"
    # 3 pods at 30m usage -> 45m with 2 pods <= 70m
    assert recommend_replicas(3, 1, 10, low, cpu_usage_millicores=30, cpu_millicores=100, configured_min_millicores=100)[0] == 2
    # 60m usage -> 90m > 70m: keep
    assert recommend_replicas(3, 1, 10, low, cpu_usage_millicores=60, cpu_millicores=100, configured_min_millicores=100)[0] == 3
    assert recommend_replicas(3, 1, 10, low, cpu_millicores=100, configured_min_millicores=100)[0] == 3
    assert recommend_replicas(1, 1, 10, low, cpu_usage_millicores=1, cpu_millicores=100, configured_min_millicores=100)[0] == 1
    with pytest.raises(ValueError):
        recommend_replicas(1, 0, 1, low)


from kubecost_engine.economics import (  # noqa: E402
    audit_counterfactual_savings,
    calculate_commercial_billing,
    compute_workload_cost_breakdown,
    cost_per_1k_successful_requests,
    cost_per_sustained_rps,
    evaluate_economic_objective,
)


def test_unit_economics_1k_successful_requests_and_sustained_rps():
    # $3.60 allocated over 1 hour serving 100 RPS (360,000 requests) -> $0.01 per 1k requests
    assert cost_per_1k_successful_requests(3.60, 360_000) == pytest.approx(0.01)
    assert math.isinf(cost_per_1k_successful_requests(3.60, 0))

    # Cost per sustained RPS over 1h (3600s) at $3.60/hr and 100 RPS = $0.036 per sustained RPS
    assert cost_per_sustained_rps(3.60, 100.0, observation_window_seconds=3600.0) == pytest.approx(0.036)

    # Breakdown with 50% error rate doubles the cost per 1k successful requests vs 0% error rate
    clean = compute_workload_cost_breakdown(
        replicas=4,
        cpu_request_cores_per_pod=2.0,
        cpu_used_cores_per_pod=1.0,
        mem_request_gib_per_pod=4.0,
        mem_used_gib_per_pod=2.0,
        total_rps=200.0,
        error_rate=0.0,
    )
    degraded = compute_workload_cost_breakdown(
        replicas=4,
        cpu_request_cores_per_pod=2.0,
        cpu_used_cores_per_pod=1.0,
        mem_request_gib_per_pod=4.0,
        mem_used_gib_per_pod=2.0,
        total_rps=200.0,
        error_rate=0.50,
    )
    assert degraded.cost_per_1k_successful_req_usd == pytest.approx(clean.cost_per_1k_successful_req_usd * 2.0)


def test_counterfactual_savings_and_monetization_models():
    # Baseline: $10,000 for 100M requests ($0.10 / 1k req)
    # Optimized: $6,200 actual + $300 prewarm overhead for 100M requests -> Net savings = $3,500 (35%)
    audit = audit_counterfactual_savings(
        baseline_cost_usd=10000.0,
        baseline_successful_requests=100_000_000,
        optimized_actual_cost_usd=6200.0,
        optimized_successful_requests=100_000_000,
        incremental_optimization_cost_usd=300.0,
        slo_violated=False,
    )
    assert audit.is_verified_saving is True
    assert audit.net_savings_usd == pytest.approx(3500.0)
    assert audit.net_savings_pct == pytest.approx(35.0)

    # If traffic dropped by 50% (50M requests) and spend dropped to $5,000, counterfactual baseline is $5,000 -> $0 net savings!
    traffic_drop_audit = audit_counterfactual_savings(
        baseline_cost_usd=10000.0,
        baseline_successful_requests=100_000_000,
        optimized_actual_cost_usd=5000.0,
        optimized_successful_requests=50_000_000,
        incremental_optimization_cost_usd=0.0,
    )
    assert traffic_drop_audit.net_savings_usd == pytest.approx(0.0)
    assert traffic_drop_audit.is_verified_saving is False

    # Commercial Billing: Model A (22% Value Shared) & Model B ($25/node/month)
    billing = calculate_commercial_billing(
        verified_monthly_net_savings_usd=3500.0,
        managed_nodes_count=20,
        value_shared_percentage=0.22,
        fixed_price_per_node_month_usd=25.0,
    )
    assert billing["model_a_value_shared"]["platform_fee_usd"] == pytest.approx(770.0)
    assert billing["model_a_value_shared"]["customer_net_benefit_usd"] == pytest.approx(2730.0)
    assert billing["model_b_fixed_per_node"]["platform_fee_usd"] == pytest.approx(500.0)
    assert billing["model_b_fixed_per_node"]["customer_net_benefit_usd"] == pytest.approx(3000.0)

