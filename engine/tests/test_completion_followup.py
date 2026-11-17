import base64
import hashlib
import hmac
import json
import time

import pytest

from kubecost_engine.enterprise import EnterpriseService
from kubecost_engine.optimization import backtest
from kubecost_engine.saas import SaaSControlPlane
from kubecost_engine.security import AuthorizationError


def test_backtest_does_not_leak_future_and_reports_coverage():
    samples = [{"timestamp": 10000 + i * 60, "demand": 100 + i} for i in range(40)]
    payload = {"samples": samples}
    result = backtest(payload, now=13000)
    assert result["evaluated_folds"] == 28
    assert result["mae"] == 0
    assert result["naive_mae"] == 1
    assert result["empirical_interval_coverage"] == 1
    samples[-1]["demand"] = 10000
    changed = backtest(payload, now=13000)
    assert result["folds"][:-1] == changed["folds"][:-1]
    assert changed["folds"][-1]["predicted"] == result["folds"][-1]["predicted"]
    assert changed["mae"] > 0
    assert all(f["training_end"] < f["target_timestamp"] for f in result["folds"])


def test_backtest_bounds_and_empty():
    assert backtest({"samples": []})["mae"] is None
    with pytest.raises(ValueError):
        backtest({"samples": [], "stride": 0})
    with pytest.raises(ValueError):
        backtest({"samples": [{"timestamp": i * 60, "demand": 1} for i in range(200)]}, now=12000)


def test_license_revocation_survives_restart_and_token_replay(tmp_path, monkeypatch):
    secret = "s" * 32
    monkeypatch.setenv("LICENSE_SIGNING_SECRET", secret)
    monkeypatch.setenv("REQUIRE_LICENSE", "true")
    claims = {"id": "license-test", "tenant": "a", "plan": "Starter", "issued_at": time.time() - 10, "expires_at": time.time() + 3600}
    body = base64.urlsafe_b64encode(json.dumps(claims).encode()).decode().rstrip("=")
    token = body + "." + hmac.new(secret.encode(), body.encode(), hashlib.sha256).hexdigest()
    path = str(tmp_path / "licenses.db")
    cp = SaaSControlPlane(path)
    service = EnterpriseService(cp)
    first = service.post("licenses/activate", "a", {"license": token}, "admin")
    assert service.post("licenses/activate", "a", {"license": token}, "admin") == first
    service.enforce_license("a")
    service.post("licenses/revoke", "a", {"id": first["id"], "version": first["version"], "reason": "contract ended"}, "admin")
    cp._store.close()
    cp = SaaSControlPlane(path)
    service = EnterpriseService(cp)
    with pytest.raises(AuthorizationError):
        service.enforce_license("a")
    with pytest.raises(AuthorizationError):
        service.post("licenses/activate", "a", {"license": token}, "admin")
    assert service.get("licenses", "a", {})["items"][0]["status"] == "revoked"
    cp._store.close()


def test_persisted_telemetry_forecast_restart_isolation_and_conflicts(tmp_path):
    path = str(tmp_path / "history.db")
    cp = SaaSControlPlane(path)
    now = int(time.time())
    for i in range(30):
        cp.ingest_telemetry({"client_id": "a", "cluster_id": "private-history", "event_id": f"e-{i}", "timestamp": now - (29 - i) * 60,
                             "namespace": "default", "workload_name": "api", "pod_name": "api-one", "container_name": "app",
                             "metrics": {"rps": 100 + i, "cpu_usage_cores": 0.2, "mem_usage_bytes": 104857600}}, "a")
    cp._store.close()

    cp = SaaSControlPlane(path)
    service = EnterpriseService(cp)
    payload = {"source": "telemetry", "cluster_id": "private-history", "workload": "default/api", "namespace": "default", "pod_name": "api-one", "container_name": "app"}
    result = service.post("predictions", "a", payload, "test")
    assert result["provenance"]["sample_count"] == 30
    assert result["status"] == "ready"
    assert service.post("predictions/backtest", "a", payload, "test")["evaluated_folds"] == 18
    assert service.post("predictions", "a", {**payload, "pod_name": "other"}, "test")["status"] == "insufficient_data"
    with pytest.raises(AuthorizationError):
        service.post("predictions", "b", payload, "test")
    cp.ingest_telemetry({"client_id": "a", "cluster_id": "private-history", "event_id": "conflicting", "timestamp": now,
                         "namespace": "default", "workload_name": "api", "pod_name": "api-one", "container_name": "app", "metrics": {"rps": 999}}, "a")
    with pytest.raises(ValueError, match="conflicting"):
        service.post("predictions", "a", payload, "test")
    cp._store.close()


def test_inventory_quotas_use_durable_inventory(tmp_path, monkeypatch):
    from kubecost_engine.enterprise import PLANS

    monkeypatch.setenv("REQUIRE_LICENSE", "true")
    monkeypatch.setitem(PLANS, "Starter", {**PLANS["Starter"], "nodes": 1, "workloads": 1})
    cp = SaaSControlPlane(str(tmp_path / "quota.db"))
    service = EnterpriseService(cp)
    service._save("a", "license", {"status": "active", "claims": {"plan": "Starter", "expires_at": time.time() + 100}})
    cp.register_agent({"client_id": "a", "cluster_id": "quota"})
    event = {"client_id": "a", "cluster_id": "quota", "node_name": "n1", "workload_name": "api", "metrics": {"rps": 1}, "event_id": "q1"}
    cp.ingest_telemetry(event)
    cp._telemetry.clear()  # Simulate eviction from the bounded in-memory cache.
    with pytest.raises(AuthorizationError):
        cp.ingest_telemetry({**event, "node_name": "n2", "event_id": "q2"})
    with pytest.raises(AuthorizationError):
        cp.ingest_telemetry({**event, "workload_name": "another", "event_id": "q3"})
    assert len(cp._store.telemetry_history("a", "quota", 0)) == 1
    cp._store.close()
