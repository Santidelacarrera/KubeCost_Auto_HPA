#!/usr/bin/env python3
"""Portable verification with command logs, durations and exit codes. Never deploys."""

import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import subprocess
import sys
import time
import xml.etree.ElementTree as ET


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", default=".local-validation/verification")
    parser.add_argument("--browser", action="store_true")
    parser.add_argument("--services", action="store_true", help="Require real PostgreSQL and Redis")
    parser.add_argument("--operational", action="store_true", help="Run disposable Docker/Kind operational smoke tests")
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    output = (root / args.output).resolve()
    if (output / "results.json").exists():
        raise ValueError("evidence directory already contains a run; choose a new --output path")
    output.mkdir(parents=True, exist_ok=True)
    env = dict(os.environ, PYTHONPATH=str(root / "engine"), PYTHONIOENCODING="utf-8")
    if args.services:
        for key in ("KCA_TEST_POSTGRES_DSN", "TEST_REDIS_URL"):
            if not env.get(key):
                parser.error(f"--services requires {key}")
    checks = [
        (
            "python",
            [
                sys.executable,
                "-m",
                "pytest",
                "engine/tests",
                "-q",
                "--junitxml=" + str(output / "python.xml"),
                "--cov=kubecost_engine",
                "--cov-fail-under=80",
                "--cov-report=json:" + str(output / "coverage.json"),
                "--cov-report=term",
            ],
            root,
        ),
        (
            "lint",
            [
                sys.executable,
                "-m",
                "ruff",
                "check",
                "engine",
                "--select",
                "E9,F63,F7,F82",
            ],
            root,
        ),
        ("go", ["go", "test", "-count=1", "./..."], root / "operator"),
        ("go-vet", ["go", "vet", "./..."], root / "operator"),
        ("audit-collector", ["go", "test", "./..."], root / "tools/audit-collector"),
        ("audit-collector-vet", ["go", "vet", "./..."], root / "tools/audit-collector"),
        ("benchmarks", [sys.executable, "-m", "pytest", "benchmarks", "validation/test_backup.py",
                        "validation/test_verification_runner.py", "-q",
                        "--junitxml=" + str(output / "benchmarks.xml")], root),
        ("helm", ["helm", "lint", "deploy/helm/kubecost-auto-hpa"], root),
        (
            "helm-production",
            [
                "helm",
                "template",
                "verification",
                "deploy/helm/kubecost-auto-hpa",
                "-f",
                "deploy/helm/kubecost-auto-hpa/values-production.yaml",
            ],
            root,
        ),
        ("javascript", ["node", "--check", "web/console.js"], root),
    ]
    if args.browser:
        checks.append(
            (
                "browser",
                [
                    sys.executable,
                    "-m",
                    "pytest",
                    "validation/test_console.py",
                    "-q",
                    "--junitxml=" + str(output / "browser.xml"),
                ],
                root,
            )
        )
    if args.services:
        checks.append(("redis", [sys.executable, "-m", "pytest", "validation/test_redis_service.py", "-q",
                                 "--junitxml=" + str(output / "redis.xml")], root))
    if args.operational:
        env.update(KCA_KIND_CONSOLIDATION="1", KCA_ALERTMANAGER_SMOKE="1", KCA_FAILOVER_SMOKE="1")
        checks.append(("operational", [sys.executable, "-m", "pytest",
                                       "validation/test_consolidation_kind.py",
                                       "validation/test_alert_notifications.py",
                                       "validation/test_failover_runtime.py", "-q",
                                       "--junitxml=" + str(output / "operational.xml")], root))
    report = {
        "started_at_utc": datetime.now(timezone.utc).isoformat(),
        "commit": subprocess.check_output(
            ["git", "rev-parse", "HEAD"], cwd=root, text=True
        ).strip(),
        "working_tree_changes": bool(
            subprocess.check_output(["git", "status", "--porcelain"], cwd=root)
        ),
        "environment": sys.platform,
        "scope": {"browser": args.browser, "services": args.services, "operational": args.operational},
        "checks": [],
    }
    for name, command, cwd in checks:
        started = time.monotonic()
        print(f"Running {name}", flush=True)
        with (output / (name + ".log")).open("w", encoding="utf-8") as log:
            try:
                result = subprocess.run(
                    command,
                    cwd=cwd,
                    env=env,
                    stdout=log,
                    stderr=subprocess.STDOUT,
                    timeout=900,
                )
                code = result.returncode
            except (OSError, subprocess.TimeoutExpired) as exc:
                log.write(str(exc))
                code = 124 if isinstance(exc, subprocess.TimeoutExpired) else 127
        counts = {}
        junit = output / (name + ".xml")
        if junit.exists():
            suites = ET.parse(junit).getroot()
            for key in ("tests", "failures", "errors", "skipped"):
                counts[key] = sum(int(s.get(key, 0)) for s in suites.iter("testsuite"))
            if counts["skipped"] and (args.services or args.operational):
                code = code or 1
        report["checks"].append(
            {
                "name": name,
                "command": command,
                "exit_code": code,
                "duration_seconds": round(time.monotonic() - started, 2),
                "log": name + ".log",
                "test_counts": counts,
            }
        )
        (output / "results.json").write_text(
            json.dumps(report, indent=2), encoding="utf-8"
        )
        print(f"{name}: exit {code}", flush=True)
    report["finished_at_utc"] = datetime.now(timezone.utc).isoformat()
    report["status"] = "failed" if any(c["exit_code"] for c in report["checks"]) else (
        "passed_with_skips" if any(c["test_counts"].get("skipped", 0) for c in report["checks"]) else "passed"
    )
    (output / "results.json").write_text(json.dumps(report, indent=2), encoding="utf-8")
    return int(any(c["exit_code"] for c in report["checks"]))


if __name__ == "__main__":
    sys.exit(main())
