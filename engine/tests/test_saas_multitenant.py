import pytest
from kubecost_engine.saas import SaaSControlPlane


def test_backward_compatible_section_13_1_telemetry_ingest():
    cp = SaaSControlPlane()
    payload = {
        "client_id": "uuid-tenant-1",
        "cluster_id": "eks-prod-1",
        "metrics": {
            "rps": 450.5,
            "p99_latency_ms": 142.0,
            "cpu_usage_cores": 2.4,
            "mem_usage_bytes": 1073741824,
            "current_replicas": 4,
        },
        "timestamp": 1711890000,
    }
    res = cp.ingest_telemetry(payload)
    assert res["status"] == "ingested"
    assert res["deduplicated"] is False
    assert res["normalized"]["rps"] == 450.5
    assert res["normalized"]["current_replicas"] == 4

    # Replay same payload -> deduplicated by deterministic event_id
    dup = cp.ingest_telemetry(payload)
    assert dup["status"] == "duplicate_ignored"
    assert dup["deduplicated"] is True


def test_multitenant_isolation_prevents_cross_tenant_access():
    cp = SaaSControlPlane()
    cp.register_agent({"client_id": "tenant-alpha", "cluster_id": "eks-alpha-1"}, tenant_header="tenant-alpha")

    # Tenant Beta attempting to read Tenant Alpha's policy must raise PermissionError
    with pytest.raises(PermissionError):
        cp.get_policy("tenant-alpha", "eks-alpha-1", tenant_header="tenant-beta")


def test_prewarm_command_lifecycle_expiry_and_antireplay():
    cp = SaaSControlPlane()
    cmd = cp.issue_prewarm_command(
        {
            "command_id": "cmd-101",
            "client_id": "tenant-alpha",
            "cluster_id": "eks-prod-1",
            "workload": "default/checkout-api",
            "requested_resources": {"cpu": "1500m", "memory": "2Gi"},
            "recommended_replicas": 6,
            "execute_at": 1711890000,
            "expires_at": 1711890900,
            "confidence": 0.91,
            "reason": "Predicted seasonal traffic spike",
            "policy_version": "2026-11-01.v1",
        }
    )
    assert cmd["action"] == "PRE_WARM"

    pending = cp.get_pending_commands("tenant-alpha", "eks-prod-1", now_ts=1711890100)
    assert len(pending) == 1
    assert pending[0]["command_id"] == "cmd-101"

    # First ACK succeeds
    ack1 = cp.acknowledge_command(
        {
            "client_id": "tenant-alpha",
            "command_id": "cmd-101",
            "status": "EXECUTED",
            "timestamp": 1711890200,
        }
    )
    assert ack1["ack_result"] == "EXECUTED"

    # Second ACK is rejected as duplicate (anti-replay)
    ack2 = cp.acknowledge_command(
        {
            "client_id": "tenant-alpha",
            "command_id": "cmd-101",
            "status": "EXECUTED",
            "timestamp": 1711890250,
        }
    )
    assert ack2["ack_result"] == "DUPLICATE_IGNORED"


def test_sqlite_wal_persistence_survives_restart(tmp_path):
    db_file = str(tmp_path / "saas_control_plane.db")
    cp1 = SaaSControlPlane(db_path=db_file)
    payload = {
        "event_id": "evt-persistent-001",
        "client_id": "tenant-sqlite",
        "cluster_id": "eks-prod-1",
        "metrics": {
            "rps": 300.0,
            "p99_latency_ms": 95.0,
            "cpu_usage_cores": 1.8,
            "mem_usage_bytes": 536870912,
            "current_replicas": 3,
        },
        "timestamp": 1711890000,
    }
    first = cp1.ingest_telemetry(payload)
    assert first["status"] == "ingested"
    cp1._store.close()

    # Simulate control plane restart with a fresh SaaSControlPlane pointing to the same SQLite DB
    cp2 = SaaSControlPlane(db_path=db_file)
    replay_after_restart = cp2.ingest_telemetry(payload)
    assert replay_after_restart["status"] == "duplicate_ignored"
    assert replay_after_restart["deduplicated"] is True
    cp2._store.close()

