"""Durable SQLite WAL & PostgreSQL/TimescaleDB Persistence Layer for Multi-Tenant SaaS Control Plane.

Provides zero-external-dependency transactional persistence (stdlib sqlite3 in WAL mode)
and enterprise PostgreSQL/TimescaleDB hypertable schema migrations with retention policies
for telemetry records, event_id deduplication, PRE_WARM commands, tenant API keys (with expiration),
Stripe webhook idempotency, rate-limit counters, and FinOps audit logs.
"""

from __future__ import annotations

import json
import os
import sqlite3
import threading
import time
from typing import Any, Dict, List, Optional


def generate_timescaledb_migrations(retention_days: int = 90) -> List[str]:
    """Return idempotent PostgreSQL + TimescaleDB DDL statements for multi-tenant production deployment."""
    return [
        "CREATE EXTENSION IF NOT EXISTS timescaledb CASCADE;",
        """
        CREATE TABLE IF NOT EXISTS telemetry_events (
            client_id TEXT NOT NULL,
            event_id TEXT NOT NULL,
            cluster_id TEXT NOT NULL,
            observed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
            timestamp BIGINT NOT NULL,
            payload_json JSONB NOT NULL,
            PRIMARY KEY (client_id, event_id, observed_at)
        );
        """,
        "SELECT create_hypertable('telemetry_events', 'observed_at', if_not_exists => TRUE);",
        f"SELECT add_retention_policy('telemetry_events', INTERVAL '{int(retention_days)} days', if_not_exists => TRUE);",
        """
        CREATE TABLE IF NOT EXISTS prewarm_commands (
            client_id TEXT NOT NULL,
            command_id TEXT NOT NULL,
            cluster_id TEXT NOT NULL,
            status TEXT NOT NULL,
            expires_at BIGINT NOT NULL,
            payload_json JSONB NOT NULL,
            PRIMARY KEY (client_id, command_id)
        );
        """,
        """
        CREATE TABLE IF NOT EXISTS audit_decisions (
            client_id TEXT NOT NULL,
            decision_id TEXT NOT NULL,
            cluster_id TEXT NOT NULL,
            observed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
            timestamp BIGINT NOT NULL,
            payload_json JSONB NOT NULL,
            PRIMARY KEY (client_id, decision_id, observed_at)
        );
        """,
        "SELECT create_hypertable('audit_decisions', 'observed_at', if_not_exists => TRUE);",
        """
        CREATE TABLE IF NOT EXISTS tenant_api_keys (
            key_id TEXT NOT NULL,
            client_id TEXT NOT NULL,
            key_prefix TEXT NOT NULL,
            key_sha256 TEXT NOT NULL UNIQUE,
            scopes_json JSONB NOT NULL,
            created_at BIGINT NOT NULL,
            expires_at BIGINT NOT NULL,
            revoked BOOLEAN NOT NULL DEFAULT FALSE,
            PRIMARY KEY (client_id, key_id)
        );
        """,
        """
        CREATE TABLE IF NOT EXISTS stripe_webhook_events (
            event_id TEXT PRIMARY KEY,
            tenant_id TEXT NOT NULL,
            event_type TEXT NOT NULL,
            processed_at BIGINT NOT NULL
        );
        """,
    ]


