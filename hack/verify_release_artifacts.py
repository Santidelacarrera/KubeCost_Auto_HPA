#!/usr/bin/env python3
"""Generate local image SBOMs and vulnerability evidence without publishing."""

import argparse
from datetime import datetime, timezone
import hashlib
import json
from pathlib import Path
import subprocess


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", default=".local-validation/release-artifacts")
    parser.add_argument("--operator-image", default="kubecost-auto-hpa-operator:dev")
    parser.add_argument("--engine-image", default="kubecost-auto-hpa-engine:dev")
    parser.add_argument("--cache-dir", help="Reuse a scanner cache outside the immutable evidence directory")
    parser.add_argument("--scanner-image", default=(
        "aquasec/trivy@sha256:8f6bb947dbc528902e11f2f5be52f7b519f6a39d5d148f41166f2040499768fb"
    ))
    args = parser.parse_args()
    output = Path(args.output).resolve()
    output.mkdir(parents=True, exist_ok=True)
    report_path = output / "results.json"
    if report_path.exists():
        parser.error("evidence directory already contains a run; choose a new --output")
    cache = Path(args.cache_dir).resolve() if args.cache_dir else output / "cache"
    cache.mkdir(parents=True, exist_ok=True)
    report = {"started_at_utc": datetime.now(timezone.utc).isoformat(),
              "scanner": args.scanner_image, "published": False, "images": [], "checks": []}
    for name, reference in (("operator", args.operator_image), ("engine", args.engine_image)):
        # Bind the analysis to the immutable local image ID, even if its tag moves.
        inspected = json.loads(subprocess.check_output(
            ["docker", "image", "inspect", reference], text=True, encoding="utf-8"
        ))[0]
        image_id = inspected["Id"]
        report["images"].append({"name": name, "reference": reference, "id": image_id})
        for kind in ("sbom", "vulnerabilities"):
            filename = f"{name}." + ("cdx.json" if kind == "sbom" else "vulnerabilities.json")
            command = ["docker", "run", "--rm", "-v", "/var/run/docker.sock:/var/run/docker.sock",
                       "-v", f"{output}:/out", "-v", f"{cache}:/root/.cache/", args.scanner_image,
                       "image", "--output", "/out/" + filename]
            if kind == "sbom":
                command += ["--format", "cyclonedx"]
            else:
                command += ["--scanners", "vuln", "--severity", "HIGH,CRITICAL", "--ignore-unfixed",
                            "--exit-code", "1", "--format", "json"]
            command.append(image_id)
            print(f"Running {name} {kind}", flush=True)
            with (output / f"{name}-{kind}.log").open("w", encoding="utf-8") as log:
                try:
                    code = subprocess.run(command, stdout=log, stderr=subprocess.STDOUT, timeout=900).returncode
                except (OSError, subprocess.TimeoutExpired) as exc:
                    log.write(str(exc))
                    code = 124 if isinstance(exc, subprocess.TimeoutExpired) else 127
            artifact = output / filename
            check = {"name": f"{name}-{kind}", "command": command, "exit_code": code}
            if artifact.exists():
                check["sha256"] = hashlib.sha256(artifact.read_bytes()).hexdigest()
            report["checks"].append(check)
            report_path.write_text(json.dumps(report, indent=2), encoding="utf-8")
            print(f"{name} {kind}: exit {code}", flush=True)
    report["finished_at_utc"] = datetime.now(timezone.utc).isoformat()
    report["status"] = "failed" if any(c["exit_code"] for c in report["checks"]) else "passed"
    report_path.write_text(json.dumps(report, indent=2), encoding="utf-8")
    return int(report["status"] == "failed")


if __name__ == "__main__":
    raise SystemExit(main())
