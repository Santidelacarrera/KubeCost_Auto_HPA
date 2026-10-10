"""Spot migration eligibility."""
from dataclasses import dataclass


@dataclass(frozen=True)
class SpotCandidate:
    fault_tolerant: bool
    replicas: int
    has_pdb: bool
    stateful: bool = False


def is_spot_eligible(c: SpotCandidate, min_replicas: int = 2) -> bool:
    """Only stateless, multi-replica, fault-tolerant workloads protected by a PDB."""
    return c.fault_tolerant and not c.stateful and c.replicas >= min_replicas and c.has_pdb
