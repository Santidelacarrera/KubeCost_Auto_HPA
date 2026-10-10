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
