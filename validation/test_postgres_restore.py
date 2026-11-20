"""Restore a PostgreSQL custom archive into a separate, empty server."""

import os
import json
from pathlib import Path
import subprocess
import sys
import time
import uuid

import pytest

from kubecost_engine.postgres import PostgreSQLSaaSStore


@pytest.mark.skipif(os.getenv("KCA_POSTGRES_RESTORE") != "1", reason="requires disposable Docker servers")
def test_restore_preserves_tenant_state_and_versions(tmp_path):
    prefix = "kca-restore-" + uuid.uuid4().hex[:10]
    containers = []
    password = uuid.uuid4().hex

    def docker(*args, **kwargs):
        return subprocess.run(["docker", *args], check=True, timeout=120, **kwargs)

    def start(role):
        name = prefix + "-" + role
        docker("run", "-d", "--name", name, "-e", "POSTGRES_PASSWORD=" + password,
               "-p", "127.0.0.1::5432", "postgres:17", capture_output=True)
        containers.append(name)
        deadline = time.monotonic() + 60
        while time.monotonic() < deadline:
            try:
                docker("exec", name, "pg_isready", "-h", "127.0.0.1", "-U", "postgres", capture_output=True)
                break
            except subprocess.CalledProcessError:
                time.sleep(0.25)
        else:
            pytest.fail("PostgreSQL did not become ready")
        port = docker("port", name, "5432/tcp", capture_output=True, text=True).stdout.strip().split(":")[-1]
        return name, f"postgresql://postgres:{password}@127.0.0.1:{port}/postgres"

    try:
        script = Path(__file__).resolve().parents[1] / "hack" / "restored_profiles.py"
        command = [sys.executable, str(script), "--database", "postgres"]
        revocations = tmp_path / "revocations.json"
        src, dsn = start("source")
        store = PostgreSQLSaaSStore(dsn)
        try:
            for tenant in ("north", "south"):
                store.put_object(tenant, "plan", "shared", {"id": "shared", "owner": tenant})
                store.put_object(tenant, "user", "operator", {"id": "operator", "display_name": tenant,
                                 "created_at": 123, "status": "active", "role": "viewer", "history": []})
            store.put_object("north", "maintenance", "m1", {"principal": "user:north:operator", "created_at": 124})
            store.put_object("north", "maintenance", "m2", {"principal": "user:north:other", "created_at": 125})
            archive = tmp_path / "backup.dump"
            store.save_api_key("backup-key", "north", "prefix", "fixture", [], 123, 200)
            with archive.open("xb") as output:
                docker("exec", src, "pg_dump", "-U", "postgres", "-Fc", "postgres", stdout=output)
            store.save_api_key("backup-key", "north", "prefix", "fixture", [], 123, 200, True)
            subprocess.run([*command, "export-revocations", "--output", str(revocations)],
                           env={**os.environ, "KCA_RESTORED_POSTGRES_DSN": dsn}, check=True, timeout=15)
        finally:
            store.close()
        docker("stop", src, capture_output=True)
        dst, dsn = start("destination")
        with archive.open("rb") as data:
            docker("exec", "-i", dst, "pg_restore", "--single-transaction", "-U", "postgres",
                   "-d", "postgres", stdin=data)
        restored = PostgreSQLSaaSStore(dsn)
        try:
            env = {**os.environ, "KCA_RESTORED_POSTGRES_DSN": dsn}
            report = tmp_path / "profile.json"
            subprocess.run([*command, "review", "--tenant", "north", "--user", "operator", "--output", str(report)],
                           env=env, check=True, timeout=15)
            exported = json.loads(report.read_text(encoding="utf-8"))
            assert exported["profile"]["display_name"] == "north"
            assert exported["activity"] == [{"kind": "maintenance", "action": "principal", "timestamp": 124}]
            manifest = tmp_path / "decisions.json"
            decision = {"tenant": "north", "id": "operator", "created_at": 123, "action": "remove_display_name"}
            replay = [*command, "replay", "--decisions", str(manifest), "--revocations", str(revocations)]
            manifest.write_text(json.dumps([decision, {**decision, "created_at": 999}]), encoding="utf-8")
            assert subprocess.run(replay, env=env, capture_output=True, timeout=15).returncode != 0
            assert restored.get_object("north", "user", "operator")["status"] == "active"
            manifest.write_text(json.dumps([decision]), encoding="utf-8")
            subprocess.run(replay, env=env, check=True, timeout=15)
            assert restored.load_all_api_keys()[0]["revoked"]
            assert restored.get_object("north", "user", "operator")["status"] == "disabled"
            assert restored.get_object("south", "user", "operator")["display_name"] == "south"
            restored.acquire_control_plane()
            assert subprocess.run(replay, env=env, capture_output=True, timeout=15).returncode != 0
            for tenant in ("north", "south"):
                assert restored.get_object(tenant, "plan", "shared") == {
                    "id": "shared", "owner": tenant, "version": 1,
                }
            restored.put_object("north", "plan", "shared", {"id": "shared", "owner": "updated"}, 1)
            assert restored.get_object("north", "plan", "shared")["version"] == 2
            assert restored.get_object("south", "plan", "shared")["owner"] == "south"
        finally:
            restored.close()
    finally:
        if containers:
            docker("rm", "-fv", *containers, capture_output=True)
