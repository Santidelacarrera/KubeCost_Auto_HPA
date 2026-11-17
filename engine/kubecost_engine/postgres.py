"""PostgreSQL store using the same entity contract as SQLite.

SQL parameters remain bound. The small connection adapter only changes the DB-API
placeholder syntax and normalizes integrity errors for inherited store methods.
This backend does not make the control plane's process-local caches HA safe.
"""
from __future__ import annotations

import sqlite3
import threading
import time

from .persistence import SQLiteSaaSStore


class _Connection:
    def __init__(self, conn, integrity_error):
        self.conn = conn
        self.integrity_error = integrity_error

    def execute(self, sql, params=()):
        try:
            return self.conn.execute(sql.replace("?", "%s"), params)
        except self.integrity_error as exc:
            raise sqlite3.IntegrityError("database integrity constraint violated") from exc

    def commit(self):
        # Autocommit for individual statements; __enter__ groups atomic batches.
        pass

    def __enter__(self):
        self.transaction = self.conn.transaction()
        self.transaction.__enter__()
        return self

    def __exit__(self, *args):
        return self.transaction.__exit__(*args)

    def close(self):
        self.conn.close()


class PostgreSQLSaaSStore(SQLiteSaaSStore):
    def acquire_control_plane(self):
        """Fail closed until runtime caches support multiple simultaneous writers."""
        with self._lock:
            row = self._conn.execute("SELECT pg_try_advisory_lock(1262698833) AS acquired").fetchone()
            if not row["acquired"]:
                self._conn.close()
                raise RuntimeError("another control plane is active; multi-replica runtime is not supported")

    def __init__(self, dsn):
        import psycopg
        from psycopg.rows import dict_row

        self.backend = "postgresql"
        self.db_path = None
        self._lock = threading.Lock()
        connection = psycopg.connect(dsn, autocommit=True, row_factory=dict_row,
                                     connect_timeout=10)
        self._conn = _Connection(connection, psycopg.IntegrityError)
        try:
            self._init_schema()
        except Exception:
            connection.close()
            raise

    def _init_schema(self):
        tables = {
            "enterprise_objects": "tenant TEXT NOT NULL, kind TEXT NOT NULL, id TEXT NOT NULL, version BIGINT NOT NULL, payload TEXT NOT NULL, PRIMARY KEY(tenant,kind,id)",
            "telemetry_events": "client_id TEXT NOT NULL, event_id TEXT NOT NULL, cluster_id TEXT NOT NULL, timestamp BIGINT NOT NULL, payload_json TEXT NOT NULL, PRIMARY KEY(client_id,event_id)",
            "prewarm_commands": "client_id TEXT NOT NULL, command_id TEXT NOT NULL, cluster_id TEXT NOT NULL, status TEXT NOT NULL, expires_at BIGINT NOT NULL, payload_json TEXT NOT NULL, PRIMARY KEY(client_id,command_id)",
            "audit_decisions": "client_id TEXT NOT NULL, decision_id TEXT NOT NULL, cluster_id TEXT NOT NULL, timestamp BIGINT NOT NULL, payload_json TEXT NOT NULL, PRIMARY KEY(client_id,decision_id)",
            "tenant_api_keys": "key_id TEXT NOT NULL, client_id TEXT NOT NULL, key_prefix TEXT NOT NULL, key_sha256 TEXT NOT NULL UNIQUE, scopes_json TEXT NOT NULL, created_at BIGINT NOT NULL, expires_at BIGINT NOT NULL DEFAULT 0, revoked INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(client_id,key_id)",
            "stripe_webhook_events": "event_id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, event_type TEXT NOT NULL, processed_at BIGINT NOT NULL",
            "rate_limit_counters": "counter_key TEXT PRIMARY KEY, count BIGINT NOT NULL, expires_at BIGINT NOT NULL",
            "financial_record_ids": "tenant TEXT NOT NULL, provider TEXT NOT NULL, record_id TEXT NOT NULL, PRIMARY KEY(tenant,provider,record_id)",
        }
        with self._lock, self._conn:
            # Serialize first startup and subsequent migrations across processes.
            self._conn.execute("SELECT pg_advisory_xact_lock(1262698832)")
            self._conn.execute("CREATE TABLE IF NOT EXISTS kca_schema_migrations (version INTEGER PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW())")
            versions = self._conn.execute("SELECT version FROM kca_schema_migrations").fetchall()
            if any(row["version"] > 2 for row in versions):
                raise ValueError("database schema is newer than this engine")
            if not versions:
                for name, definition in tables.items():
                    self._conn.execute(f"CREATE TABLE IF NOT EXISTS {name} ({definition})")
                self._conn.execute("CREATE INDEX IF NOT EXISTS telemetry_tenant_time ON telemetry_events(client_id,timestamp)")
                self._conn.execute("INSERT INTO kca_schema_migrations(version) VALUES (1)")
            self._conn.execute("CREATE TABLE IF NOT EXISTS telemetry_tombstones (client_id TEXT NOT NULL, event_id TEXT NOT NULL, PRIMARY KEY(client_id,event_id))")
            self._conn.execute("INSERT INTO kca_schema_migrations(version) VALUES (2) ON CONFLICT(version) DO NOTHING")

    def increment_rate_counter(self, key, window_seconds, now_ts=None):
        if window_seconds <= 0:
            raise ValueError("window_seconds must be positive")
        now = int(time.time() if now_ts is None else now_ts)
        with self._lock:
            row = self._conn.execute("""
                INSERT INTO rate_limit_counters(counter_key,count,expires_at) VALUES (?,1,?)
                ON CONFLICT(counter_key) DO UPDATE SET
                    count=CASE WHEN rate_limit_counters.expires_at<=? THEN 1 ELSE rate_limit_counters.count+1 END,
                    expires_at=CASE WHEN rate_limit_counters.expires_at<=? THEN excluded.expires_at ELSE rate_limit_counters.expires_at END
                RETURNING count,expires_at
            """, (key, now + int(window_seconds), now, now)).fetchone()
        return int(row["count"]), max(1, int(row["expires_at"]) - now)


def create_store(db_path=None):
    import os

    dsn = os.environ.get("POSTGRES_DSN", "")
    if dsn:
        return PostgreSQLSaaSStore(dsn)
    return SQLiteSaaSStore(db_path=db_path)
