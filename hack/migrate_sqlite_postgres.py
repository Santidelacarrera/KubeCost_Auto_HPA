"""Copy a consistent SQLite snapshot to an EMPTY PostgreSQL application database.

Stop the source control plane before cutover. Source is opened read-only. The
destination refuses existing application data and holds the control-plane lease.
Credentials are read from POSTGRES_DSN, never command arguments or reports.
"""
import argparse
import json
import os
from pathlib import Path
import sqlite3
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "engine"))
from kubecost_engine.postgres import PostgreSQLSaaSStore  # noqa: E402

TABLES = ("enterprise_objects", "telemetry_events", "telemetry_tombstones", "prewarm_commands", "audit_decisions", "tenant_api_keys", "stripe_webhook_events", "rate_limit_counters", "financial_record_ids")


def migrate(source, dsn):
    from psycopg import sql

    source = Path(source).resolve(strict=True)
    destination = PostgreSQLSaaSStore(dsn)
    try:
        destination.acquire_control_plane()
        with sqlite3.connect(source.as_uri() + "?mode=ro", uri=True) as origin:
            origin.execute("BEGIN")
            if origin.execute("PRAGMA integrity_check").fetchone()[0] != "ok":
                raise ValueError("source SQLite integrity check failed")
            available = {r[0] for r in origin.execute("SELECT name FROM sqlite_master WHERE type='table'")}
            counts = {}
            conn = destination._conn.conn
            with conn.transaction():
                for table in TABLES:
                    conn.execute(sql.SQL("LOCK TABLE {} IN ACCESS EXCLUSIVE MODE").format(sql.Identifier(table)))
                for table in TABLES:
                    count = conn.execute(sql.SQL("SELECT COUNT(*) AS n FROM {}").format(sql.Identifier(table))).fetchone()["n"]
                    if count:
                        raise ValueError("destination contains application data; migration refused")
                for table in TABLES:
                    if table not in available:
                        counts[table] = 0
                        continue
                    cursor = origin.execute(f'SELECT * FROM "{table}"')
                    columns = [c[0] for c in cursor.description]
                    insert = sql.SQL("INSERT INTO {} ({}) VALUES ({})").format(sql.Identifier(table), sql.SQL(",").join(map(sql.Identifier, columns)), sql.SQL(",").join(sql.Placeholder() for _ in columns))
                    counts[table] = 0
                    for row in cursor:
                        conn.execute(insert, row)
                        counts[table] += 1
                    actual = conn.execute(sql.SQL("SELECT COUNT(*) AS n FROM {}").format(sql.Identifier(table))).fetchone()["n"]
                    if actual != counts[table]:
                        raise ValueError("migration row-count mismatch")
            origin.rollback()
        return {"status": "copied_and_counts_verified", "tables": counts, "source_modified": False, "cutover_performed": False}
    finally:
        destination.close()


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("source")
    args = parser.parse_args()
    print(json.dumps(migrate(args.source, os.environ["POSTGRES_DSN"]), indent=2))
