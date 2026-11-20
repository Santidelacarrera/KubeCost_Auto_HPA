"""Review profile-related audit activity or replay decisions on a restored PostgreSQL database."""
import argparse
import json
import os
from pathlib import Path

from control_plane_backup import apply_profile_decisions


def activity(conn, tenant, user_id):
    principal = f"user:{tenant}:{user_id}"
    cursor = conn.execute(
        "SELECT kind,payload FROM enterprise_objects WHERE tenant=%s AND kind IN "
        "('user','user_credential','license','alert_ack','maintenance','consolidation') ORDER BY kind,id", (tenant,)
    )
    profile = None
    events = []
    for kind, payload in cursor:
        row = json.loads(payload)
        if kind == "user" and row["id"] == user_id:
            profile = {k: row[k] for k in ("id", "display_name", "role", "status", "created_at")}
        if kind == "user_credential" and row.get("user_id") == user_id:
            events.append({"kind": kind, "action": "assigned", "timestamp": row.get("created_at")})
        for field in ("issued_by", "requested_by", "activated_by", "principal"):
            if row.get(field) == principal:
                events.append({"kind": kind, "action": field, "timestamp": row.get("created_at")})
        for entry in row.get("history", []):
            if kind == "user" and row["id"] == user_id:
                events.append({"kind": kind, "action": "account_updated", "timestamp": entry.get("timestamp"),
                               "role": entry.get("role"), "status": entry.get("status")})
            elif entry.get("principal") == principal:
                events.append({"kind": kind, "action": entry.get("action") or entry.get("decision")
                               or entry.get("execution") or "updated", "timestamp": entry.get("timestamp")})
    return {"tenant": tenant, "profile": profile, "activity": events}


def main():
    import psycopg

    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--database", required=True, help="Expected database name on the restored server")
    actions = parser.add_subparsers(dest="action", required=True)
    replay = actions.add_parser("replay")
    replay.add_argument("--decisions", type=Path, required=True)
    review = actions.add_parser("review")
    review.add_argument("--tenant", required=True)
    review.add_argument("--user", required=True)
    review.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    with psycopg.connect(os.environ["KCA_RESTORED_POSTGRES_DSN"], connect_timeout=10) as conn:
        if args.action == "review":
            conn.execute("SET TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY")
        database = conn.execute("SELECT current_database()").fetchone()[0]
        if database != args.database:
            parser.error("unexpected database name")
        if args.action == "replay":
            if not conn.execute("SELECT pg_try_advisory_xact_lock(1262698833)").fetchone()[0]:
                parser.error("control plane is active")
            conn.execute("LOCK TABLE enterprise_objects IN SHARE ROW EXCLUSIVE MODE NOWAIT")
            decisions = json.loads(args.decisions.read_text(encoding="utf-8-sig"))
            apply_profile_decisions(conn, decisions, postgres=True)
        else:
            result = activity(conn, args.tenant, args.user)
            encoded = json.dumps(result, ensure_ascii=False, allow_nan=False).encode()
            fd = os.open(args.output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            with os.fdopen(fd, "wb") as output:
                output.write(encoded)


if __name__ == "__main__":
    main()
