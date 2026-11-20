"""Measure a real PostgreSQL leader handoff between engine containers."""

import json
import os
from pathlib import Path
import subprocess
import time
import urllib.request
import uuid

import pytest


@pytest.mark.skipif(os.getenv("KCA_FAILOVER_SMOKE") != "1", reason="requires Docker")
@pytest.mark.parametrize("failure", ["sigterm", "sigkill", "database-session-loss"])
def test_engine_signals_release_postgres_and_preserve_state(failure):
    name = "kca-failover-" + uuid.uuid4().hex[:8]
    pg, active, standby = [f"{name}-{role}" for role in ("pg", "active", "standby")]
    tag = name + ":test"
    root = Path(__file__).resolve().parents[1]

    def docker(*args, timeout=30):
        return subprocess.check_output(["docker", *args], text=True, encoding="utf-8", timeout=timeout).strip()

    def engine(container):
        dsn = f"postgresql://postgres:validation-only@{pg}:5432/postgres"
        docker("run", "-d", "--name", container, "--network", name, "-p", "127.0.0.1::8000",
               "-e", f"POSTGRES_DSN={dsn}", "-e", "POSTGRES_STANDBY=true",
               "-e", "ENGINE_ADMIN_TOKEN=local-failover-test", tag)
        port = docker("port", container, "8000/tcp").rsplit(":", 1)[1]
        return f"http://127.0.0.1:{port}"

    def request(base, path, payload=None):
        req = urllib.request.Request(base + path, data=json.dumps(payload).encode() if payload else None,
                                     headers={"Content-Type": "application/json", "Authorization": "Bearer local-failover-test"})
        with urllib.request.urlopen(req, timeout=2) as response:
            return json.load(response)

    def ready(base):
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            try:
                if request(base, "/readyz")["status"] == "ok":
                    return
            except OSError:
                pass
            time.sleep(0.25)
        pytest.fail("engine did not become ready")

    try:
        docker("network", "create", name)
        docker("build", "-t", tag, str(root / "engine"), timeout=300)
        docker("run", "-d", "--name", pg, "--network", name, "-e", "POSTGRES_PASSWORD=validation-only", "postgres:17")
        for _ in range(30):
            try:
                docker("exec", pg, "pg_isready", "-h", "127.0.0.1", "-U", "postgres")
                break
            except subprocess.CalledProcessError:
                time.sleep(1)
        else:
            pytest.fail("PostgreSQL did not become ready")
        first = engine(active)
        ready(first)
        request(first, "/v1/agents/register", {"client_id": "lab", "cluster_id": "before-failover"})
        second = engine(standby)
        with pytest.raises(OSError):
            request(second, "/readyz")
        started = time.monotonic()
        if failure == "sigterm":
            docker("stop", "-t", "15", active)
            expected_exit = "0"
        elif failure == "sigkill":
            docker("kill", active)
            expected_exit = "137"
        else:
            # Only the lock-owning connection in this disposable database is
            # terminated. The waiting replica must reload committed state.
            terminated = docker("exec", pg, "psql", "-U", "postgres", "-Atc",
                                "SELECT pg_terminate_backend(pid) FROM pg_locks "
                                "WHERE locktype='advisory' AND objid=1262698833 AND granted")
            assert terminated == "t"
            expected_exit = "1"
        ready(second)
        print(f"Engine available on standby after {failure}: {time.monotonic() - started:.2f}s")
        assert docker("wait", active) == expected_exit
        clusters = request(second, "/v1/clusters?client_id=lab")["items"]
        assert [c["cluster_id"] for c in clusters] == ["before-failover"]
        request(second, "/v1/agents/register", {"client_id": "lab", "cluster_id": "after-failover"})
        assert {c["cluster_id"] for c in request(second, "/v1/clusters?client_id=lab")["items"]} == {
            "before-failover", "after-failover",
        }
    finally:
        for args in (("rm", "-f", active, standby, pg), ("network", "rm", name), ("image", "rm", tag)):
            subprocess.run(["docker", *args], capture_output=True, timeout=45)
