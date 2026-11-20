import json
import threading
import time
import urllib.error
import urllib.request

import pytest

from kubecost_engine import server
from kubecost_engine.auth_billing import MultiTenantAuthManager
from kubecost_engine.saas import SaaSControlPlane
from kubecost_engine.security import DistributedRateLimiter


@pytest.fixture
def api(monkeypatch, tmp_path):
    monkeypatch.setenv("ENGINE_TOKEN", "local-test")
    cp = SaaSControlPlane(str(tmp_path / "api.db"))
    auth = MultiTenantAuthManager(cp._store)
    monkeypatch.setattr(server, "get_control_plane", lambda: cp)
    monkeypatch.setattr(server, "_auth_manager", auth)
    monkeypatch.setattr(
        server, "_rate_limiter", DistributedRateLimiter(sqlite_store=cp._store)
    )
    server.get_rate_limiter().reset_local()
    httpd = server.make_server("127.0.0.1", 0)
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()

    def request(path, payload=None, key="local-test"):
        req = urllib.request.Request(
            f"http://127.0.0.1:{httpd.server_port}/v1/{path}",
            data=json.dumps(payload).encode() if payload is not None else None,
            headers={
                "Authorization": "Bearer " + key,
                "Content-Type": "application/json",
            },
        )
        try:
            with urllib.request.urlopen(req) as res:
                return res.status, json.load(res)
        except urllib.error.HTTPError as exc:
            return exc.code, json.load(exc)

    yield request, auth, cp
    httpd.shutdown()
    httpd.server_close()
    thread.join(timeout=5)
    cp._store.close()


def test_versioned_enterprise_auth_scopes_and_tenant_isolation(api):
    request, auth, cp = api
    reader = auth.issue_api_key("a", scopes=["enterprise:read"])["api_key"]
    assert request("clusters", key="invalid")[0] == 401
    assert request("clusters", key=reader)[1]["items"] == []
    assert request("clusters?client_id=b", key=reader)[0] == 403
    assert request("rightsizing", {}, reader)[0] == 403
    assert request("policies", {"cluster_id": "private"}, reader)[0] == 403
    assert request("openapi.json", key=reader)[1]["openapi"] == "3.0.3"
    assert request("clusters?limit=500", key=reader)[0] == 400


def test_api_key_delegation_cannot_escalate(api):
    request, auth, _ = api
    manager = auth.issue_api_key("a", scopes=["keys:manage", "enterprise:read"])[
        "api_key"
    ]
    assert request("auth/keys", {"scopes": ["admin:*"]}, manager)[0] == 403
    assert request("auth/keys", {"scopes": ["enterprise:read"]}, manager)[0] == 200
    assert request("auth/keys", {}, manager)[0] == 403
    tenant_admin = auth.issue_api_key("a", scopes=["admin:*"])["api_key"]
    assert request("clusters?client_id=b", key=tenant_admin)[0] == 403


def test_actual_http_forecast_round_trip_and_financial_empty_state(api):
    request, _, cp = api
    now = time.time()
    payload = {
        "client_id": "a",
        "cluster_id": "lab",
        "workload": "default/api",
        "samples": [
            {"timestamp": now - (19 - i) * 60, "demand": 100 + i} for i in range(20)
        ],
    }
    status, prediction = request("predictions", payload)
    assert status == 200
    status, listing = request("predictions?client_id=a")
    assert listing["items"][0]["id"] == prediction["id"]
    assert request("predictions?client_id=b")[1]["items"] == []
    assert (
        request("finops/costs?client_id=a")[1]["savings"]["invoice_reconciled"] is None
    )
    assert request("system/readiness")[1]["certified_subsystems"] == 0


