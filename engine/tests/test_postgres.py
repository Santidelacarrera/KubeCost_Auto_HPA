"""Run against an isolated real database via KCA_TEST_POSTGRES_DSN."""
import concurrent.futures
import os
import uuid

import pytest

from kubecost_engine.postgres import PostgreSQLSaaSStore


@pytest.fixture
def stores():
    dsn = os.environ.get("KCA_TEST_POSTGRES_DSN")
    if not dsn:
        pytest.skip("isolated PostgreSQL not configured")
    stores = [PostgreSQLSaaSStore(dsn) for _ in range(4)]
    yield stores
    for store in stores:
        store.close()


def test_atomic_cas_and_tenant_isolation(stores):
    tenant = uuid.uuid4().hex
    stores[0].put_object(tenant, "plan", "p", {"id": "p", "status": "pending"})

    def approve(store):
        try:
            store.put_object(tenant, "plan", "p", {"id": "p", "status": "approved"}, 1)
            return True
        except ValueError:
            return False

    with concurrent.futures.ThreadPoolExecutor() as pool:
        assert sum(pool.map(approve, stores)) == 1
    assert stores[1].list_objects(tenant, "plan")[0]["version"] == 2
    assert stores[1].list_objects(tenant + "other", "plan") == []


def test_rate_counter_across_connections(stores):
    key = uuid.uuid4().hex

    def increment(i):
        return stores[i % len(stores)].increment_rate_counter(key, 60, 100)[0]

    with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
        assert sorted(pool.map(increment, range(100))) == list(range(1, 101))
    assert stores[0].increment_rate_counter(key, 60, 161) == (1, 60)


def test_postgres_retention_preserves_dedup_and_other_tenants(stores):
    tenant = uuid.uuid4().hex
    store = stores[0]
    store.save_telemetry_event(tenant, "expired", "cluster", 100, {"client_id": tenant})
    store.save_telemetry_event(tenant, "fresh", "cluster", 200, {"client_id": tenant})
    store.save_telemetry_event(tenant + "other", "expired", "cluster", 100, {})
    assert store.prune_telemetry(tenant, 150)["eligible_events"] == 1
    assert stores[1].prune_telemetry(tenant, 150, False)["deleted_events"] == 1
    assert not stores[2].save_telemetry_event(tenant, "expired", "cluster", 300, {})
    assert len(stores[2].telemetry_history(tenant, "cluster", 0)) == 1
    assert len(stores[2].telemetry_history(tenant + "other", "cluster", 0)) == 1


def test_financial_rollback_and_connection_reusable(stores):
    tenant = uuid.uuid4().hex
    store = stores[0]
    row = lambda record_id: {"provider": "focus", "record_id": record_id}
    store.import_financial_batch(tenant, "first", {"records": [row("a")]})
    with pytest.raises(ValueError):
        store.import_financial_batch(tenant, "second", {"records": [row("b"), row("a")]})
    store.import_financial_batch(tenant, "third", {"records": [row("b")]})
    assert len(stores[1].list_objects(tenant, "cost_import")) == 2


def test_all_legacy_entities_and_recovery(stores):
    tenant = uuid.uuid4().hex
    store = stores[0]
    assert store.save_telemetry_event(tenant, "e", "cluster", 100, {"client_id": tenant})
    assert not store.save_telemetry_event(tenant, "e", "cluster", 100, {})
    store.upsert_prewarm_command(tenant, "cmd", "cluster", "PENDING", 200, {"client_id": tenant})
    store.save_audit_decision(tenant, "d", "cluster", 100, {"client_id": tenant, "value": 1})
    store.save_audit_decision(tenant, "d", "cluster", 101, {"client_id": tenant, "value": 2})
    assert stores[1].list_audit_decisions(tenant)[0]["value"] == 2
    store.save_api_key("key", tenant, "prefix", tenant, ["enterprise:read"], 1, 200)
    store.save_api_key("key", tenant, "prefix", tenant, [], 1, 200, True)
    assert next(k for k in stores[1].load_all_api_keys() if k["client_id"] == tenant)["revoked"]
    assert store.record_stripe_event_if_new(tenant, tenant, "test", 100)
    assert not store.record_stripe_event_if_new(tenant, tenant, "test", 100)
    recovered = stores[1].recovery_records()
    for kind in ("telemetry", "commands", "audit"):
        assert any(r.get("client_id") == tenant for r in recovered[kind])


