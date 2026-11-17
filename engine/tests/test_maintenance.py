import time

import pytest

from kubecost_engine.maintenance import retain
from kubecost_engine.saas import SaaSControlPlane


def test_retention_preview_boundary_tenant_and_restart_dedup(tmp_path):
    path = str(tmp_path / "retention.db")
    cp = SaaSControlPlane(path)
    now = int(time.time())
    for tenant, cluster in (("a", "retention-a"), ("b", "retention-b")):
        for label, ts in (("old", now - 86401), ("boundary", now - 86400), ("new", now)):
            cp.ingest_telemetry({"client_id": tenant, "cluster_id": cluster, "event_id": tenant + label, "timestamp": ts, "metrics": {"rps": 1}})
    assert retain(cp, "a", 1, "admin", now=now)["eligible_events"] == 1
    assert len(cp.get_telemetry_events("a")) == 3
    result = retain(cp, "a", 1, "admin", dry_run=False, now=now)
    assert result["deleted_events"] == 1
    assert len(cp.get_telemetry_events("a")) == 2
    assert len(cp.get_telemetry_events("b")) == 3
    assert cp._store.list_objects("a", "maintenance")[0]["principal"] == "admin"
    cp._store.close()
    restored = SaaSControlPlane(path)
    assert len(restored.get_telemetry_events("a")) == 2
    replay = restored.ingest_telemetry({"client_id": "a", "cluster_id": "retention-a", "event_id": "aold", "timestamp": now, "metrics": {"rps": 1}})
    assert replay["status"] == "duplicate_ignored"
    assert retain(restored, "a", 1, "admin", dry_run=False, now=now)["deleted_events"] == 0
    restored._store.close()


@pytest.mark.parametrize("days,dry_run", [(0, True), (True, True), (1, "false"), (3651, False)])
def test_invalid_retention_never_deletes(days, dry_run):
    cp = SaaSControlPlane()
    with pytest.raises(ValueError):
        retain(cp, "a", days, "admin", dry_run=dry_run)
    cp._store.close()


def test_configured_retention_runs_on_ingestion(monkeypatch):
    cp = SaaSControlPlane()
    now = int(time.time())
    payload = {"client_id": "a", "cluster_id": "sweep", "event_id": "old", "timestamp": now - 3 * 86400, "metrics": {"rps": 1}}
    cp.ingest_telemetry(payload)
    monkeypatch.setenv("TELEMETRY_RETENTION_DAYS", "2")
    cp.ingest_telemetry({**payload, "event_id": "new", "timestamp": now})
    assert len(cp.get_telemetry_events("a")) == 1
    assert cp._store.list_objects("a", "maintenance")[0]["principal"] == "system:retention"
    cp._store.close()
