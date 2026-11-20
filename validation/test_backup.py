import importlib.util
import json
from pathlib import Path
import sqlite3

import pytest
from kubecost_engine.auth_billing import MultiTenantAuthManager
from kubecost_engine.persistence import SQLiteSaaSStore
from kubecost_engine.security import AuthenticationError


@pytest.fixture
def backup_tool():
    spec = importlib.util.spec_from_file_location(
        "backup", Path(__file__).resolve().parents[1] / "hack/control_plane_backup.py"
    )
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def test_backup_with_uncheckpointed_wal_and_no_overwrite(tmp_path, backup_tool):
    source, backup, restored = (
        tmp_path / name for name in ("source.db", "backup.db", "restored.db")
    )
    with sqlite3.connect(source) as conn:
        conn.execute("PRAGMA journal_mode=WAL")
        conn.execute("CREATE TABLE evidence (id TEXT)")
        conn.execute("INSERT INTO evidence VALUES ('preserved')")
        conn.commit()
        assert backup_tool.copy_database(source, backup)["integrity"] == "ok"
    backup_tool.copy_database(backup, restored)
    with sqlite3.connect(restored) as conn:
        assert conn.execute("SELECT id FROM evidence").fetchone()[0] == "preserved"
    with pytest.raises(ValueError):
        backup_tool.copy_database(source, backup)


def test_restore_replays_only_reviewed_profile_decisions(tmp_path, backup_tool):
    source, backup, restored = (tmp_path / name for name in ("live.db", "old.db", "restored.db"))
    store = SQLiteSaaSStore(str(source))
    try:
        for tenant in ("a", "b"):
            store.put_object(tenant, "user", "operator", {
                "id": "operator", "created_at": 123, "status": "active",
                "display_name": "Original " + tenant, "role": "viewer", "history": [],
            })
        key = MultiTenantAuthManager(store).issue_api_key("a", scopes=["enterprise:read"])
        store.put_object("a", "user_credential", key["key_id"], {"user_id": "operator"})
        backup_tool.copy_database(source, backup)
    finally:
        store.close()

    decision = {"tenant": "a", "id": "operator", "created_at": 123, "action": "remove_display_name"}
    with pytest.raises(ValueError, match="creation timestamp"):
        backup_tool.copy_database(backup, restored, profile_decisions=[decision, {**decision, "created_at": 456}])
    assert not restored.exists()
    corrected = {"tenant": "b", "id": "operator", "created_at": 123, "action": "correct", "display_name": "Alias"}
    backup_tool.copy_database(backup, restored, profile_decisions=[decision, decision, corrected])
    store = SQLiteSaaSStore(str(restored))
    try:
        user = store.get_object("a", "user", "operator")
        assert (user["status"], user["display_name"], user["version"]) == ("disabled", "[removed]", 2)
        with pytest.raises(AuthenticationError):
            MultiTenantAuthManager(store).verify_api_key(key["api_key"])
        other = store.get_object("b", "user", "operator")
        assert (other["status"], other["display_name"]) == ("active", "Alias")
    finally:
        store.close()


def test_newer_revocation_inventory_applies_to_older_snapshot(tmp_path, backup_tool):
    live, backup, manifest, restored = (tmp_path / n for n in ("live", "backup", "revocations.json", "restored"))
    store = SQLiteSaaSStore(str(live))
    try:
        manager = MultiTenantAuthManager(store)
        old = manager.issue_api_key("a")
        keep = manager.issue_api_key("b")
        backup_tool.copy_database(live, backup)
        manager.rotate_api_key("a", old["key_id"])
        with sqlite3.connect(live) as connection:
            assert backup_tool.export_revocations(connection, manifest) == {"revoked_keys": 1}
        entries = json.loads(manifest.read_text(encoding="utf-8"))
        assert entries == [{"tenant": "a", "key_id": old["key_id"], "created_at": old["created_at"]}]
        backup_tool.copy_database(backup, restored, revocations=entries)
    finally:
        store.close()
    store = SQLiteSaaSStore(str(restored))
    try:
        manager = MultiTenantAuthManager(store)
        with pytest.raises(AuthenticationError):
            manager.verify_api_key(old["api_key"])
        assert manager.verify_api_key(keep["api_key"]).client_id == "b"
    finally:
        store.close()
