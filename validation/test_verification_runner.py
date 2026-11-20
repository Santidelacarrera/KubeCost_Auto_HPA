"""Verification must distinguish executed checks, omissions and process failures."""

import importlib.util
import json
from pathlib import Path
import subprocess
import sys

import pytest


@pytest.mark.parametrize(
    "strict,skipped,process_code,status,exit_code",
    [(False, 0, 0, "passed", 0), (False, 2, 0, "passed_with_skips", 0),
     (True, 2, 0, "failed", 1), (True, 0, 1, "failed", 1)],
)
def test_report_preserves_skips_and_failures(tmp_path, monkeypatch, strict, skipped,
                                           process_code, status, exit_code):
    path = Path(__file__).resolve().parents[1] / "hack/verify_enterprise.py"
    spec = importlib.util.spec_from_file_location("verification_runner", path)
    runner = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(runner)
    args = [str(path), "--output", str(tmp_path)] + (["--services"] if strict else [])
    monkeypatch.setattr(sys, "argv", args)
    monkeypatch.setenv("KCA_TEST_POSTGRES_DSN", "configured-for-test")
    monkeypatch.setenv("TEST_REDIS_URL", "configured-for-test")
    monkeypatch.setattr(runner.subprocess, "check_output", lambda *a, **k: "commit\n" if k.get("text") else b"")

    def run(command, **kwargs):
        for arg in command:
            if arg.startswith("--junitxml="):
                Path(arg.split("=", 1)[1]).write_text(
                    f'<testsuites><testsuite tests="3" failures="0" errors="0" skipped="{skipped}"/></testsuites>',
                    encoding="utf-8",
                )
        return subprocess.CompletedProcess(command, process_code)

    monkeypatch.setattr(runner.subprocess, "run", run)
    assert runner.main() == exit_code
    report = json.loads((tmp_path / "results.json").read_text())
    assert report["status"] == status
    assert report["checks"][0]["test_counts"]["skipped"] == skipped
    assert report["finished_at_utc"] >= report["started_at_utc"]
