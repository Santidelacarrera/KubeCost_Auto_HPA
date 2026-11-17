"""Reads `kubectl get sts store -o json` on stdin and prints a small manifest for the GitOps proposal test,
using the LIVE requests and limits so the recommendation is not stale against the file."""
import json
import sys

o = json.load(sys.stdin)
r = o["spec"]["template"]["spec"]["containers"][0]["resources"]
print(f"""apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: store
  namespace: lab
spec:
  serviceName: store
  replicas: 2
  template:
    spec:
      containers:
      - name: app   # keep this comment
        image: busybox:1.36
        resources:
          requests:
            cpu: "{r['requests']['cpu']}"
            memory: {r['requests']['memory']}
          limits:
            cpu: "{r['limits']['cpu']}"
            memory: {r['limits']['memory']}""")
