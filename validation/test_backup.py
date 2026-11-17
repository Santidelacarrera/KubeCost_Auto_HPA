import importlib.util
from pathlib import Path
import sqlite3

import pytest


def test_backup_with_uncheckpointed_wal_and_no_overwrite(tmp_path):
    spec = importlib.util.spec_from_file_location(
        "backup", Path(__file__).resolve().parents[1] / "hack/control_plane_backup.py"
    )
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    source, backup, restored = (
        tmp_path / name for name in ("source.db", "backup.db", "restored.db")
    )
    with sqlite3.connect(source) as conn:
        conn.execute("PRAGMA journal_mode=WAL")
        conn.execute("CREATE TABLE evidence (id TEXT)")
        conn.execute("INSERT INTO evidence VALUES ('preserved')")
        conn.commit()
        assert module.copy_database(source, backup)["integrity"] == "ok"
    module.copy_database(backup, restored)
    with sqlite3.connect(restored) as conn:
        assert conn.execute("SELECT id FROM evidence").fetchone()[0] == "preserved"
    with pytest.raises(ValueError):
        module.copy_database(source, backup)
