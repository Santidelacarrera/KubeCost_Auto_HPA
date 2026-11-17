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


def test_scheduled_retention_cleans_idle_tenants_and_stops(tmp_path, monkeypatch):
    from kubecost_engine import server
    from kubecost_engine.maintenance import RetentionWorker
    import threading

    cp = SaaSControlPlane(str(tmp_path / "idle.db"))
    cp.ingest_telemetry({"client_id": "idle", "cluster_id": "idle-cluster", "event_id": "expired", "timestamp": int(time.time()) - 3*86400, "metrics": {"rps": 1}})
    monkeypatch.setenv("TELEMETRY_RETENTION_DAYS", "2")
    monkeypatch.setenv("TELEMETRY_RETENTION_INTERVAL_SECONDS", "1")
    monkeypatch.setattr(server, "get_control_plane", lambda: cp)
    completed = threading.Event()
    original = RetentionWorker.run_once

    def run_once(worker):
        result = original(worker)
        completed.set()
        return result

    monkeypatch.setattr(RetentionWorker, "run_once", run_once)
    httpd = server.make_server("127.0.0.1", 0)
    try:
        assert completed.wait(5)
        assert cp.get_telemetry_events("idle") == []
        assert cp._store.list_objects("idle", "maintenance")[0]["principal"] == "system:scheduled-retention"
    finally:
        httpd.server_close()
    assert not httpd.retention_worker._thread.is_alive()
    cp._store.close()


def test_worker_isolates_failed_tenant_and_invalid_settings(monkeypatch):
    from kubecost_engine.maintenance import RetentionWorker, configured_worker
    from kubecost_engine.enterprise import EnterpriseService

    cp = SaaSControlPlane()
    for tenant in ("a", "b"):
        cp.ingest_telemetry({"client_id": tenant, "cluster_id": tenant + "-cluster", "event_id": tenant + "-expired", "timestamp": int(time.time())-3*86400, "metrics": {"rps": 1}})
    EnterpriseService(cp)._save("b", "license", {"status": "active", "claims": {"plan": "Starter", "expires_at": time.time()+100}})
    monkeypatch.setenv("REQUIRE_LICENSE", "true")
    assert len(RetentionWorker(cp, 2).run_once()) == 1
    assert len(cp.get_telemetry_events("a")) == 1
    assert cp.get_telemetry_events("b") == []
    monkeypatch.setenv("TELEMETRY_RETENTION_DAYS", "bad")
    with pytest.raises(ValueError):
        configured_worker(cp)
    cp._store.close()


def test_worker_repeats_after_idle_initial_scan_and_transient_failure(monkeypatch):
    import threading
    from kubecost_engine.maintenance import RetentionWorker

    cp = SaaSControlPlane()
    worker = RetentionWorker(cp, 2, interval_seconds=1)
    first = threading.Event()
    cleaned = threading.Event()
    original = cp._store.telemetry_tenants
    calls = []

    def tenants():
        calls.append(True)
        if len(calls) == 1:
            first.set()
            raise ConnectionError("simulated transient scan failure")
        return original()

    monkeypatch.setattr(cp._store, "telemetry_tenants", tenants)
    original_prune = cp._store.prune_telemetry

    def prune(*args):
        result = original_prune(*args)
        cleaned.set()
        return result

    monkeypatch.setattr(cp._store, "prune_telemetry", prune)
    worker.start()
    try:
        assert first.wait(5)
        cp._store.save_telemetry_event("idle", "expired", "idle-cluster", int(time.time())-3*86400, {})
        assert cleaned.wait(5)
    finally:
        worker.stop()
    assert len(calls) >= 2
    assert cp._store.telemetry_history("idle", "idle-cluster", 0) == []
    cp._store.close()
