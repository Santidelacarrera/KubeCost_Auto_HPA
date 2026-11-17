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


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", default=".local-validation/verification")
    parser.add_argument("--browser", action="store_true")
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    output = (root / args.output).resolve()
    if (output / "results.json").exists():
        raise ValueError("evidence directory already contains a run; choose a new --output path")
    output.mkdir(parents=True, exist_ok=True)
    env = dict(os.environ, PYTHONPATH=str(root / "engine"), PYTHONIOENCODING="utf-8")
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
    report = {
        "started_at_utc": datetime.now(timezone.utc).isoformat(),
        "commit": subprocess.check_output(
            ["git", "rev-parse", "HEAD"], cwd=root, text=True
        ).strip(),
        "working_tree_changes": bool(
            subprocess.check_output(["git", "status", "--porcelain"], cwd=root)
        ),
        "environment": sys.platform,
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
        report["checks"].append(
            {
                "name": name,
                "command": command,
                "exit_code": code,
                "duration_seconds": round(time.monotonic() - started, 2),
                "log": name + ".log",
            }
        )
        (output / "results.json").write_text(
            json.dumps(report, indent=2), encoding="utf-8"
        )
        print(f"{name}: exit {code}", flush=True)
    return int(any(c["exit_code"] for c in report["checks"]))


if __name__ == "__main__":
    sys.exit(main())
