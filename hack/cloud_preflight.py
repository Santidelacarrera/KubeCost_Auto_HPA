#!/usr/bin/env python3
"""Read-only GKE/AKS preflight and soak sampler. No credential acquisition or provisioning."""

import argparse
from datetime import datetime, timezone
import json
from pathlib import Path
import subprocess
import sys
import time


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--context", required=True)
    parser.add_argument("--provider", choices=["gke", "aks", "kind"], required=True)
    parser.add_argument(
        "--authorized-context", help="Explicit authorization to read this context"
    )
    parser.add_argument("--duration-seconds", type=int, default=0)
    parser.add_argument("--interval-seconds", type=int, default=60)
    parser.add_argument("--output", default=".cloud-evidence/preflight.json")
    args = parser.parse_args()
    if not 0 <= args.duration_seconds <= 604800 or args.interval_seconds < 10:
        parser.error("duration must be 0..604800 and interval >=10 seconds")
    prefix = ["kubectl", "--context", args.context, "--request-timeout=20s"]
    commands = [
        prefix + ["version", "-o", "json"],
        prefix + ["get", "nodes", "-o", "json"],
        prefix + ["get", "pods", "-A", "-o", "json"],
        prefix + ["get", "pdb", "-A", "-o", "json"],
        prefix + ["get", "--raw", "/apis/metrics.k8s.io/v1beta1/nodes"],
    ]
    if args.authorized_context != args.context:
        print(
            json.dumps(
                {
                    "status": "plan_only",
                    "commands": commands,
                    "execution": "Requires explicit --authorized-context matching --context; no API requests made",
                },
                indent=2,
            )
        )
        return 0
    output = Path(args.output)
    output.parent.mkdir(parents=True, exist_ok=True)
    report = {
        "context": args.context,
        "provider_requested": args.provider,
        "read_only": True,
        "started_at": datetime.now(timezone.utc).isoformat(),
        "samples": [],
    }
    started = time.monotonic()
    failed = False
    while True:
        sample = {"timestamp": datetime.now(timezone.utc).isoformat(), "checks": []}
        for command in commands:
            result = subprocess.run(command, capture_output=True, text=True, timeout=30)
            check = {"operation": command[4:], "exit_code": result.returncode}
            if result.returncode:
                failed = True
                check["error"] = result.stderr[:1000]
            else:
                data = json.loads(result.stdout)
                # Summaries avoid persisting pod environment values, credentials or account IDs.
                if "serverVersion" in data:
                    check["version"] = data["serverVersion"]["gitVersion"]
                else:
                    check["items"] = [
                        {
                            "name": item.get("metadata", {}).get("name"),
                            "namespace": item.get("metadata", {}).get("namespace"),
                            "kind": item.get("kind"),
                            "phase": item.get("status", {}).get("phase"),
                            "conditions": item.get("status", {}).get("conditions", []),
                            "disruptionsAllowed": item.get("status", {}).get(
                                "disruptionsAllowed"
                            ),
                            "usage": item.get("usage"),
                            "restarts": sum(
                                c.get("restartCount", 0)
                                for c in item.get("status", {}).get(
                                    "containerStatuses", []
                                )
                            ),
                        }
                        for item in data.get("items", [])
                    ]
                    if "nodes" in command and "--raw" not in command:
                        providers = [
                            n.get("spec", {}).get("providerID", "")
                            for n in data.get("items", [])
                        ]
                        expected = {
                            "gke": "gce://",
                            "aks": "azure://",
                            "kind": "kind://",
                        }[args.provider]
                        check["provider_matches"] = bool(providers) and all(
                            p.startswith(expected) for p in providers
                        )
                        failed |= not check["provider_matches"]
            sample["checks"].append(check)
        report["samples"].append(sample)
        report["elapsed_seconds"] = round(time.monotonic() - started, 1)
        report["status"] = "failed" if failed else "collected_not_acceptance_certified"
        output.write_text(json.dumps(report, indent=2), encoding="utf-8")
        print(f"{sample['timestamp']} collected; failures={failed}", flush=True)
        if time.monotonic() - started >= args.duration_seconds:
            return int(failed)
        time.sleep(
            min(
                args.interval_seconds,
                max(0, args.duration_seconds - (time.monotonic() - started)),
            )
        )


if __name__ == "__main__":
    sys.exit(main())
