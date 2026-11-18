"""Execute one approved node consolidation using a dedicated kubeconfig context."""

import argparse
import json
import os
import subprocess
import time
import urllib.parse
import urllib.request


MARKER = "autoscaling.kubecost.io/consolidation-plan"
OPT_IN = "autoscaling.kubecost.io/consolidation-enabled"
WORKLOAD_OPT_IN = "autoscaling.kubecost.io/eviction-enabled"


def controller(obj, kind):
    return next(
        (ref for ref in obj["metadata"].get("ownerReferences", [])
         if ref.get("controller") and ref["kind"] == kind),
        None,
    )


def deployment_ready(deployment):
    spec, status = deployment["spec"], deployment.get("status", {})
    replicas = spec.get("replicas", 1)
    return (
        replicas >= 2 and not spec.get("paused")
        and status.get("observedGeneration", 0) >= deployment["metadata"]["generation"]
        and all(status.get(key, 0) == replicas for key in ("replicas", "updatedReplicas", "availableReplicas"))
    )


def eviction_target(kube, pod):
    meta, spec = pod["metadata"], pod["spec"]
    namespace = meta["namespace"]
    if (
        namespace in ("kube-system", "kube-public", "kube-node-lease")
        or meta.get("deletionTimestamp") or pod["status"]["phase"] != "Running"
        or not any(c["type"] == "Ready" and c["status"] == "True" for c in pod["status"].get("conditions", []))
        or any(key in meta.get("annotations", {}) and meta["annotations"][key] == "false"
               for key in ("cluster-autoscaler.kubernetes.io/safe-to-evict", "autoscaling.kubecost.io/safe-to-evict"))
        or any(not set(volume).difference({"name"}) <= {"secret", "configMap", "projected", "downwardAPI"}
               for volume in spec.get("volumes", []))
        or spec.get("hostNetwork") or spec.get("hostPID") or spec.get("hostIPC")
    ):
        raise RuntimeError("pod is unhealthy, protected or uses local state")
    rs = controller(pod, "ReplicaSet")
    if not rs:
        raise RuntimeError("only Deployment pods are eligible")
    owner = kube("-n", namespace, "get", "rs", rs["name"], "-o", "json")
    deploy_ref = controller(owner, "Deployment")
    if owner["metadata"]["uid"] != rs["uid"] or not deploy_ref:
        raise RuntimeError("pod owner is not a live Deployment ReplicaSet")
    deployment = kube("-n", namespace, "get", "deploy", deploy_ref["name"], "-o", "json")
    if (
        deployment["metadata"]["uid"] != deploy_ref["uid"]
        or deployment["metadata"].get("labels", {}).get(WORKLOAD_OPT_IN) != "true"
        or not deployment_ready(deployment)
    ):
        raise RuntimeError("Deployment must be opted in and fully available with at least two replicas")
    # Use a budget named after the Deployment with the same simple selector.
    budget = kube("-n", namespace, "get", "pdb", deploy_ref["name"], "-o", "json")
    selector = deployment["spec"]["selector"]
    labels = selector.get("matchLabels", {})
    if (
        not labels or selector.get("matchExpressions")
        or budget["spec"].get("selector") != {"matchLabels": labels}
        or any(meta.get("labels", {}).get(k) != v for k, v in labels.items())
        or budget["status"].get("observedGeneration") != budget["metadata"]["generation"]
        or budget["status"].get("expectedPods") != deployment["spec"]["replicas"]
        or budget["status"].get("desiredHealthy", 0) < deployment["spec"]["replicas"] - 1
        or budget["status"].get("disruptionsAllowed", 0) < 1
    ):
        raise RuntimeError("Deployment needs a current matching disruption budget allowing one eviction")
    return deployment


def evict(kube, pod, *, dry_run):
    meta = pod["metadata"]
    endpoint = f"/api/v1/namespaces/{meta['namespace']}/pods/{meta['name']}/eviction"
    options = {"preconditions": {"uid": meta["uid"], "resourceVersion": meta["resourceVersion"]}}
    if dry_run:
        options["dryRun"] = ["All"]
    body = {
        "apiVersion": "policy/v1", "kind": "Eviction",
        "metadata": {"name": meta["name"], "namespace": meta["namespace"]},
        "deleteOptions": options,
    }
    kube("create", "--raw", endpoint, "-f", "-", body=body)


