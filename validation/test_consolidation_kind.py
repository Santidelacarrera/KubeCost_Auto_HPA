"""Exercise assisted eviction on a disposable two-node cluster."""

import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import threading
import time
import uuid

import pytest

from kubecost_engine import server
from kubecost_engine.saas import SaaSControlPlane


@pytest.mark.skipif(os.getenv("KCA_KIND_CONSOLIDATION") != "1", reason="requires Docker and Kind")
def test_approved_eviction_obeys_pdb_and_waits_for_rescheduling(tmp_path, monkeypatch):
    lab = "kca-eviction-" + uuid.uuid4().hex[:8]
    config = tmp_path / "cluster.yaml"
    config.write_text(
        "kind: Cluster\napiVersion: kind.x-k8s.io/v1alpha4\nnodes:\n- role: control-plane\n- role: worker\n",
        encoding="utf-8",
    )
    kubeconfig = tmp_path / "kubeconfig"
    monkeypatch.setenv("KUBECONFIG", str(kubeconfig))
    monkeypatch.setenv("KCA_CONSOLIDATION_TOKEN", "consolidation-admin-local")
    monkeypatch.setenv("ENGINE_ADMIN_TOKEN", "consolidation-admin-local")
    monkeypatch.delenv("REQUIRE_LICENSE", raising=False)
    cp = SaaSControlPlane(str(tmp_path / "state.db"))
    monkeypatch.setattr(server, "get_control_plane", lambda: cp)
    root = Path(__file__).resolve().parents[1]
    executor = root / "hack" / "consolidate_node.py"
    spec = importlib.util.spec_from_file_location("consolidate_node", executor)
    runner = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(runner)

    def kubectl(*args, body=None):
        result = subprocess.run(
            ["kubectl", "--context", "kind-" + lab, *args], timeout=200, check=True,
            text=True, input=json.dumps(body) if body else None, capture_output=True, encoding="utf-8",
        )
        if result.stdout.lstrip().startswith(("{", "[")):
            return json.loads(result.stdout)
        return result.stdout

    def apply(resource):
        kubectl("apply", "-f", "-", body=resource)

    control, target = lab + "-control-plane", lab + "-worker"
    httpd = server.make_server("127.0.0.1", 0)
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()
    try:
        subprocess.run([
            "kind", "create", "cluster", "--name", lab, "--kubeconfig", str(kubeconfig),
            "--config", str(config), "--image", "kindest/node:v1.33.4", "--wait", "180s",
        ], check=True, timeout=600)
        kubectl("taint", "node", control, "node-role.kubernetes.io/control-plane:NoSchedule-")
        patch = tmp_path / "control-plane-only.json"
        patch.write_text(json.dumps({"spec": {"template": {"spec": {
            "nodeSelector": {"kubernetes.io/hostname": control}
        }}}}), encoding="utf-8")
        for namespace, deployment in (("kube-system", "coredns"), ("local-path-storage", "local-path-provisioner")):
            kubectl("-n", namespace, "patch", "deploy", deployment, "--type=merge", "--patch-file", str(patch))
            kubectl("-n", namespace, "rollout", "status", f"deploy/{deployment}", "--timeout=100s")
        kubectl("cordon", control)
        kubectl("label", "node", target, runner.OPT_IN + "=true")
        apply({
            "apiVersion": "apps/v1", "kind": "Deployment",
            "metadata": {"name": "stateless", "labels": {runner.WORKLOAD_OPT_IN: "true"}},
            "spec": {"replicas": 2, "selector": {"matchLabels": {"app": "stateless"}},
                     "template": {"metadata": {"labels": {"app": "stateless"}},
                                  "spec": {"containers": [{"name": "http", "image": "nginx:1.27-alpine"}]}}},
        })
        kubectl("rollout", "status", "deploy/stateless", "--timeout=140s")
        kubectl("uncordon", control)
        labels = {"matchLabels": {"app": "stateless"}}

        def budget(minimum):
            apply({"apiVersion": "policy/v1", "kind": "PodDisruptionBudget", "metadata": {"name": "stateless"},
                   "spec": {"selector": labels, "minAvailable": minimum}})

        cluster = kubectl("get", "ns", "kube-system", "-o", "json")["metadata"]["uid"]
        plan = {
            "id": "lab-plan", "cluster_id": cluster, "status": "approved", "mode": "assisted",
            "execution_strategy": "stateless-eviction", "nodes_can_drain": [target],
            "expires_at": time.time() + 300, "history": [], "execution_status": "not_dispatched",
        }
        cp._store.put_object("lab", "consolidation", plan["id"], plan)
        command = [
            sys.executable, str(executor), "--api", f"http://127.0.0.1:{httpd.server_port}",
            "--tenant", "lab", "--plan", plan["id"], "--node", target, "--context", "kind-" + lab,
            "--cluster-uid", cluster,
        ]
        budget(2)
        kubectl("wait", "--for=jsonpath={.status.currentHealthy}=2", "pdb/stateless", "--timeout=40s")
        blocked = subprocess.run(command, timeout=45, capture_output=True, text=True, encoding="utf-8")
        assert blocked.returncode != 0
        assert "disruption budget" in blocked.stderr
        assert not kubectl("get", "node", target, "-o", "json")["spec"].get("unschedulable")
        budget(1)
        kubectl("wait", "--for=jsonpath={.status.disruptionsAllowed}=1", "pdb/stateless", "--timeout=50s")
        finished = subprocess.run(command, timeout=410, capture_output=True, text=True, encoding="utf-8")
        assert finished.returncode == 0, finished.stderr
        pods = kubectl("get", "pods", "-l", "app=stateless", "-o", "json")["items"]
        assert len(pods) == 2 and all(pod["spec"]["nodeName"] == control for pod in pods)
        assert cp._store.list_objects("lab", "consolidation")[0]["execution_status"] == "cordoned"
        # Model a crash after the node operation but before its outcome was saved.
        row = cp._store.get_object("lab", "consolidation", plan["id"])
        cp._store.put_object("lab", "consolidation", row["id"], {**row, "execution_status": "claimed"}, row["version"])
        for _ in range(2):
            subprocess.run([*command, "--recover"], check=True, timeout=45)
        recovered = kubectl("get", "node", target, "-o", "json")
        assert not recovered["spec"].get("unschedulable")
        assert runner.MARKER not in recovered["metadata"].get("annotations", {})
        assert cp._store.get_object("lab", "consolidation", plan["id"])["execution_status"] == "recovered"
    finally:
        httpd.shutdown()
        httpd.server_close()
        thread.join(timeout=5)
        cp._store.close()
        subprocess.run(["kind", "delete", "cluster", "--name", lab], check=True, timeout=180)
