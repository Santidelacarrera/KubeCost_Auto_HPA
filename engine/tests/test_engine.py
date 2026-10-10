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