def test_financial_import_reconcile_and_duplicate_api(api):
    request, _, _ = api
    row = {
        "record_id": "line-1",
        "BilledCost": "10.10",
        "EffectiveCost": "9.00",
        "BillingCurrency": "USD",
        "ChargePeriodStart": "2026-10-01T00:00:00Z",
        "ChargePeriodEnd": "2026-11-01T00:00:00Z",
        "cluster_id": "lab",
    }
    payload = {"client_id": "a", "provider": "focus", "records": [row]}
    assert request("finops/import", payload)[1]["status"] == "imported"
    assert request("finops/import", payload)[0] == 400
    assert request("finops/costs?client_id=a")[1]["groups"][0]["billed_cost"] == "10.10"
    invoice = {
        "invoice_id": "test-invoice",
        "source_sha256": "a" * 64,
        "currency": "USD",
        "start": row["ChargePeriodStart"],
        "end": row["ChargePeriodEnd"],
        "total": "10.10",
    }
    status, result = request("finops/reconcile", {"client_id": "a", "invoice": invoice})
    assert status == 200
    assert result["status"] == "matched"
    assert result["invoice_reconciled_savings"] is None
    assert request("finops/reconciliations?client_id=b")[1]["items"] == []


def test_user_roles_disable_and_credential_rotation(api):
    request, auth, _ = api
    assert request("users", {"client_id": "a", "id": "alice", "role": "operator"})[0] == 200
    status, issued = request("users/credentials", {"client_id": "a", "id": "alice"})
    assert status == 200
    key = issued["api_key"]
    assert request("clusters", key=key)[0] == 200
    assert request("users", {"id": "mallory", "role": "owner"}, key)[0] == 403
    assert request("licenses/revoke", {}, key)[0] == 403
    assert request("users?client_id=b", key=key)[0] == 403
    rotated = auth.rotate_api_key("a", issued["key_id"])["api_key"]
    assert request("clusters", key=key)[0] == 401
    assert request("clusters", key=rotated)[0] == 200
    update = {"client_id": "a", "id": "alice", "role": "viewer", "status": "active", "version": 1}
    assert request("users/update", update)[0] == 200
    assert request("predictions", {}, rotated)[0] == 403
    assert request("users/update", {**update, "version": 2, "status": "disabled"})[0] == 200
    assert request("clusters", key=rotated)[0] == 401


def test_personal_profile_export_and_correction(api):
    request, auth, _ = api
    for tenant in ("a", "b"):
        assert request("users", {"client_id": tenant, "id": "alice", "role": "viewer"})[0] == 200
    key = request("users/credentials", {"client_id": "a", "id": "alice"})[1]["api_key"]
    status, profile = request("users/me", key=key)
    assert status == 200
    assert set(profile) == {"id", "display_name", "role", "status", "created_at", "version"}
    update = {"version": profile["version"], "display_name": "A."}
    status, updated = request("users/me", update, key)
    assert status == 200 and updated["display_name"] == "A."
    assert request("users/me", update, key)[0] == 400
    assert request("users/me", {**update, "version": updated["version"], "role": "owner"}, key)[0] == 400
    assert request("users/me", {**update, "client_id": "b"}, key)[0] == 403
    assert request("users/me?client_id=b", key=key)[0] == 403
    assert request("users/me", key=auth.issue_api_key("a", scopes=["admin:*"])["api_key"])[0] == 403
    assert request("users?client_id=b")[1]["items"][0]["display_name"] == "alice"


def test_decision_telemetry_uses_authenticated_tenant(api, monkeypatch):
    from unittest.mock import Mock
    from kubecost_engine import telemetry

    request, auth, _ = api
    key = auth.issue_api_key("a", scopes=["compute:evaluate"])["api_key"]
    publisher = Mock()
    monkeypatch.setattr(telemetry, "get_telemetry_client", lambda: publisher)
    monkeypatch.setenv("NEXUS_TELEMETRY_ENABLED", "true")
    decision = {"cpu_millicores": 200, "p99_ms": 20, "slo_p99_ms": 100}
    assert request("evaluate", decision, key)[0] == 200
    assert publisher.publish_scaling_decision.call_args.kwargs["tenant_id"] == "a"
    monkeypatch.setenv("NEXUS_TELEMETRY_ENABLED", "false")
    assert request("evaluate", decision, key)[0] == 200
    publisher.publish_scaling_decision.assert_called_once()


