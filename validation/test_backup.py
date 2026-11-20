import importlib.util
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
