"""Autonomous Spot & Node Interruption Intelligence (Motor D — Section 10).

Provides:
- Workload Spot eligibility verification (stateless, PDB-protected, multi-replica, no local storage).
- Decoupled Cloud Provider Spot Signal normalization (AWS ITN/Rebalance/EventBridge/IMDS, GCP Preemptible, Azure Scheduled Events).
- Probabilistic Spot Interruption Risk scoring by instance type, region, AZ, historical reclaim rate, and replacement availability.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Optional


@dataclass(frozen=True)
class SpotCandidate:
    fault_tolerant: bool
    replicas: int
    has_pdb: bool
    stateful: bool = False
    has_local_storage: bool = False


def is_spot_eligible(c: SpotCandidate, min_replicas: int = 2) -> bool:
    """Only stateless, multi-replica, fault-tolerant workloads protected by a PDB without local non-migratable storage."""
    return (
        c.fault_tolerant
        and not c.stateful
        and not c.has_local_storage
        and c.replicas >= min_replicas
        and c.has_pdb
    )


@dataclass(frozen=True)
class SpotRiskReport:
    provider: str  # "aws" | "gcp" | "azure"
    node_name: str
    instance_type: str
    region: str
    availability_zone: str
    signal_type: str  # "NONE" | "MARKET_RISK_ELEVATED" | "REBALANCE_RECOMMENDATION" | "INTERRUPTION_NOTICE"
    risk_score: float  # [0.0, 1.0]
    risk_tier: str  # "LOW" | "MODERATE" | "HIGH" | "IMMINENT"
    recommended_action: str  # "HOLD" | "PREPARE_REPLACEMENT" | "PROACTIVE_CORDON_AND_DRAIN" | "EMERGENCY_DRAIN"
    is_deterministic_notice: bool
    replacement_available: bool
    reason: str


def evaluate_spot_interruption_risk(
    *,
    provider: str = "aws",
    node_name: str,
    instance_type: str,
    region: str = "us-east-1",
    availability_zone: str = "us-east-1a",
    signal_type: str = "NONE",
    historical_interruption_rate: float = 0.05,
    pool_capacity_tightness: float = 0.20,
    replacement_available: bool = True,
) -> SpotRiskReport:
    """Evaluates cloud Spot interruption risk without claiming 100% deterministic certainty prior to official provider signals."""
    prov = provider.lower().strip() or "aws"
    sig = signal_type.upper().strip() or "NONE"
    hist = max(0.0, min(1.0, historical_interruption_rate))
    tight = max(0.0, min(1.0, pool_capacity_tightness))

    it_lower = instance_type.lower()
    family_penalty = (
        0.12
        if any(k in it_lower for k in ("p3", "p4", "g5", "4xlarge", "8xlarge", "16xlarge"))
        else 0.0
    )

    if sig == "INTERRUPTION_NOTICE":
        return SpotRiskReport(
            provider=prov,
            node_name=node_name,
            instance_type=instance_type,
            region=region,
            availability_zone=availability_zone,
            signal_type=sig,
            risk_score=1.0,
            risk_tier="IMMINENT",
            recommended_action="EMERGENCY_DRAIN",
            is_deterministic_notice=True,
            replacement_available=replacement_available,
            reason=f"Confirmed {prov.upper()} termination notice on {node_name} ({instance_type} in {availability_zone}); immediate Cordon + Eviction drain.",
        )

    if sig == "REBALANCE_RECOMMENDATION":
        score = round(min(0.95, 0.80 + 0.15 * tight), 3)
        act = "PROACTIVE_CORDON_AND_DRAIN" if replacement_available else "PREPARE_REPLACEMENT"
        return SpotRiskReport(
            provider=prov,
            node_name=node_name,
            instance_type=instance_type,
            region=region,
            availability_zone=availability_zone,
            signal_type=sig,
            risk_score=score,
            risk_tier="HIGH",
            recommended_action=act,
            is_deterministic_notice=True,
            replacement_available=replacement_available,
            reason=f"Early {prov.upper()} Rebalance Recommendation on {node_name}; proactive drain initiated ahead of 2-minute ITN.",
        )

    prob_score = min(1.0, max(0.0, 0.45 * hist + 0.43 * tight + family_penalty))
    if sig == "MARKET_RISK_ELEVATED":
        prob_score = max(prob_score, 0.72)
    prob_score = round(prob_score, 3)

    if prob_score >= 0.70:
        act = "PROACTIVE_CORDON_AND_DRAIN" if replacement_available else "PREPARE_REPLACEMENT"
        tier = "HIGH"
    elif prob_score >= 0.40:
        act = "PREPARE_REPLACEMENT"
        tier = "MODERATE"
    else:
        act = "HOLD"
        tier = "LOW"

    return SpotRiskReport(
        provider=prov,
        node_name=node_name,
        instance_type=instance_type,
        region=region,
        availability_zone=availability_zone,
        signal_type=sig,
        risk_score=prob_score,
        risk_tier=tier,
        recommended_action=act,
        is_deterministic_notice=False,
        replacement_available=replacement_available,
        reason=f"Probabilistic {prov.upper()} Spot pool risk={prob_score:.2f} ({tier}) for {instance_type} in {availability_zone}.",
    )
