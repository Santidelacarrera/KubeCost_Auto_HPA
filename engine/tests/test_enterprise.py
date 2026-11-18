import base64
from concurrent.futures import ThreadPoolExecutor
import hashlib
import hmac
import json
import time

import pytest

from kubecost_engine.binpacking import NodeBinPacker, parse_nodes
from kubecost_engine.enterprise import EnterpriseService, verify_license
from kubecost_engine.finops import normalize_records, reconcile, summarize
from kubecost_engine.optimization import forecast, rightsizing
from kubecost_engine.persistence import SQLiteSaaSStore
from kubecost_engine.saas import SaaSControlPlane
from kubecost_engine.security import AuthorizationError


def inventory():
    return [
        {
            "name": name,
            "instance_type": "generic",
            "zone": "zone-a",
            "monthly_cost_usd": 100,
            "total_cpu_millicores": 4000,
            "total_memory_mib": 8000,
            "allocated_cpu_millicores": cpu,
            "allocated_memory_mib": mem,
            "pods": [
                {
                    "name": name + "-pod",
                    "namespace": "test",
                    "cpu_request_millicores": cpu,
                    "memory_request_mib": mem,
                }
            ],
        }
        for name, cpu, mem in (("target", 3000, 4000), ("source", 400, 1000))
    ]


@pytest.mark.parametrize(
    "change",
    [
        "headroom",
        "pvc",
        "local",
        "stateful",
        "affinity",
        "taint",
        "notready",
        "selector",
        "excluded",
        "incomplete",
    ],
)
def test_planner_blocks_unsafe_inventory(change):
    nodes = inventory()
    if change == "headroom":
        nodes[0]["allocated_cpu_millicores"] = 3200
    elif change in ("pvc", "local", "stateful", "affinity"):
        field = {
            "pvc": "has_persistent_volume",
            "local": "has_local_volume",
            "stateful": "is_stateful",
            "affinity": "has_complex_affinity",
        }[change]
        nodes[1]["pods"][0][field] = True
    elif change == "taint":
        nodes[0]["taints"] = ["dedicated=system:NoSchedule"]
    elif change == "notready":
        nodes[0]["ready"] = False
    elif change == "selector":
        nodes[1]["pods"][0]["node_selector"] = {"kubernetes.io/arch": "arm64"}
    elif change == "excluded":
        nodes[1]["excluded"] = True
    elif change == "incomplete":
        nodes[1]["pods"] = []
    assert NodeBinPacker().optimize_cluster(parse_nodes(nodes)).nodes_can_drain == []


def test_plan_capacity_and_input_contract():
    nodes = inventory()
    result = NodeBinPacker().optimize_cluster(parse_nodes(nodes))
    assert result.nodes_can_drain == ["source"]
    assert result.relocation_plans[0].to_node == "target"
    nodes[1]["pods"][0]["can_evict"] = "false"
    with pytest.raises(ValueError):
        parse_nodes(nodes)


def sizing_payload(now):
    return {
        "current": {
            "cpu_request_millicores": 1000,
            "cpu_limit_millicores": 2000,
            "memory_request_mib": 512,
            "memory_limit_mib": 1024,
        },
        "samples": [
            {"timestamp": now - 60 * (29 - i), "cpu_millicores": 200, "memory_mib": 300}
            for i in range(30)
        ],
    }


def test_rightsizing_oom_and_reduction_boundaries():
    data = sizing_payload(10000)
    result = rightsizing(data, now=10000)
    assert result["recommendation"]["cpu_request_millicores"] == 800
    data["samples"][-1]["oom_killed"] = True
    result = rightsizing(data, now=10000)
    assert result["recommendation"]["memory_limit_mib"] >= 1280
    data["samples"] = data["samples"][:2]
    assert rightsizing(data, now=10000)["status"] == "insufficient_data"


@pytest.mark.parametrize("value", [float("nan"), float("inf"), -1, True])
def test_rightsizing_rejects_invalid_measurements(value):
    data = sizing_payload(10000)
    data["samples"][0]["cpu_millicores"] = value
    with pytest.raises(ValueError):
        rightsizing(data, now=10000)


def test_forecast_requires_history_and_detects_repeated_cycles():
    assert forecast({"samples": []}, now=10000)["predicted_demand"] is None
    samples = [
        {"timestamp": 10000 - 60 * (71 - i), "demand": [100, 200, 300, 200][i % 4]}
        for i in range(72)
    ]
    result = forecast(
        {"samples": samples, "seasonal_period_samples": 4, "horizon_seconds": 60},
        now=10000,
    )
    assert result["seasonality_detected"] is True
    assert result["predicted_demand"] == 100
    assert result["confidence"] <= 0.95


