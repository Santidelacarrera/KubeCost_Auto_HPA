"""Unit economics: C_pod = instance cost / RPS."""

SECONDS_PER_HOUR = 3600.0


def cost_per_request(instance_cost_per_hour: float, rps: float) -> float:
    """Dollars per request served. Infinite when the pod serves no traffic."""
    if instance_cost_per_hour < 0 or rps < 0:
        raise ValueError("cost and rps must be non-negative")
    if rps == 0:
        return float("inf")
    return instance_cost_per_hour / (rps * SECONDS_PER_HOUR)


def cost_per_request_from_pod(
    cpu_cores: float,
    mem_gib: float,
    rps: float,
    cpu_price_per_core_hour: float,
    mem_price_per_gib_hour: float,
) -> float:
    """Cost per request for a pod priced by its CPU/memory requests."""
    hourly = cpu_cores * cpu_price_per_core_hour + mem_gib * mem_price_per_gib_hour
    return cost_per_request(hourly, rps)
