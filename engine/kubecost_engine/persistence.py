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
        self.postgres_dsn = os.environ.get("POSTGRES_DSN", "") if postgres_dsn is None else postgres_dsn
        if self.postgres_dsn:
            raise ValueError("POSTGRES_DSN is not supported by SQLiteSaaSStore; PostgreSQL DDL is preparatory only")
        self.backend = "sqlite_wal"
        self._lock = threading.Lock()
        self._conn = sqlite3.connect(self.db_path, check_same_thread=False)
        self._conn.row_factory = sqlite3.Row
        self._init_schema()

    def _init_schema(self) -> None:
        with self._lock:
            cur = self._conn.cursor()
            cur.execute("PRAGMA journal_mode=WAL;")
            cur.execute("PRAGMA busy_timeout=5000;")
            cur.execute("CREATE TABLE IF NOT EXISTS enterprise_objects (tenant TEXT NOT NULL, kind TEXT NOT NULL, id TEXT NOT NULL, version INTEGER NOT NULL, payload TEXT NOT NULL, PRIMARY KEY(tenant,kind,id))")
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
            self._conn.execute("CREATE TABLE IF NOT EXISTS telemetry_tombstones (client_id TEXT NOT NULL, event_id TEXT NOT NULL, PRIMARY KEY(client_id,event_id))")
            if self._conn.execute("SELECT event_id FROM telemetry_tombstones WHERE client_id=? AND event_id=?", (client_id, event_id)).fetchone():
                return False
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
                INSERT INTO audit_decisions (client_id, decision_id, cluster_id, timestamp, payload_json)
                VALUES (?, ?, ?, ?, ?)
                ON CONFLICT(client_id, decision_id) DO UPDATE SET
                    cluster_id=excluded.cluster_id, timestamp=excluded.timestamp, payload_json=excluded.payload_json
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
                INSERT INTO tenant_api_keys
                (key_id, client_id, key_prefix, key_sha256, scopes_json, created_at, expires_at, revoked)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?)
                ON CONFLICT(client_id, key_id) DO UPDATE SET
                    key_prefix=excluded.key_prefix, key_sha256=excluded.key_sha256,
                    scopes_json=excluded.scopes_json, created_at=excluded.created_at,
                    expires_at=excluded.expires_at, revoked=excluded.revoked
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

    def telemetry_history(self, tenant, cluster, since, limit=10000):
        if not 1 <= limit <= 10000:
            raise ValueError("history limit must be in [1,10000]")
        with self._lock:
            rows = self._conn.execute(
                "SELECT payload_json FROM telemetry_events WHERE client_id=? AND cluster_id=? AND timestamp>=? ORDER BY timestamp DESC, event_id LIMIT ?",
                (tenant, cluster, int(since), limit + 1),
            ).fetchall()
        if len(rows) > limit:
            raise ValueError("telemetry history exceeds limit; narrow the window")
        return [json.loads(row["payload_json"]) for row in reversed(rows)]

    def telemetry_inventory(self, tenant):
        fields = ("cluster_id", "node_name", "namespace", "workload_name")
        expressions = [f"payload_json::json->>'{field}' AS {field}" if self.backend == "postgresql" else f"json_extract(payload_json, '$.{field}') AS {field}" for field in fields]
        with self._lock:
            rows = self._conn.execute("SELECT DISTINCT " + ",".join(expressions) + " FROM telemetry_events WHERE client_id=?", (tenant,)).fetchall()
        return [{field: row[field] for field in fields} for row in rows]

    def prune_telemetry(self, tenant, cutoff, dry_run=True):
        with self._lock, self._conn:
            count = self._conn.execute("SELECT COUNT(*) AS n FROM telemetry_events WHERE client_id=? AND timestamp<?", (tenant, cutoff)).fetchone()["n"]
            if not dry_run:
                self._conn.execute("CREATE TABLE IF NOT EXISTS telemetry_tombstones (client_id TEXT NOT NULL, event_id TEXT NOT NULL, PRIMARY KEY(client_id,event_id))")
                self._conn.execute("INSERT INTO telemetry_tombstones SELECT client_id,event_id FROM telemetry_events WHERE client_id=? AND timestamp<? ON CONFLICT(client_id,event_id) DO NOTHING", (tenant, cutoff))
                self._conn.execute("DELETE FROM telemetry_events WHERE client_id=? AND timestamp<?", (tenant, cutoff))
        return {"cutoff": cutoff, "eligible_events": count, "deleted_events": 0 if dry_run else count, "deduplication_retained": True}

    def list_objects(self, tenant: str, kind: str) -> List[Dict[str, Any]]:
        with self._lock:
            rows = self._conn.execute("SELECT payload, version FROM enterprise_objects WHERE tenant=? AND kind=? ORDER BY id", (tenant, kind)).fetchall()
        return [{**json.loads(r["payload"]), "version": r["version"]} for r in rows]

    def recovery_records(self):
        """Internal startup-only access; never exposed through an HTTP route."""
        with self._lock:
            runtime = self._conn.execute("SELECT tenant,payload FROM enterprise_objects WHERE kind='runtime'").fetchall()
            telemetry = self._conn.execute("SELECT payload_json FROM telemetry_events ORDER BY timestamp").fetchall()
            commands = self._conn.execute("SELECT payload_json FROM prewarm_commands").fetchall()
            audit = self._conn.execute("SELECT payload_json FROM audit_decisions").fetchall()
        return {"runtime": [(r["tenant"], json.loads(r["payload"])) for r in runtime], "telemetry": [json.loads(r["payload_json"]) for r in telemetry], "commands": [json.loads(r["payload_json"]) for r in commands], "audit": [json.loads(r["payload_json"]) for r in audit]}

    def import_financial_batch(self, tenant, batch_id, payload):
        """Unique row identities and batch payload commit in the same transaction."""
        with self._lock, self._conn:
            self._conn.execute("CREATE TABLE IF NOT EXISTS financial_record_ids (tenant TEXT NOT NULL, provider TEXT NOT NULL, record_id TEXT NOT NULL, PRIMARY KEY(tenant,provider,record_id))")
            try:
                for row in payload["records"]:
                    self._conn.execute("INSERT INTO financial_record_ids VALUES (?,?,?)", (tenant, row["provider"], row["record_id"]))
                self._conn.execute("INSERT INTO enterprise_objects VALUES (?,?,?,?,?)", (tenant, "cost_import", batch_id, 1, json.dumps(payload, allow_nan=False)))
            except sqlite3.IntegrityError as exc:
                raise ValueError("financial records already imported") from exc
        return {**payload, "version": 1}

    def put_object(self, tenant: str, kind: str, object_id: str, payload: Dict[str, Any], expected_version: int = 0) -> Dict[str, Any]:
        """Compare-and-swap prevents approval races and lost updates across connections."""
        encoded = json.dumps(payload, allow_nan=False)
        with self._lock, self._conn:
            if expected_version == 0:
                try:
                    self._conn.execute("INSERT INTO enterprise_objects VALUES (?,?,?,?,?)", (tenant, kind, object_id, 1, encoded))
                except sqlite3.IntegrityError as exc:
                    raise ValueError("object already exists; reload its version") from exc
            else:
                cur = self._conn.execute("UPDATE enterprise_objects SET version=version+1, payload=? WHERE tenant=? AND kind=? AND id=? AND version=?", (encoded, tenant, kind, object_id, expected_version))
                if cur.rowcount != 1:
                    raise ValueError("object version conflict; reload before retry")
        return {**payload, "version": expected_version + 1}
