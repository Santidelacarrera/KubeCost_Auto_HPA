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


def export_revocations(conn, destination):
    """Write only the identifiers needed to invalidate keys in an older copy."""
    rows = conn.execute("SELECT client_id,key_id,created_at FROM tenant_api_keys WHERE revoked=1 ORDER BY client_id,key_id")
    entries = [{"tenant": tenant, "key_id": key, "created_at": created} for tenant, key, created in rows]
    destination = Path(destination).resolve()
    with tempfile.TemporaryDirectory(dir=destination.parent) as directory:
        staging = Path(directory) / "revocations.json"
        staging.touch(mode=0o600)
        staging.write_text(json.dumps(entries), encoding="utf-8")
        os.link(staging, destination)
    return {"revoked_keys": len(entries)}


def apply_revocations(conn, entries, *, postgres=False):
    if not isinstance(entries, list):
        raise ValueError("expected a list of credential revocations")
    bind = "%s" if postgres else "?"
    where = f"client_id={bind} AND key_id={bind}"
    for entry in entries:
        if not isinstance(entry, dict) or set(entry) != {"tenant", "key_id", "created_at"}:
            raise ValueError("invalid credential revocation")
        if not all(isinstance(entry[k], str) and entry[k] for k in ("tenant", "key_id")) or type(entry["created_at"]) is not int:
            raise ValueError("invalid credential identity")
        identity = (entry["tenant"], entry["key_id"])
        row = conn.execute("SELECT created_at FROM tenant_api_keys WHERE " + where, identity).fetchone()
        if row is None:
            continue  # The key was issued after this snapshot or is already absent.
        if row[0] != entry["created_at"]:
            raise ValueError("credential creation timestamp mismatch")
        conn.execute("UPDATE tenant_api_keys SET revoked=1 WHERE " + where, identity)


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


def copy_database(source, destination, *, profile_decisions=None, revocations=None):
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
                if revocations is not None:
                    with dst:
                        apply_revocations(dst, revocations)
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
    parser.add_argument("operation", choices=["backup", "restore", "export-revocations"])
    parser.add_argument("--source", required=True)
    parser.add_argument("--destination", required=True)
    parser.add_argument("--profile-decisions", type=Path, help="Reviewed JSON decisions to replay during SQLite restore")
    parser.add_argument("--revocations", type=Path, help="Revoked key inventory to replay on restore")
    args = parser.parse_args()
    decisions = None
    if args.profile_decisions:
        if args.operation != "restore":
            parser.error("--profile-decisions requires restore")
        decisions = json.loads(args.profile_decisions.read_text(encoding="utf-8-sig"))
        if not isinstance(decisions, list):
            parser.error("--profile-decisions must contain an array")
    if args.revocations and args.operation != "restore":
        parser.error("--revocations requires restore")
    if args.operation == "export-revocations":
        source = Path(args.source).resolve(strict=True)
        with closing(sqlite3.connect(source.as_uri() + "?mode=ro", uri=True)) as conn:
            result = export_revocations(conn, args.destination)
    else:
        revocations = json.loads(args.revocations.read_text(encoding="utf-8-sig")) if args.revocations else None
        if args.revocations and not isinstance(revocations, list):
            parser.error("--revocations must contain an array")
        result = copy_database(args.source, args.destination, profile_decisions=decisions, revocations=revocations)
    print(json.dumps(result, indent=2))


if __name__ == "__main__":
    main()
