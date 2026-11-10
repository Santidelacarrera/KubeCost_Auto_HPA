"""Prints a HAND-MADE recommendation JSON for a lab workload (no Prometheus in the lean GitOps cluster).
It is not produced by the recommender; it only exercises policy, detection and the GitOps proposal on real controllers.
usage: handmade-rec.py NAME [ID]"""
import json
import sys

name = sys.argv[1]
rid = sys.argv[2] if len(sys.argv) > 2 else "handmade-" + name
print(json.dumps({
    "ID": rid, "Cluster": "kubecost-gitops", "Namespace": "gitops-demo", "Workload": name, "Kind": "Deployment", "Container": "app",
    "Confidence": "Medium", "Abstained": False, "Samples": 0,
    "CPU": {"Action": "Decrease", "Current": 0.5, "Recommended": 0.25, "Limit": 1},
    "Memory": {"Action": "Decrease", "Current": 134217728, "Recommended": 67108864, "Limit": 268435456},
}))