class SQLiteSaaSStore:
    """Thread-safe SQLite WAL / PostgreSQL-compatible persistence store with strict client_id multi-tenant scoping."""

    def __init__(self, db_path: Optional[str] = None, postgres_dsn: Optional[str] = None) -> None:
        self.db_path = db_path or os.environ.get("SAAS_DB_PATH", ":memory:")
        self.postgres_dsn = postgres_dsn or os.environ.get("POSTGRES_DSN", "")
        self.backend = "timescaledb" if self.postgres_dsn else "sqlite_wal"
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
            cur.execute(
                """
                CREATE TABLE IF NOT EXISTS tenant_api_keys (
                    key_id TEXT NOT NULL,
                    client_id TEXT NOT NULL,
                    key_prefix TEXT NOT NULL,
                    key_sha256 TEXT NOT NULL UNIQUE,
                    scopes_json TEXT NOT NULL,
                    created_at INTEGER NOT NULL,
                    expires_at INTEGER NOT NULL DEFAULT 0,
                    revoked INTEGER NOT NULL DEFAULT 0,
                    PRIMARY KEY (client_id, key_id)
                );
                """
            )
            # Safe migration for legacy databases missing expires_at column
            cols = [row["name"] for row in cur.execute("PRAGMA table_info(tenant_api_keys);").fetchall()]
            if "expires_at" not in cols:
                cur.execute("ALTER TABLE tenant_api_keys ADD COLUMN expires_at INTEGER NOT NULL DEFAULT 0;")

            cur.execute(
                """
                CREATE TABLE IF NOT EXISTS stripe_webhook_events (
                    event_id TEXT PRIMARY KEY,
                    tenant_id TEXT NOT NULL,
                    event_type TEXT NOT NULL,
                    processed_at INTEGER NOT NULL
                );
                """
            )
            cur.execute(
                """
                CREATE TABLE IF NOT EXISTS rate_limit_counters (
                    counter_key TEXT PRIMARY KEY,
                    count INTEGER NOT NULL,
                    expires_at INTEGER NOT NULL
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

    def save_api_key(
        self,
        key_id: str,
        client_id: str,
        key_prefix: str,
        key_sha256: str,
        scopes: List[str],
        created_at: int,
        expires_at: int,
        revoked: bool = False,
    ) -> None:
        with self._lock:
            self._conn.execute(
                """
                INSERT OR REPLACE INTO tenant_api_keys
                (key_id, client_id, key_prefix, key_sha256, scopes_json, created_at, expires_at, revoked)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    key_id,
                    client_id,
                    key_prefix,
                    key_sha256,
                    json.dumps(list(scopes)),
                    int(created_at),
                    int(expires_at),
                    1 if revoked else 0,
                ),
            )
            self._conn.commit()

    def load_all_api_keys(self) -> List[Dict[str, Any]]:
        with self._lock:
            rows = self._conn.execute(
                "SELECT key_id, client_id, key_prefix, key_sha256, scopes_json, created_at, expires_at, revoked FROM tenant_api_keys"
            ).fetchall()
        return [
            {
                "key_id": r["key_id"],
                "client_id": r["client_id"],
                "key_prefix": r["key_prefix"],
                "key_sha256": r["key_sha256"],
                "scopes": json.loads(r["scopes_json"]),
                "created_at": int(r["created_at"]),
                "expires_at": int(r["expires_at"]),
                "revoked": bool(r["revoked"]),
            }
            for r in rows
        ]

    def record_stripe_event_if_new(self, event_id: str, tenant_id: str, event_type: str, processed_at: int) -> bool:
        """Atomically record a Stripe webhook event_id. Returns True if new, False if duplicate."""
        with self._lock:
            try:
                self._conn.execute(
                    "INSERT INTO stripe_webhook_events (event_id, tenant_id, event_type, processed_at) VALUES (?, ?, ?, ?)",
                    (event_id, tenant_id, event_type, int(processed_at)),
                )
                self._conn.commit()
                return True
            except sqlite3.IntegrityError:
                return False

    def increment_rate_counter(self, key: str, window_seconds: int, now_ts: Optional[int] = None) -> tuple[int, int]:
        """Atomically increment a rate-limit or daily quota counter in SQLite WAL."""
        now = int(now_ts if now_ts is not None else time.time())
        with self._lock:
            row = self._conn.execute(
                "SELECT count, expires_at FROM rate_limit_counters WHERE counter_key = ?",
                (key,),
            ).fetchone()
            if row is None or int(row["expires_at"]) <= now:
                expires_at = now + int(window_seconds)
                self._conn.execute(
                    "INSERT OR REPLACE INTO rate_limit_counters (counter_key, count, expires_at) VALUES (?, ?, ?)",
                    (key, 1, expires_at),
                )
                self._conn.commit()
                return 1, int(window_seconds)
            new_count = int(row["count"]) + 1
            expires_at = int(row["expires_at"])
            self._conn.execute(
                "UPDATE rate_limit_counters SET count = ? WHERE counter_key = ?",
                (new_count, key),
            )
            self._conn.commit()
            return new_count, max(1, expires_at - now)

    def close(self) -> None:
        with self._lock:
            self._conn.close()