def financial_row(record_id="one"):
    return {
        "record_id": record_id,
        "BilledCost": "10.10",
        "EffectiveCost": "8.50",
        "BillingCurrency": "USD",
        "ChargePeriodStart": "2026-10-01T00:00:00Z",
        "ChargePeriodEnd": "2026-11-01T00:00:00Z",
        "cluster_id": "test",
    }


def test_finance_exact_currency_separation_and_missing_records():
    a, b = financial_row(), financial_row("two")
    b["BillingCurrency"] = "EUR"
    result = normalize_records([a, b, {}], "focus")
    assert len(result["errors"]) == 1
    totals = summarize(result["records"])
    assert len(totals) == 2
    assert all(t["billed_cost"] == "10.10" for t in totals)
    match = reconcile(
        result["records"],
        {
            "invoice_id": "inv-1",
            "source_sha256": "a" * 64,
            "currency": "USD",
            "start": a["ChargePeriodStart"],
            "end": a["ChargePeriodEnd"],
            "total": "10.10",
        },
    )
    assert match["status"] == "matched"
    assert match["invoice_reconciled_savings"] is None


def test_provider_adapters_credits_and_rejected_nonfinite():
    gcp = {
        "record_id": "gcp-1",
        "cost": "10",
        "credits": [{"amount": "-2.5"}],
        "currency": "USD",
        "usage_start_time": "2026-10-01T00:00:00Z",
        "usage_end_time": "2026-10-02T00:00:00Z",
    }
    assert normalize_records([gcp], "gcp")["records"][0]["billed_cost"] == "7.5"
    azure = {**financial_row(), "CostInBillingCurrency": "NaN"}
    assert normalize_records([azure], "azure")["errors"]


def test_persistence_restart_and_tenant_ownership(tmp_path):
    db = str(tmp_path / "state.db")
    cp = SaaSControlPlane(db)
    cp.register_agent(
        {"client_id": "a", "cluster_id": "private", "cloud_provider": "gcp"}, "a"
    )
    cp.ingest_telemetry(
        {
            "client_id": "a",
            "cluster_id": "private",
            "event_id": "evt-1",
            "metrics": {"cpu_usage_cores": 1, "mem_usage_bytes": 100},
        },
        "a",
    )
    cp._store.close()
    restored = SaaSControlPlane(db)
    assert EnterpriseService(restored).get("clusters", "a", {})["total"] == 1
    assert len(restored.get_telemetry_events("a")) == 1
    with pytest.raises(AuthorizationError):
        restored.register_agent({"client_id": "b", "cluster_id": "private"}, "b")
    restored._store.close()


def test_approval_separation_expiration_and_restart(tmp_path, monkeypatch):
    cp = SaaSControlPlane(str(tmp_path / "state.db"))
    service = EnterpriseService(cp)
    payload = {"cluster_id": "test", "nodes": inventory(), "mode": "assisted"}
    plan = service.post("consolidation/plans", "a", payload, "requester")
    decision = {"id": plan["id"], "version": 1, "decision": "approve"}
    with pytest.raises(AuthorizationError):
        service.post("consolidation/decision", "a", decision, "requester")
    approved = service.post("consolidation/decision", "a", decision, "approver")
    assert approved["status"] == "approved"
    assert approved["execution_status"] == "not_dispatched"
    execution = {"id": plan["id"], "version": approved["version"], "node": approved["nodes_can_drain"][0], "action": "claim"}
    claimed = service.post("consolidation/execution", "a", execution, "executor")
    with pytest.raises(ValueError):
        service.post("consolidation/execution", "a", execution, "other")
    completion = {**execution, "version": claimed["version"], "action": "cordoned"}
    with pytest.raises(AuthorizationError):
        service.post("consolidation/execution", "a", completion, "other")
    completed = service.post("consolidation/execution", "a", completion, "executor")
    assert completed["execution_status"] == "cordoned"
    with pytest.raises(ValueError):
        service.post("consolidation/decision", "b", decision, "approver")
    with pytest.raises(ValueError):
        service.post("consolidation/decision", "a", decision, "approver")
    monkeypatch.delenv("ENABLE_AUTOMATED_CONSOLIDATION", raising=False)
    with pytest.raises(ValueError):
        service.post(
            "consolidation/plans", "a", {**payload, "mode": "automated"}, "requester"
        )
    cp._store.close()


def test_compare_and_swap_and_financial_import_races(tmp_path):
    db = str(tmp_path / "concurrent.db")
    stores = [SQLiteSaaSStore(db) for _ in range(2)]
    stores[0].put_object("a", "test", "1", {"id": "1"})

    def write(store):
        try:
            store.put_object("a", "test", "1", {"id": "1"}, 1)
            return True
        except ValueError:
            return False

    with ThreadPoolExecutor(2) as pool:
        assert sorted(pool.map(write, stores)) == [False, True]
    records = normalize_records([financial_row()], "focus")["records"]
    stores[0].import_financial_batch("a", "first", {"records": records})
    with pytest.raises(ValueError):
        stores[1].import_financial_batch("a", "second", {"records": records})
    assert len(stores[0].list_objects("a", "cost_import")) == 1
    for store in stores:
        store.close()


