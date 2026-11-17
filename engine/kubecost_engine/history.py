"""Convert retained, tenant-scoped container telemetry into optimizer observations."""
import hashlib
import json
import time

from .optimization import number
from .security import validate_identifier


def resolve_samples(store, tenant, payload, route):
    now = time.time()
    window = number(payload.get("window_seconds", payload.get("policy", {}).get("window_seconds", 86400)), "window_seconds", 60, 2592000)
    # A fixed container identity avoids summing partial replicas or mixing restarts.
    identity = {key: validate_identifier(payload[key], key) for key in ("namespace", "pod_name", "container_name")}
    workload = payload["workload"].split("/", 1)[-1]
    rows = store.telemetry_history(tenant, payload["cluster_id"], now - window)
    rows = [r for r in rows if all(r.get(k) == v for k, v in identity.items()) and r.get("workload_name") in (workload, payload["workload"])]
    by_timestamp = {}
    metric = payload.get("demand_metric", "rps")
    if metric not in ("rps", "cpu_usage_cores", "mem_usage_bytes"):
        raise ValueError("unsupported demand_metric")
    for row in rows:
        if "cpu_usage_cores" not in row:
            continue  # Unmigrated legacy payloads are not observations of this schema.
        if route == "rightsizing":
            sample = {"timestamp": row["timestamp"], "cpu_millicores": row["cpu_usage_cores"] * 1000, "memory_mib": row["mem_usage_bytes"] / 1048576}
        else:
            sample = {"timestamp": row["timestamp"], "demand": row[metric]}
        previous = by_timestamp.get(sample["timestamp"])
        if previous is not None and previous != sample:
            raise ValueError("conflicting telemetry observations for the same container and timestamp")
        by_timestamp[sample["timestamp"]] = sample
    samples = sorted(by_timestamp.values(), key=lambda s: s["timestamp"])
    provenance = {"source": "persisted_telemetry", "scope": "single_pod_container", **identity,
                  "demand_metric": metric if route != "rightsizing" else None,
                  "sample_count": len(samples), "samples_sha256": hashlib.sha256(json.dumps(samples, sort_keys=True).encode()).hexdigest(),
                  "oom_history_available": False}
    return {**payload, "samples": samples}, provenance
