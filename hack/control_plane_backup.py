#!/usr/bin/env python3
"""Consistent SQLite backup/restore; never overwrite an existing destination."""

import argparse
from contextlib import closing
import hashlib
import json
import os
from pathlib import Path
import sqlite3
import tempfile


def apply_profile_decisions(conn, decisions, *, postgres=False):
    """Replay an operator-reviewed list against a private recovery copy."""
    if not isinstance(decisions, list):
        raise ValueError("expected an array of profile decisions")
    bind = "%s" if postgres else "?"
    where = f"tenant={bind} AND kind='user' AND id={bind}"
    for decision in decisions:
        action = decision["action"]
        fields = {"tenant", "id", "created_at", "action"}
        if action == "correct":
            fields.add("display_name")
        if set(decision) != fields or action not in ("correct", "disable", "remove_display_name"):
            raise ValueError("invalid profile decision")
        identity = (decision["tenant"], decision["id"])
        row = conn.execute("SELECT payload FROM enterprise_objects WHERE " + where, identity).fetchone()
        if not row:
            raise ValueError("account missing from backup")
        user = json.loads(row[0])
        if user["created_at"] != decision["created_at"]:
            raise ValueError("account creation timestamp mismatch")
        if action == "correct":
            value = decision["display_name"]
            if not isinstance(value, str) or not 1 <= len(value.strip()) <= 120:
                raise ValueError("invalid display name")
            changes = {"display_name": value.strip()}
        elif action == "disable":
            changes = {"status": "disabled"}
        else:
            changes = {"display_name": "[removed]", "status": "disabled"}
        if any(user.get(key) != value for key, value in changes.items()):
            user.update(changes)
            conn.execute(f"UPDATE enterprise_objects SET version=version+1,payload={bind} WHERE " + where,
                         (json.dumps(user), *identity))


def copy_database(source, destination, *, profile_decisions=None):
    source = Path(source).resolve(strict=True)
    destination = Path(destination).resolve()
    if source == destination or destination.exists():
        raise ValueError("destination must not exist")
    destination.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(dir=destination.parent) as directory:
        staging = Path(directory) / "copy.db"
        staging.touch(mode=0o600)
        with closing(sqlite3.connect(source.as_uri() + "?mode=ro", uri=True)) as src:
            with closing(sqlite3.connect(staging)) as dst:
                src.backup(dst)
                dst.execute("PRAGMA journal_mode=DELETE")
                if profile_decisions is not None:
                    dst.execute("PRAGMA secure_delete=ON")
                    with dst:
                        apply_profile_decisions(dst, profile_decisions)
                    dst.execute("VACUUM")
                integrity = dst.execute("PRAGMA integrity_check").fetchone()[0]
                if integrity != "ok":
                    raise ValueError("backup integrity check failed")
        with staging.open("rb") as stream:
            checksum = hashlib.sha256()
            for chunk in iter(lambda: stream.read(1024 * 1024), b""):
                checksum.update(chunk)
            digest = checksum.hexdigest()
        os.link(staging, destination)
        return {
            "destination": str(destination),
            "sha256": digest,
            "integrity": integrity,
        }


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("operation", choices=["backup", "restore"])
    parser.add_argument("--source", required=True)
    parser.add_argument("--destination", required=True)
    parser.add_argument("--profile-decisions", type=Path, help="Reviewed JSON decisions to replay during SQLite restore")
    args = parser.parse_args()
    decisions = None
    if args.profile_decisions:
        if args.operation != "restore":
            parser.error("--profile-decisions requires restore")
        decisions = json.loads(args.profile_decisions.read_text(encoding="utf-8-sig"))
        if not isinstance(decisions, list):
            parser.error("--profile-decisions must contain an array")
    print(json.dumps(copy_database(args.source, args.destination, profile_decisions=decisions), indent=2))


if __name__ == "__main__":
    main()
