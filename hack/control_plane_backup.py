#!/usr/bin/env python3
"""Consistent SQLite backup/restore; never overwrite an existing destination."""

import argparse
import hashlib
import json
from pathlib import Path
import sqlite3


def copy_database(source, destination):
    source = Path(source).resolve(strict=True)
    destination = Path(destination).resolve()
    if source == destination or destination.exists():
        raise ValueError("destination must not exist")
    destination.parent.mkdir(parents=True, exist_ok=True)
    # Atomic reservation also rejects races between two backup processes.
    with destination.open("xb"):
        pass
    try:
        with sqlite3.connect(source.as_uri() + "?mode=ro", uri=True) as src:
            with sqlite3.connect(destination) as dst:
                src.backup(dst)
                integrity = dst.execute("PRAGMA integrity_check").fetchone()[0]
                if integrity != "ok":
                    raise ValueError("backup integrity check failed")
        digest = hashlib.sha256(destination.read_bytes()).hexdigest()
        return {
            "destination": str(destination),
            "sha256": digest,
            "integrity": integrity,
        }
    except Exception:
        # Keep failed copies for diagnosis, never remove source or existing data.
        raise


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("operation", choices=["backup", "restore"])
    parser.add_argument("--source", required=True)
    parser.add_argument("--destination", required=True)
    args = parser.parse_args()
    print(json.dumps(copy_database(args.source, args.destination), indent=2))


if __name__ == "__main__":
    main()
