"""Evidence-based recommendations; these functions never apply Kubernetes changes."""

from __future__ import annotations

import math
import statistics
import time


def number(value, name, minimum=0, maximum=1e15):
    if (
        isinstance(value, bool)
        or not isinstance(value, (int, float))
        or not math.isfinite(value)
        or not minimum <= value <= maximum
    ):
        raise ValueError(f"{name} must be finite and between {minimum} and {maximum}")
    return value


def samples_in_window(samples, fields, window_seconds, now):
    if not isinstance(samples, list) or len(samples) > 10000:
        raise ValueError("samples must be a list of at most 10000 observations")
    result = []
    seen = set()
    for sample in samples:
        ts = number(sample["timestamp"], "timestamp", maximum=now + 30)
        if ts in seen:
            raise ValueError("duplicate sample timestamp")
        seen.add(ts)
        for field in fields:
            number(sample[field], field)
        if ts >= now - window_seconds:
            result.append(sample)
    return sorted(result, key=lambda s: s["timestamp"])


def percentile(values, fraction):
    values = sorted(values)
    return values[max(0, math.ceil(len(values) * fraction) - 1)]


def rightsizing(payload, now=None):
    now = time.time() if now is None else now
    policy = payload.get("policy", {})
    window = number(policy.get("window_seconds", 86400), "window_seconds", 60, 2592000)
    min_samples = number(policy.get("min_samples", 20), "min_samples", 3, 10000)
    headroom = number(policy.get("headroom", 1.3), "headroom", 1.1, 3)
    current = payload["current"]
    for key in (
        "cpu_request_millicores",
        "memory_request_mib",
        "cpu_limit_millicores",
        "memory_limit_mib",
    ):
        number(current[key], key, 1)
    for resource in ("cpu", "memory"):
        unit = "millicores" if resource == "cpu" else "mib"
        if current[f"{resource}_request_{unit}"] > current[f"{resource}_limit_{unit}"]:
            raise ValueError("request exceeds limit")
    samples = samples_in_window(
        payload["samples"], ("cpu_millicores", "memory_mib"), window, now
    )
    if any("oom_killed" in sample and type(sample["oom_killed"]) is not bool for sample in samples):
        raise ValueError("oom_killed must be a boolean when supplied")
    fresh = bool(samples) and now - samples[-1]["timestamp"] <= min(300, window)
    result = {
        "kind": "recommendation",
        "observed": {"sample_count": len(samples), "fresh": fresh},
        "recommendation": dict(current),
        "reasons": [],
    }
    if len(samples) < min_samples or not fresh:
        result.update(status="insufficient_data")
        result["reasons"].append(
            "Hold current resources: insufficient fresh historical observations"
        )
        return result
    cpu = [s["cpu_millicores"] for s in samples]
    mem = [s["memory_mib"] for s in samples]
    cpu_p95, mem_p99 = percentile(cpu, 0.95), percentile(mem, 0.99)
    oom = any(s.get("oom_killed") is True for s in samples)
    memory_floor = max(mem_p99 * headroom, max(mem) * 1.1)
    if oom:
        memory_floor = max(memory_floor, current["memory_limit_mib"] * 1.25)
    cpu_request = math.ceil(max(10, cpu_p95 * headroom))
    memory_request = math.ceil(max(16, memory_floor))
    # Bound each reduction, regardless of how oversized the current allocation is.
    cpu_request = max(cpu_request, math.ceil(current["cpu_request_millicores"] * 0.8))
    memory_request = max(memory_request, math.ceil(current["memory_request_mib"] * 0.9))
    recommendation = {
        "cpu_request_millicores": cpu_request,
        "cpu_limit_millicores": math.ceil(max(current["cpu_limit_millicores"], cpu_request, max(cpu) * headroom)),
        "memory_request_mib": memory_request,
        "memory_limit_mib": math.ceil(max(current["memory_limit_mib"], memory_request * 1.1, memory_floor)),
    }
    result.update(status="ready", recommendation=recommendation)
    result["observed"].update(
        cpu_p95_millicores=cpu_p95,
        memory_p99_mib=mem_p99,
        memory_peak_mib=max(mem),
        oom_killed=oom,
    )
    result["anomalies"] = []
    for label, values in (("cpu", cpu), ("memory", mem)):
        median = statistics.median(values)
        mad = statistics.median(abs(v - median) for v in values)
        if values[-1] > median + max(6 * mad, median * 0.5, 1):
            result["anomalies"].append(f"{label}_spike")
    result["reasons"] = [
        "CPU p95 and memory p99 with configurable headroom; peak-memory floor",
        "Per-decision reduction limited to 20% CPU and 10% memory",
        "Existing CPU and memory limits are never lowered",
        "OOM history prevents memory reduction"
        if oom
        else "No OOM event supplied in this window",
    ]
    result["provisioning"] = {
        key: (
            "underprovisioned"
            if recommendation[key] > current[key]
            else "overprovisioned"
            if recommendation[key] < current[key]
            else "balanced"
        )
        for key in recommendation
    }
    return result