def test_single_control_plane_lease_release(stores):
    stores[0].acquire_control_plane()
    stores[0].acquire_control_plane()  # Must not stack session-lock acquisitions.
    with pytest.raises(RuntimeError, match="another control plane"):
        stores[1].acquire_control_plane()
    stores[0].close()
    stores[2].acquire_control_plane()


def test_repeated_immediate_control_plane_handoff(stores):
    dsn = os.environ["KCA_TEST_POSTGRES_DSN"]
    for _ in range(20):
        holder = PostgreSQLSaaSStore(dsn)
        holder.acquire_control_plane()
        holder.acquire_control_plane()
        holder.close()
        holder.close()  # Repeated shutdown remains harmless.
        successor = PostgreSQLSaaSStore(dsn)
        try:
            successor.acquire_control_plane()
        finally:
            successor.close()


def test_waiting_replica_promotes_and_recovers_committed_state(stores, monkeypatch):
    import threading
    from concurrent.futures import ThreadPoolExecutor
    import psycopg
    from psycopg import sql
    from psycopg.conninfo import make_conninfo
    from kubecost_engine.saas import SaaSControlPlane

    tenant = "ha-" + uuid.uuid4().hex
    dsn = os.environ["KCA_TEST_POSTGRES_DSN"]
    with psycopg.connect(dsn, autocommit=True) as connection:
        connection.execute(sql.SQL("CREATE SCHEMA {}").format(sql.Identifier(tenant)))
    monkeypatch.setenv("POSTGRES_DSN", make_conninfo(dsn, options=f"-csearch_path={tenant}"))
    holder = SaaSControlPlane()
    holder.register_agent({"client_id": tenant, "cluster_id": tenant})
    monkeypatch.setenv("POSTGRES_STANDBY", "true")
    waiting = threading.Event()
    from kubecost_engine import postgres
    sleep = postgres.time.sleep

    def mark_waiting(seconds):
        waiting.set()
        sleep(seconds)

    monkeypatch.setattr(postgres.time, "sleep", mark_waiting)
    with ThreadPoolExecutor(max_workers=1) as pool:
        successor = pool.submit(SaaSControlPlane)
        try:
            assert waiting.wait(10)
            assert not successor.done()
        finally:
            holder._store.close()
        promoted = successor.result(timeout=12)
    try:
        assert tenant in promoted._agents[tenant]
        assert promoted._store.check_connection()
    finally:
        promoted._store.close()


def test_sqlite_migration_restart_and_no_overwrite(stores, tmp_path, monkeypatch):
    import runpy
    from pathlib import Path
    import psycopg
    from psycopg import sql
    from psycopg.conninfo import make_conninfo
    from kubecost_engine.saas import SaaSControlPlane
    from kubecost_engine.auth_billing import MultiTenantAuthManager

    schema = "migration_" + uuid.uuid4().hex
    dsn = os.environ["KCA_TEST_POSTGRES_DSN"]
    with psycopg.connect(dsn, autocommit=True) as connection:
        connection.execute(sql.SQL("CREATE SCHEMA {}").format(sql.Identifier(schema)))
    target_dsn = make_conninfo(dsn, options=f"-csearch_path={schema}")
    source = tmp_path / "source.db"
    original = SaaSControlPlane(str(source))
    original.register_agent({"client_id": "migration-tenant", "cluster_id": "migration-cluster"})
    key = MultiTenantAuthManager(original._store).issue_api_key("migration-tenant")
    original._store.close()
    migrate = runpy.run_path(str(Path(__file__).resolve().parents[2] / "hack/migrate_sqlite_postgres.py"))["migrate"]
    result = migrate(source, target_dsn)
    assert result["tables"]["tenant_api_keys"] == 1
    with pytest.raises(ValueError, match="destination contains"):
        migrate(source, target_dsn)
    monkeypatch.setenv("POSTGRES_DSN", target_dsn)
    restored = SaaSControlPlane()
    assert "migration-cluster" in restored._agents["migration-tenant"]
    assert MultiTenantAuthManager(restored._store).verify_api_key(key["api_key"]).client_id == "migration-tenant"
    restored._store.close()
