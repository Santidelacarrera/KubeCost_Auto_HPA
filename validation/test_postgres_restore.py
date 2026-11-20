"""Restore a PostgreSQL custom archive into a separate, empty server."""

import os
import subprocess
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
        src, dsn = start("source")
        store = PostgreSQLSaaSStore(dsn)
        try:
            for tenant in ("north", "south"):
                store.put_object(tenant, "plan", "shared", {"id": "shared", "owner": tenant})
            archive = tmp_path / "backup.dump"
            with archive.open("xb") as output:
                docker("exec", src, "pg_dump", "-U", "postgres", "-Fc", "postgres", stdout=output)
        finally:
            store.close()
        docker("stop", src, capture_output=True)
        dst, dsn = start("destination")
        with archive.open("rb") as data:
            docker("exec", "-i", dst, "pg_restore", "--single-transaction", "-U", "postgres",
                   "-d", "postgres", stdin=data)
        restored = PostgreSQLSaaSStore(dsn)
        try:
            restored.acquire_control_plane()
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