def deployment_pods(kube, deployment, original_pod):
    namespace = deployment["metadata"]["namespace"]
    labels = deployment["spec"]["selector"]["matchLabels"]
    selector = ",".join(f"{key}={value}" for key, value in sorted(labels.items()))
    pods = kube("-n", namespace, "get", "pods", "-l", selector, "-o", "json")["items"]
    replica_set = controller(original_pod, "ReplicaSet")["uid"]
    return [pod for pod in pods if (controller(pod, "ReplicaSet") or {}).get("uid") == replica_set]


def replacements_ready(kube, deployment, original_pod, previous, node):
    ready = [
        pod for pod in deployment_pods(kube, deployment, original_pod)
        if not pod["metadata"].get("deletionTimestamp")
        and pod["metadata"]["uid"] != original_pod["metadata"]["uid"]
        and pod["spec"].get("nodeName")
        and any(c["type"] == "Ready" and c["status"] == "True" for c in pod["status"].get("conditions", []))
    ]
    return len(ready) >= deployment["spec"]["replicas"] and any(
        p["metadata"]["uid"] not in previous and p["spec"]["nodeName"] != node for p in ready
    )


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for flag in ("api", "tenant", "plan", "node", "context", "cluster-uid"):
        parser.add_argument("--" + flag, required=True)
    args = parser.parse_args()
    token = os.environ["KCA_CONSOLIDATION_TOKEN"]
    url = urllib.parse.urlsplit(args.api)
    if (
        (url.scheme != "https" and not (url.scheme == "http" and url.hostname in ("localhost", "127.0.0.1", "::1")))
        or not url.hostname or url.username or url.password or url.query or url.fragment
    ):
        parser.error("use an HTTPS API URL or an HTTP loopback URL")
    opener = urllib.request.build_opener(NoRedirect())

    def api(path, payload=None, **query):
        query["client_id"] = args.tenant
        req = urllib.request.Request(
            args.api.rstrip("/") + "/v1/" + path + "?" + urllib.parse.urlencode(query),
            data=json.dumps(payload).encode() if payload is not None else None,
            headers={"Authorization": "Bearer " + token, "Content-Type": "application/json"},
        )
        with opener.open(req, timeout=10) as response:
            return json.load(response)

    def kube(*command, body=None):
        result = subprocess.run(
            ["kubectl", "--context", args.context, "--request-timeout=15s", *command],
            input=json.dumps(body) if body is not None else None,
            stdout=subprocess.PIPE, encoding="utf-8", timeout=20, check=True,
        )
        return json.loads(result.stdout) if result.stdout.strip() else None

    def record(plan, action):
        return api("consolidation/execution", {
            "id": plan["id"], "version": plan["version"], "node": args.node, "action": action,
        })

    def workloads():
        pods = kube("get", "pods", "-A", "--field-selector", "spec.nodeName=" + args.node, "-o", "json")
        return {pod["metadata"]["uid"]: pod for pod in pods["items"]
                if pod["status"]["phase"] not in ("Succeeded", "Failed") and not controller(pod, "DaemonSet")}

    def change_node(node, cordon, *, dry_run=False):
        meta = node["metadata"]
        annotations = dict(meta.get("annotations", {}))
        if cordon:
            annotations[MARKER] = args.plan
        else:
            if annotations.pop(MARKER, None) != args.plan:
                raise RuntimeError("node is no longer owned by this plan")
        patch = [
            {"op": "test", "path": "/metadata/uid", "value": meta["uid"]},
            {"op": "test", "path": "/metadata/resourceVersion", "value": meta["resourceVersion"]},
            {"op": "add", "path": "/metadata/annotations", "value": annotations},
            {"op": "add", "path": "/spec/unschedulable", "value": cordon},
        ]
        options = ["--dry-run=server"] if dry_run else []
        return kube("patch", "node", args.node, "--type=json", "-p", json.dumps(patch), *options, "-o", "json")

    if kube("get", "namespace", "kube-system", "-o", "json")["metadata"]["uid"] != args.cluster_uid:
        raise RuntimeError("kubeconfig context does not match the expected cluster UID")
    plans = api("consolidation/plans", id=args.plan)["items"]
    if not plans:
        raise RuntimeError("plan not found in this organization")
    plan = plans[0]
    if plan["cluster_id"] != args.cluster_uid or plan.get("status") != "approved" or plan.get("mode") != "assisted":
        raise RuntimeError("plan must be approved for the verified cluster UID")
    if args.node not in plan["nodes_can_drain"]:
        raise RuntimeError("node is not an approved candidate")

    def get_node():
        return kube("get", "node", args.node, "-o", "json")

    node = get_node()
    marker = node["metadata"].get("annotations", {}).get(MARKER)
    evacuation = plan.get("execution_strategy") == "stateless-eviction"
    approved_pods = {}
    if plan["execution_status"] == "cordoned" and marker == args.plan:
        return
    if evacuation and plan["execution_status"] == "claimed":
        raise RuntimeError("eviction claim already exists. Inspect the node and recover manually before re-planning")
    if plan["execution_status"] == "not_dispatched":
        if node["spec"].get("unschedulable") or marker:
            raise RuntimeError("candidate is already cordoned or reserved")
        if node["metadata"].get("labels", {}).get(OPT_IN) != "true":
            raise RuntimeError("candidate does not have the consolidation opt-in label")
        others = kube("get", "nodes", "-o", "json")["items"]
        if any(
            condition["type"] in ("DiskPressure", "MemoryPressure", "PIDPressure")
            and condition["status"] != "False"
            for item in others for condition in item["status"].get("conditions", [])
        ):
            raise RuntimeError("cluster reports node pressure")
        if not any(
            other["metadata"]["uid"] != node["metadata"]["uid"]
            and other["metadata"].get("labels", {}).get(OPT_IN) != "true"
            and not other["spec"].get("unschedulable")
            and any(c["type"] == "Ready" and c["status"] == "True" for c in other["status"]["conditions"])
            for other in others
        ):
            raise RuntimeError("cluster needs a ready, schedulable node outside the consolidation pool")
        pending = kube("get", "pods", "-A", "--field-selector", "status.phase=Pending", "-o", "json")
        if any(
            condition.get("type") == "PodScheduled" and condition.get("reason") == "Unschedulable"
            for pod in pending["items"] for condition in pod["status"].get("conditions", [])
        ):
            raise RuntimeError("scheduler reports unschedulable pods")
        approved_pods = workloads()
        if approved_pods and not evacuation:
            raise RuntimeError("node has workload pods and the plan approves only an empty node")
        if len(approved_pods) > 5:
            raise RuntimeError("assisted eviction is limited to five workload pods per node")
        for pod in approved_pods.values():
            eviction_target(kube, pod)
            evict(kube, pod, dry_run=True)
        change_node(node, True, dry_run=True)
        plan = record(plan, "claim")
        try:
            node = change_node(node, True)
        except (subprocess.SubprocessError, OSError):
            # A timed-out patch may have succeeded. Re-read before recording a failure.
            node = get_node()
    if plan["execution_status"] != "claimed":
        raise RuntimeError("plan has no active execution claim")
    if node["metadata"].get("annotations", {}).get(MARKER) != args.plan:
        record(plan, "failed")
        raise RuntimeError("claim exists but the node was not cordoned by this plan")
    try:
        if set(workloads()) != set(approved_pods):
            raise RuntimeError("node workload changed during preflight")
        for uid in approved_pods:
            live_node = get_node()
            if not live_node["spec"].get("unschedulable") or live_node["metadata"].get("annotations", {}).get(MARKER) != args.plan:
                raise RuntimeError("consolidation no longer owns a cordoned node")
            current = workloads()
            if uid not in current or set(current) - set(approved_pods):
                raise RuntimeError("node workload changed during evacuation")
            pod = current[uid]
            before = eviction_target(kube, pod)
            previous = {p["metadata"]["uid"] for p in deployment_pods(kube, before, pod)}
            target = before["metadata"]
            evict(kube, pod, dry_run=True)
            evict(kube, pod, dry_run=False)
            deadline = time.monotonic() + 180
            while time.monotonic() < deadline:
                live = kube("-n", target["namespace"], "get", "deploy", target["name"], "-o", "json")
                meta = live["metadata"]
                if (meta["uid"], meta["generation"]) != (target["uid"], target["generation"]):
                    raise RuntimeError("Deployment changed during evacuation")
                if uid not in workloads() and deployment_ready(live) and replacements_ready(kube, live, pod, previous, args.node):
                    break
                time.sleep(2)
            else:
                raise RuntimeError("scheduler or rollout did not restore available replicas in time")
        if workloads():
            raise RuntimeError("node still has workload pods")
    except (Exception, KeyboardInterrupt):
        change_node(get_node(), False)
        record(plan, "failed")
        raise
    record(plan, "cordoned")
    print(f"{args.node} cordoned. Node release remains with the cluster autoscaler.")


if __name__ == "__main__":
    main()
