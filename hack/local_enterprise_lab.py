#!/usr/bin/env python3
"""Disposable Kind resize + Helm lifecycle validation; explicit isolated kubeconfig."""

import argparse
import json
import os
from pathlib import Path
import subprocess
import sys
import time
import uuid


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--keep", action="store_true")
    parser.add_argument("--output", default=".local-validation/enterprise-lab")
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    output = (root / args.output).resolve()
    if (output / "results.json").exists():
        raise ValueError("evidence directory already contains a run; choose a new --output path")
    output.mkdir(parents=True, exist_ok=True)
    name = "kca-v2-" + uuid.uuid4().hex[:8]
    config = output / "lab.kubeconfig"
    env = dict(os.environ, KUBECONFIG=str(config))
    steps = []

    def run(command, timeout=600, capture=False):
        start = time.monotonic()
        print(" ".join(command), flush=True)
        result = subprocess.run(
            command,
            cwd=root,
            env=env,
            text=True,
            encoding="utf-8",
            errors="replace",
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            timeout=timeout,
        )
        with (output / "lab.log").open("a", encoding="utf-8") as log:
            log.write("$ " + " ".join(command) + "\n" + result.stdout + "\n")
        steps.append(
            {
                "command": command,
                "exit_code": result.returncode,
                "seconds": round(time.monotonic() - start, 2),
            }
        )
        (output / "results.json").write_text(
            json.dumps(
                {"cluster": name, "provider": "kind-local", "steps": steps}, indent=2
            )
        )
        if result.returncode:
            raise RuntimeError(
                f"command failed: {command}; inspect {output / 'lab.log'}"
            )
        return result.stdout if capture else None

    created = False
    try:
        run(["docker", "info", "--format", "{{.ServerVersion}}"])
        existing = run(["kind", "get", "clusters"], capture=True).splitlines()
        if name in existing:
            raise RuntimeError("refusing existing cluster")
        run(
            [
                "kind",
                "create",
                "cluster",
                "--name",
                name,
                "--image",
                "kindest/node:v1.33.4",
                "--kubeconfig",
                str(config),
                "--wait",
                "180s",
            ],
            timeout=600,
        )
        created = True
        for component in ("operator", "engine"):
            run(
                [
                    "docker",
                    "build",
                    "-t",
                    f"kubecost-auto-hpa-{component}:dev",
                    component,
                ],
                timeout=1200,
            )
        run(
            [
                "kind",
                "load",
                "docker-image",
                "--name",
                name,
                "kubecost-auto-hpa-operator:dev",
                "kubecost-auto-hpa-engine:dev",
            ]
        )
        run(["kubectl", "apply", "-k", "operator/config/crd"])
        run(
            [
                "kubectl",
                "wait",
                "--for=condition=Established",
                "--timeout=60s",
                "crd/kubecosthpas.autoscaling.kubecost.io",
                "crd/kubecostautoscalers.autoscaling.kubecost.io",
            ]
        )
        run(["kubectl", "apply", "-k", "operator/config/overlays/e2e"])
        for deployment in (
            "kubecost-auto-hpa-operator",
            "kubecost-auto-hpa-engine",
            "kubecost-auto-hpa-redis",
            "fake-prometheus",
        ):
            run(
                [
                    "kubectl",
                    "-n",
                    "kubecost-auto-hpa-system",
                    "rollout",
                    "status",
                    "deploy/" + deployment,
                    "--timeout=180s",
                ]
            )
        run(
            [
                "kubectl",
                "-n",
                "e2e",
                "rollout",
                "status",
                "deploy/web",
                "--timeout=180s",
            ]
        )
        pods_command = [
            "kubectl",
            "-n",
            "e2e",
            "get",
            "pods",
            "-l",
            "app=web",
            "-o",
            "json",
        ]
        before = json.loads(run(pods_command, capture=True))["items"]
        for _ in range(60):
            after = json.loads(run(pods_command, capture=True))["items"]
            cpus = [
                p["spec"]["containers"][0]["resources"]["requests"]["cpu"]
                for p in after
            ]
            if cpus and all(c.endswith("m") and int(c[:-1]) < 500 for c in cpus):
                break
            time.sleep(5)
        else:
            raise RuntimeError("no in-place shrink observed")
        if {p["metadata"]["uid"] for p in before} != {
            p["metadata"]["uid"] for p in after
        }:
            raise RuntimeError("pods recreated during resize")
        if any(
            s["restartCount"] for p in after for s in p["status"]["containerStatuses"]
        ):
            raise RuntimeError("container restarted during resize")
        # Disable first operator before installing a second writer in the same disposable cluster.
        run(
            [
                "kubectl",
                "-n",
                "kubecost-auto-hpa-system",
                "scale",
                "deploy/kubecost-auto-hpa-operator",
                "--replicas=0",
            ]
        )
        values = [
            "--set",
            "operator.image.repository=kubecost-auto-hpa-operator",
            "--set",
            "operator.image.tag=dev",
            "--set",
            "engine.image.repository=kubecost-auto-hpa-engine",
            "--set",
            "engine.image.tag=dev",
            "--set",
            "operator.replicas=1",
            "--set",
            "engine.persistence.enabled=true",
        ]
        chart = "deploy/helm/kubecost-auto-hpa"
        run(
            [
                "helm",
                "install",
                "rehearsal",
                chart,
                "-n",
                "helm-lab",
                "--create-namespace",
                *values,
                "--wait",
                "--timeout",
                "4m",
            ]
        )
        run(
            [
                "helm",
                "upgrade",
                "rehearsal",
                chart,
                "-n",
                "helm-lab",
                *values,
                "--set",
                "grafanaDashboard.enabled=true",
                "--wait",
                "--timeout",
                "4m",
            ]
        )
        run(
            [
                "helm",
                "rollback",
                "rehearsal",
                "1",
                "-n",
                "helm-lab",
                "--wait",
                "--timeout",
                "4m",
            ]
        )
        run(["kubectl", "-n", "helm-lab", "get", "pvc", "-o", "json"])
        (output / "acceptance.json").write_text(
            json.dumps(
                {
                    "resize_without_recreation": True,
                    "container_restarts": 0,
                    "helm_install_upgrade_rollback": True,
                    "provider": "kind-local",
                    "version_upgrade_v010_v020": "not_tested",
                    "cloud_validation": False,
                },
                indent=2,
            )
        )
    finally:
        if created and not args.keep:
            run(["kind", "delete", "cluster", "--name", name], timeout=180)
    return 0


if __name__ == "__main__":
    sys.exit(main())
