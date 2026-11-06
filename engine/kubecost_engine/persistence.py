"""Durable SQLite WAL Persistence Layer for Multi-Tenant SaaS Control Plane.

Provides zero-external-dependency transactional persistence (stdlib sqlite3 in WAL mode)
for telemetry records, event_id deduplication, PRE_WARM commands, and FinOps audit logs
across process restarts.
"""

from __future__ import annotations

import json
import os
import sqlite3
import threading
from typing import Any, Dict, List, Optional


class SQLiteSaaSStore:
    """Thread-safe SQLite persistence store with strict client_id multi-tenant scoping."""

    def __init__(self, db_path: Optional[str] = None) -> None:
        self.db_path = db_path or os.environ.get("SAAS_DB_PATH", ":memory:")
        self._lock = threading.Lock()
        self._conn = sqlite3.connect(self.db_path, check_same_thread=False)
        self._conn.row_factory = sqlite3.Row
        self._init_schema()

    def _init_schema(self) -> None:
        with self._lock:
            cur = self._conn.cursor()
            cur.execute("PRAGMA journal_mode=WAL;")
            cur.execute(
                """
                CREATE TABLE IF NOT EXISTS telemetry_events (
                    client_id TEXT NOT NULL,
                    event_id TEXT NOT NULL,
                    cluster_id TEXT NOT NULL,
                    timestamp INTEGER NOT NULL,
                    payload_json TEXT NOT NULL,
                    PRIMARY KEY (client_id, event_id)
                );
                """
            )
            cur.execute(
                """
                CREATE TABLE IF NOT EXISTS prewarm_commands (
                    client_id TEXT NOT NULL,
                    command_id TEXT NOT NULL,
                    cluster_id TEXT NOT NULL,
                    status TEXT NOT NULL,
                    expires_at INTEGER NOT NULL,
                    payload_json TEXT NOT NULL,
                    PRIMARY KEY (client_id, command_id)
                );
                """
            )
            cur.execute(
                """
                CREATE TABLE IF NOT EXISTS audit_decisions (
                    client_id TEXT NOT NULL,
                    decision_id TEXT NOT NULL,
                    cluster_id TEXT NOT NULL,
                    timestamp INTEGER NOT NULL,
                    payload_json TEXT NOT NULL,
                    PRIMARY KEY (client_id, decision_id)
                );
                """
            )
            self._conn.commit()

    def save_telemetry_event(
        self, client_id: str, event_id: str, cluster_id: str, timestamp: int, record_dict: Dict[str, Any]
    ) -> bool:
        """Inserts telemetry event atomically. Returns True if inserted, False if duplicate event_id."""
        with self._lock:
            try:
                self._conn.execute(
                    "INSERT INTO telemetry_events (client_id, event_id, cluster_id, timestamp, payload_json) VALUES (?, ?, ?, ?, ?)",
                    (client_id, event_id, cluster_id, int(timestamp), json.dumps(record_dict)),
                )
                self._conn.commit()
                return True
            except sqlite3.IntegrityError:
                return False

    def upsert_prewarm_command(
        self, client_id: str, command_id: str, cluster_id: str, status: str, expires_at: int, cmd_dict: Dict[str, Any]
    ) -> None:
        with self._lock:
            self._conn.execute(
                """
                INSERT INTO prewarm_commands (client_id, command_id, cluster_id, status, expires_at, payload_json)
                VALUES (?, ?, ?, ?, ?, ?)
                ON CONFLICT(client_id, command_id) DO UPDATE SET
                    status=excluded.status,
                    payload_json=excluded.payload_json
                """,
                (client_id, command_id, cluster_id, status, int(expires_at), json.dumps(cmd_dict)),
            )
            self._conn.commit()

    def save_audit_decision(
        self, client_id: str, decision_id: str, cluster_id: str, timestamp: int, entry_dict: Dict[str, Any]
    ) -> None:
        with self._lock:
            self._conn.execute(
                """
                INSERT OR REPLACE INTO audit_decisions (client_id, decision_id, cluster_id, timestamp, payload_json)
                VALUES (?, ?, ?, ?, ?)
                """,
                (client_id, decision_id, cluster_id, int(timestamp), json.dumps(entry_dict)),
            )
            self._conn.commit()

    def list_audit_decisions(self, client_id: str, cluster_id: Optional[str] = None) -> List[Dict[str, Any]]:
        with self._lock:
            if cluster_id:
                rows = self._conn.execute(
                    "SELECT payload_json FROM audit_decisions WHERE client_id = ? AND cluster_id = ? ORDER BY timestamp ASC",
                    (client_id, cluster_id),
                ).fetchall()
            else:
                rows = self._conn.execute(
                    "SELECT payload_json FROM audit_decisions WHERE client_id = ? ORDER BY timestamp ASC",
                    (client_id,),
                ).fetchall()
        return [json.loads(r["payload_json"]) for r in rows]

    def close(self) -> None:
        with self._lock:
            self._conn.close()
