import math
import pytest
from kubecost_engine.predictive import (
    HybridTimeSeriesForecaster,
    PredictiveAutoscaler,
    compute_error_metrics,
    detect_model_drift,
)


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
    assert res.cold_start_fallback is True


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
    assert res.prewarm_command is not None
    assert res.prewarm_command["action"] == "PRE_WARM"


def test_predictive_cyclical_peak_hour():
    pa = PredictiveAutoscaler(min_replicas=2, max_replicas=40)
    res_peak = pa.forecast_demand(
        current_replicas=2,
        current_rps=100.0,
        target_rps_per_pod=50.0,
        hour_of_day_utc=15,
    )
    res_trough = pa.forecast_demand(
        current_replicas=2,
        current_rps=100.0,
        target_rps_per_pod=50.0,
        hour_of_day_utc=3,
    )
    assert res_peak.predicted_replicas >= res_trough.predicted_replicas


def test_hybrid_forecaster_beats_baseline_on_seasonal_wave_and_emits_metrics():
    forecaster = HybridTimeSeriesForecaster(min_seasonal_samples=8, min_sequential_samples=16)
    # Generate a clean sinusoidal seasonal series where Fourier decomposition outperforms naive moving average
    history = [200.0 + 80.0 * math.sin(2 * math.pi * i / 8.0) + 2.0 * i for i in range(20)]
    model_name, point, low, high, metrics, cold_start = forecaster.select_best_model_and_forecast(
        history, history[-1]
    )
    assert cold_start is False
    assert model_name in ("seasonal_fourier_prophet", "sequential_gated_lstm", "baseline_moving_average")
    assert low <= point <= high
    assert metrics.mae >= 0.0
    assert metrics.rmse >= 0.0
    assert 0.0 <= metrics.wape <= 1.0
    assert 0.0 <= metrics.interval_coverage <= 1.0


def test_model_drift_detection_and_1745_prewarm_for_1800_spike():
    # Historical errors around ~5 RPS, recent errors jumped to ~35 RPS -> drift detected
    assert detect_model_drift([32.0, -38.0, 40.0, 35.0], [4.0, -5.0, 6.0, 3.0, -4.0, 5.0]) is True
    assert detect_model_drift([4.5, -5.0, 4.0], [4.0, -5.0, 6.0, 3.0, -4.0, 5.0]) is False

    # 300% traffic spike predicted at 18:00 (timestamp=1711908000) -> PRE_WARM execute_at 17:45 (900s earlier)
    pa = PredictiveAutoscaler(min_replicas=2, max_replicas=50, warmup_lead_time_seconds=900)
    spike_1800_ts = 1711908000
    cmd = pa.build_prewarm_command(
        cluster_id="eks-prod-1",
        workload="default/checkout-api",
        recommended_replicas=12,
        requested_cpu="1500m",
        requested_memory="2Gi",
        confidence=0.91,
        reason="Predicted 300% seasonal traffic spike at 18:00 UTC",
        spike_timestamp=spike_1800_ts,
        lead_time_seconds=900,
    )
    assert cmd["execute_at"] == spike_1800_ts - 900  # 17:45 UTC
    assert cmd["expires_at"] > spike_1800_ts
    assert cmd["recommended_replicas"] == 12
    assert cmd["requested_resources"] == {"cpu": "1500m", "memory": "2Gi"}
