"""Prints copies of the lab workload "api" (hack/lab/api.yaml) under other names, as one multi-document YAML stream.
usage: make-copies.py NAME [NAME ...]"""
import os
import sys

here = os.path.dirname(os.path.abspath(__file__))
docs = open(os.path.join(here, "api.yaml"), encoding="utf-8").read().split("\n---\n")
deploy = next(d for d in docs if "kind: Deployment" in d)
for name in sys.argv[1:]:
    print("---")
    d = deploy.replace("{name: api,", "{name: %s," % name).replace("{app: api}", "{app: %s}" % name)
    # small on purpose: the 5 GB lab node must schedule every copy (the original asks 1Gi per replica)
    d = d.replace("replicas: 2", "replicas: 1").replace('requests: {cpu: "1", memory: 1Gi}', "requests: {cpu: 500m, memory: 128Mi}")
    d = d.replace('limits: {cpu: "2", memory: 1Gi}', 'limits: {cpu: "1", memory: 256Mi}')
    print(d)