def forecast(payload, now=None):
    now = time.time() if now is None else now
    window = number(payload.get("window_seconds", 86400), "window_seconds", 60, 2592000)
    horizon = number(payload.get("horizon_seconds", 300), "horizon_seconds", 1, 3600)
    samples = samples_in_window(payload["samples"], ("demand",), window, now)
    result = {
        "kind": "prediction",
        "created_at": now,
        "target_timestamp": now + horizon,
        "sample_count": len(samples),
        "confidence": 0,
        "predicted_demand": None,
        "interval": None,
        "seasonality_detected": False,
        "status": "insufficient_data",
    }
    if len(samples) < 12 or now - samples[-1]["timestamp"] > 300:
        return result
    gaps = [
        samples[i]["timestamp"] - samples[i - 1]["timestamp"]
        for i in range(1, len(samples))
    ]
    cadence = statistics.median(gaps)
    if max(abs(g - cadence) for g in gaps) > cadence * 0.2 or horizon > cadence * 12:
        result["status"] = "irregular_or_excessive_horizon"
        return result
    values = [s["demand"] for s in samples]
    # Only use repeated cycles with at least three complete periods.
    period = payload.get("seasonal_period_samples", 24)
    if type(period) is not int or not 2 <= period <= 1000:
        raise ValueError("seasonal_period_samples must be an integer in [2,1000]")
    baseline_errors = [abs(values[i] - values[i - 1]) for i in range(1, len(values))]
    if len(values) >= period * 3:
        seasonal_errors = [
            abs(values[i] - values[i - period]) for i in range(period, len(values))
        ]
        # Compare on identical held-out indices.
        if statistics.mean(seasonal_errors) < 0.8 * statistics.mean(
            baseline_errors[period - 1 :]
        ):
            result["seasonality_detected"] = True
    slope = statistics.median(
        [(values[i] - values[i - 1]) / gaps[i - 1] for i in range(1, len(values))]
    )
    point = max(0, values[-1] + slope * horizon)
    errors = baseline_errors
    if result["seasonality_detected"]:
        point = values[-period + ((math.ceil(horizon / cadence) - 1) % period)]
        errors = seasonal_errors
    mae = statistics.mean(errors)
    confidence = max(0, min(0.95, 1 - mae / max(statistics.mean(values), 1)))
    band = max(percentile(errors, 0.95), 1) * math.sqrt(max(1, horizon / cadence))
    result.update(
        status="ready" if confidence >= 0.7 else "low_confidence",
        predicted_demand=point,
        confidence=confidence,
        interval=[max(0, point - band), point + band],
        backtest_mae=mae,
        trend_per_second=slope,
        method="seasonal_naive" if result["seasonality_detected"] else "median_trend",
        interval_kind="heuristic_not_calibrated",
    )
    return result


def backtest(payload, now=None):
    """Expanding-window evaluation. Each forecast only receives its past samples."""
    now = time.time() if now is None else now
    samples = samples_in_window(payload["samples"], ("demand",), 2592000, now)
    train = payload.get("min_train_samples", 12)
    stride = payload.get("stride", 1)
    if type(train) is not int or not 12 <= train <= 5000:
        raise ValueError("min_train_samples must be an integer in [12,5000]")
    if type(stride) is not int or not 1 <= stride <= 1000:
        raise ValueError("stride must be an integer in [1,1000]")
    indices = list(range(train, len(samples), stride))
    if len(indices) > 100:
        raise ValueError("at most 100 folds; increase stride")
    folds = []
    for index in indices:
        past = samples[:index]
        target = samples[index]
        horizon = target["timestamp"] - past[-1]["timestamp"]
        if horizon > 3600:
            folds.append({"target_timestamp": target["timestamp"], "status": "gap_too_large"})
            continue
        prediction = forecast({**payload, "samples": past, "horizon_seconds": horizon}, now=past[-1]["timestamp"])
        fold = {"target_timestamp": target["timestamp"], "training_end": past[-1]["timestamp"], "status": prediction["status"]}
        if prediction["predicted_demand"] is not None:
            observed = target["demand"]
            fold.update(observed=observed, predicted=prediction["predicted_demand"],
                        absolute_error=abs(observed - prediction["predicted_demand"]),
                        naive_absolute_error=abs(observed - past[-1]["demand"]),
                        interval_covered=prediction["interval"][0] <= observed <= prediction["interval"][1])
        folds.append(fold)
    evaluated = [f for f in folds if "absolute_error" in f]
    return {"kind": "temporal_backtest", "status": "evaluated" if evaluated else "insufficient_data",
            "folds": folds, "evaluated_folds": len(evaluated), "skipped_folds": len(folds) - len(evaluated),
            "mae": statistics.mean(f["absolute_error"] for f in evaluated) if evaluated else None,
            "naive_mae": statistics.mean(f["naive_absolute_error"] for f in evaluated) if evaluated else None,
            "empirical_interval_coverage": statistics.mean(f["interval_covered"] for f in evaluated) if evaluated else None,
            "interval_kind": "heuristic_not_calibrated", "evaluation": "expanding_window_one_step_no_future_training"}