def test_signed_license_expiration_tamper_and_org():
    secret = "s" * 32
    claims = {
        "id": "license-1",
        "tenant": "a",
        "plan": "Starter",
        "issued_at": 100,
        "expires_at": 200,
    }
    body = base64.urlsafe_b64encode(json.dumps(claims).encode()).decode().rstrip("=")
    token = (
        body
        + "."
        + hmac.new(secret.encode(), body.encode(), hashlib.sha256).hexdigest()
    )
    assert verify_license(token, "a", secret, now=150) == claims
    for bad, org, now in (
        (token + "0", "a", 150),
        (token, "b", 150),
        (token, "a", 201),
    ):
        with pytest.raises(ValueError):
            verify_license(bad, org, secret, now=now)


def test_unsupported_postgres_does_not_claim_to_be_connected():
    with pytest.raises(ValueError, match="not supported"):
        SQLiteSaaSStore(postgres_dsn="postgresql://invalid")


def test_subscription_sandbox_lifecycle_and_idempotency(tmp_path):
    from kubecost_engine.subscriptions import transition
    from kubecost_engine.enterprise import PLANS

    store = SQLiteSaaSStore(str(tmp_path / "subscription.db"))
    request = {
        "action": "activate",
        "event_id": "one",
        "plan": "Starter",
        "amount": "100.01",
        "currency": "USD",
    }
    active = transition(store, "a", request, PLANS, now=100)
    assert active["status"] == "active"
    assert not active["grants_production_entitlements"]
    replay = transition(store, "a", request, PLANS, now=100)
    assert replay["version"] == 1
    assert replay["duplicate_ignored"]
    renewed = transition(
        store, "a", {**request, "action": "renew", "event_id": "two"}, PLANS, now=200
    )
    assert renewed["expires_at"] > active["expires_at"]
    cancelled = transition(
        store, "a", {"action": "cancel", "event_id": "three"}, PLANS, now=300
    )
    assert cancelled["status"] == "cancelled"
    assert len(cancelled["history"]) == 3
    assert cancelled["history"][0]["payment"]["live_transaction"] is False
    assert store.list_objects("b", "subscription") == []
    store.close()


def test_ack_survives_restart_and_command_cannot_be_rearmed(tmp_path):
    db = str(tmp_path / "commands.db")
    cp = SaaSControlPlane(db)
    request = {
        "client_id": "a",
        "cluster_id": "private",
        "workload": "default/app",
        "command_id": "cmd-1",
    }
    cp.issue_prewarm_command(request, "a")
    cp.acknowledge_command(
        {"client_id": "a", "command_id": "cmd-1", "status": "EXECUTED"},
        tenant_header="a",
    )
    cp._store.close()
    restored = SaaSControlPlane(db)
    assert restored.get_pending_commands("a", "private") == []
    assert restored.issue_prewarm_command(request, "a")["status"] != "PENDING"
    with pytest.raises(ValueError):
        restored.issue_prewarm_command({**request, "workload": "default/other"}, "a")
    restored._store.close()


def test_license_cluster_limit_and_feature_gate(monkeypatch):
    monkeypatch.setenv("REQUIRE_LICENSE", "true")
    cp = SaaSControlPlane()
    service = EnterpriseService(cp)
    with pytest.raises(AuthorizationError):
        service.enforce_license("a")
    service._save(
        "a", "license", {"status": "active", "claims": {"plan": "Starter", "expires_at": time.time() + 60}}
    )
    service.enforce_license("a", new_cluster=True)
    cp.register_agent({"client_id": "a", "cluster_id": "one"}, "a")
    with pytest.raises(AuthorizationError):
        service.enforce_license("a", new_cluster=True)
    with pytest.raises(AuthorizationError):
        service.enforce_license("a", "automated")


def test_expired_plan_and_observe_mode_cannot_be_approved(monkeypatch):
    cp = SaaSControlPlane()
    service = EnterpriseService(cp)
    for mode in ("observe-only", "assisted"):
        plan = service.post(
            "consolidation/plans",
            "a",
            {"cluster_id": "lab", "nodes": inventory(), "mode": mode},
            "requester",
        )
        if mode == "assisted":
            monkeypatch.setattr(
                "kubecost_engine.enterprise.time.time", lambda: plan["expires_at"] + 1
            )
        with pytest.raises(ValueError):
            service.post(
                "consolidation/decision",
                "a",
                {"id": plan["id"], "version": 1, "decision": "approve"},
                "approver",
            )
