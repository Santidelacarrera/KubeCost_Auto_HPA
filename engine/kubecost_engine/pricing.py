"""Multi-Cloud (AWS, GCP, Azure) Dynamic Instance Pricing Catalog for the SaaS Control Plane."""

from dataclasses import asdict, dataclass


@dataclass(frozen=True)
class CloudInstancePricing:
    provider: str
    instance_type: str
    region: str
    vcpus: int
    memory_gib: float
    ondemand_hourly_usd: float
    spot_hourly_usd: float
    cpu_price_per_core_hour: float
    mem_price_per_gib_hour: float
    spot_cpu_price_per_core_hour: float
    spot_mem_price_per_gib_hour: float
    spot_discount_pct: float


REGION_MULTIPLIERS: dict[str, float] = {
    "us-east-1": 1.00,
    "us-east-2": 0.98,
    "us-west-2": 1.04,
    "eu-west-1": 1.11,
    "eu-central-1": 1.15,
    "ap-southeast-1": 1.18,
    "sa-east-1": 1.38,
    "us-central1": 1.00,
    "europe-west1": 1.10,
    "eastus": 1.00,
    "westeurope": 1.12,
}

BASE_INSTANCES: dict[str, tuple[str, int, float, float, float]] = {
    # instance_type: (provider, vcpus, memory_gib, ondemand_usd, spot_usd)
    "m6i.large": ("AWS", 2, 8.0, 0.096, 0.034),
    "m6i.xlarge": ("AWS", 4, 16.0, 0.192, 0.068),
    "m6i.2xlarge": ("AWS", 8, 32.0, 0.384, 0.135),
    "c6i.xlarge": ("AWS", 4, 8.0, 0.170, 0.058),
    "c6i.2xlarge": ("AWS", 8, 16.0, 0.340, 0.115),
    "c6i.4xlarge": ("AWS", 16, 32.0, 0.680, 0.230),
    "c6g.xlarge": ("AWS", 4, 8.0, 0.136, 0.046),
    "r6i.xlarge": ("AWS", 4, 32.0, 0.252, 0.082),
    "t3.medium": ("AWS", 2, 4.0, 0.0416, 0.0125),
    "t3.large": ("AWS", 2, 8.0, 0.0832, 0.0250),
    "e2-standard-4": ("GCP", 4, 16.0, 0.134, 0.040),
    "n2-standard-4": ("GCP", 4, 16.0, 0.194, 0.058),
    "n2-standard-8": ("GCP", 8, 32.0, 0.388, 0.116),
    "c2-standard-8": ("GCP", 8, 32.0, 0.417, 0.125),
    "standard_d4s_v5": ("AZURE", 4, 16.0, 0.192, 0.059),
    "standard_d8s_v5": ("AZURE", 8, 32.0, 0.384, 0.118),
    "standard_f8s_v2": ("AZURE", 8, 16.0, 0.338, 0.098),
}


def lookup_instance_pricing(
    instance_type: str,
    region: str = "us-east-1",
) -> dict:
    """Return regionalized On-Demand and Spot unit pricing for a cloud VM."""
    clean_reg = (region or "us-east-1").strip().lower()
    mult = REGION_MULTIPLIERS.get(clean_reg, 1.0)
    clean_type = (instance_type or "m6i.2xlarge").strip().lower()

    if clean_type in BASE_INSTANCES:
        prov, vcpus, mem_gib, od_base, sp_base = BASE_INSTANCES[clean_type]
    else:
        prov, vcpus, mem_gib, od_base, sp_base = ("GENERIC", 4, 16.0, 0.188, 0.065)

    od = round(od_base * mult, 5)
    sp = round(sp_base * mult, 5)
    cpu_od = round((od * 0.75) / vcpus, 5)
    mem_od = round((od * 0.25) / mem_gib, 5)
    cpu_sp = round((sp * 0.75) / vcpus, 5)
    mem_sp = round((sp * 0.25) / mem_gib, 5)
    disc = round((1.0 - (sp / od)) * 100.0, 1) if od > 0 else 0.0

    return asdict(
        CloudInstancePricing(
            provider=prov,
            instance_type=instance_type or "m6i.2xlarge",
            region=clean_reg,
            vcpus=vcpus,
            memory_gib=mem_gib,
            ondemand_hourly_usd=od,
            spot_hourly_usd=sp,
            cpu_price_per_core_hour=cpu_od,
            mem_price_per_gib_hour=mem_od,
            spot_cpu_price_per_core_hour=cpu_sp,
            spot_mem_price_per_gib_hour=mem_sp,
            spot_discount_pct=disc,
        )
    )
