import pytest
from kubecost_engine.predictive import PredictiveAutoscaler


def test_predictive_steady_state():
    pa = PredictiveAutoscaler(min_replicas=2, max_replicas=20)
    res = pa.forecast_demand(
        current_replicas=4,
        current_rps=200.0,
        target_rps_per_pod=50.0,
        rps_history=[190.0, 195.0, 200.0],
    )
    assert res.reactive_replicas == 4
    assert res.recommended_replicas == 4
    assert not res.is_prewarming


def test_predictive_surge_velocity_prewarming():
    pa = PredictiveAutoscaler(min_replicas=2, max_replicas=30)
    # Rapid surge: RPS went from 100 to 250 in 3 intervals
    res = pa.forecast_demand(
        current_replicas=4,
        current_rps=250.0,
        target_rps_per_pod=50.0,
        rps_history=[100.0, 160.0, 250.0],
    )
    assert res.is_prewarming is True
    assert res.recommended_replicas > res.reactive_replicas
    assert res.confidence_score >= 0.85
    assert "Predictive pre-warm" in res.reason


def test_predictive_cyclical_peak_hour():
    pa = PredictiveAutoscaler(min_replicas=2, max_replicas=40)
    # 15:00 UTC is peak traffic hour in the diurnal curve
    res_peak = pa.forecast_demand(
        current_replicas=2,
        current_rps=100.0,
        target_rps_per_pod=50.0,
        hour_of_day_utc=15,
    )
    # 03:00 UTC is trough hour
    res_trough = pa.forecast_demand(
        current_replicas=2,
        current_rps=100.0,
        target_rps_per_pod=50.0,
        hour_of_day_utc=3,
    )
    assert res_peak.predicted_replicas >= res_trough.predicted_replicas