def test_operational_data_requires_read_scopes(api):
    request, auth, _ = api
    writer = auth.issue_api_key("a", scopes=["compute:evaluate"])["api_key"]
    for route, scope in (
        ("audit/decisions", "audit:read"), ("commands/pending", "commands:read"),
        ("policies", "policies:read"), ("telemetry/events", "telemetry:read"),
        ("ui/cast-dashboard", "telemetry:read"), ("billing/invoice", "billing:read"),
    ):
        assert request(route, key=writer)[0] == 403
        reader = auth.issue_api_key("a", scopes=[scope])["api_key"]
        assert request(route, key=reader)[0] == 200


def test_legacy_contract_matches_operational_http(api):
    request, _, _ = api
    paths = request("openapi.json")[1]["paths"]
    for route in ("pricing/lookup", "gpu/optimize", "finops/guardrail", "gameday/simulate"):
        status, result = request(route)
        assert status == 200
        schema = paths["/v1/" + route]["get"]["responses"]["200"]["content"]["application/json"]["schema"]
        assert set(schema["required"]) <= result.keys()
    command = paths["/v1/commands/prewarm"]["post"]["requestBody"]["content"]["application/json"]["schema"]
    assert set(command["required"]) == {"cluster_id", "workload"}
    planner = paths["/v1/cluster/binpack"]["post"]["requestBody"]["content"]["application/json"]["schema"]
    pod = planner["properties"]["nodes"]["items"]["properties"]["pods"]["items"]
    assert pod["properties"]["can_evict"]["type"] == "boolean"
    assert "200" not in paths["/v1/ui/recommendations/execute"]["post"]["responses"]


def test_two_credentials_of_same_user_cannot_self_approve(api):
    request, _, _ = api
    assert request("users", {"client_id": "a", "id": "alice", "role": "owner"})[0] == 200
    keys = [request("users/credentials", {"client_id": "a", "id": "alice"})[1]["api_key"] for _ in range(2)]
    status, plan = request("consolidation/plans", {"cluster_id": "lab", "nodes": [], "mode": "assisted"}, keys[0])
    assert status == 200
    assert request("consolidation/decision", {"id": plan["id"], "version": plan["version"], "decision": "approve"}, keys[1])[0] == 403
    assert request("auth/keys", {"scopes": ["enterprise:read"]}, keys[0])[0] == 403


def test_policy_booleans_and_offline_alerts(api):
    request, _, cp = api
    payload = {"client_id": "a", "cluster_id": "alerts-lab"}
    assert request("policies", {**payload, "enable_spot_drain": "false"})[0] == 400
    assert request("policies", payload)[1]["enable_spot_drain"] is False
    cp.register_agent(payload)
    cp._agents["a"]["alerts-lab"].last_heartbeat_at = time.time() - 180
    assert request("alerts?client_id=a")[1]["items"][0]["reason"] == "heartbeat_missing"
    assert request("alerts?client_id=b")[1]["total"] == 0
    alert = request("alerts?client_id=a")[1]["items"][0]
    assert request("alerts/ack", {"client_id": "a", "id": alert["id"], "version": 0})[0] == 200
    assert request("alerts?client_id=a")[1]["items"][0]["status"] == "acknowledged"
    assert request("alerts/ack", {"client_id": "b", "id": alert["id"]})[0] == 400
    cp._agents["a"]["alerts-lab"].last_heartbeat_at -= 1
    assert request("alerts?client_id=a")[1]["items"][0]["status"] == "active"


def test_retention_requires_explicit_scope_and_defaults_preview(api):
    request, auth, _ = api
    writer = auth.issue_api_key("a", scopes=["enterprise:write"])["api_key"]
    assert request("maintenance/retention", {"days": 1}, writer)[0] == 403
    assert request("maintenance/retention", {"client_id": "a", "days": 1})[1]["dry_run"] is True


def test_backtest_api_and_license_management_scope(api):
    request, auth, _ = api
    now = time.time()
    payload = {"client_id": "a", "cluster_id": "lab", "workload": "api", "samples": [{"timestamp": now - (29-i)*60, "demand": 100+i} for i in range(30)]}
    assert request("predictions/backtest", payload)[1]["evaluated_folds"] == 18
    assert request("predictions/backtests?client_id=a")[1]["total"] == 1
    writer = auth.issue_api_key("a", scopes=["enterprise:write"])["api_key"]
    assert request("licenses/activate", {"license": "invalid"}, writer)[0] == 403
