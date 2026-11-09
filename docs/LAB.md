# Reproducible lab and verification evidence

A disposable single-node Kubernetes (minikube, docker driver) with Prometheus + kube-state-metrics + metrics-server. Everything here ran on 2026-11-09
on one Windows 11 machine (Docker Desktop 29.8.0). Versions: docs/COMPATIBILITY.md. Logs: `docs/reports/evidence/`.

```bash
bash hack/lab-up.sh        # profile "kubecost-lab" only; never touches other minikube profiles
kubectl --context kubecost-lab -n monitoring port-forward svc/prom-prometheus-server 9090:80 &
bash hack/lab-e2e.sh       # ~12 min: recommender -> policy -> approval -> apply -> observe -> rollback
bash hack/lab-helm.sh      # ~5 min: chart install / observe-only / upgrade-off / rollback / uninstall
bash hack/lab-down.sh      # deletes the lab profile
```

Both scripts refuse to run against a context other than `kubecost-lab` unless `ALLOW_CTX=1`.
Lab-only settings (`--min-window 8m --min-samples 40`) are far below the production defaults (24 h, 200 samples) because the lab has minutes of history, not days.

## Result 1: rightsizing pipeline on a real cluster (`hack/lab-e2e.sh`): 22 PASS, 0 FAIL

Evidence: `evidence/2026-11-09-lab-e2e.log`, `evidence/2026-11-09-lab-audit.jsonl`.

| Check | Result |
|---|---|
| Recommendation built from **real** cAdvisor + kube-state-metrics series via `cmd/recommend` | PASS (workload asking 1 CPU / 1 GiB, using about 0.1 CPU / 2 MiB) |
| Empty policy, missing approval, kill switch: all deny; dry-run changes nothing; `--execute` without `--context` refused | PASS |
| Approved + allowed apply: requests changed on the live Deployment (1 CPU / 1 GiB to 500m / 512Mi), pods run with the new values, 60 s observation healthy | PASS |
| The same recommendation is not applied twice | PASS |
| SLO breach (a synthetic always-true PromQL query through the real Prometheus) after a change: automatic rollback, previous values restored, verified | PASS |
| Injected fault (a file makes a container exit, a real restart) after a change: detected, automatic rollback, previous values back, rolled-back marker recorded, the ID refused afterwards | PASS |
| Audit trail: decision, applied, verified, degraded, rolled_back with actor and recommendation ID | PASS |

**A bug only the real cluster showed:** the first runs failed the SLO scenario because the rollback's `Update` conflicted with the Deployment controller's concurrent status update
(`rollback_failed` in the audit, change left in place). The fake clientset cannot reproduce this. Fixed with conflict retries (apply retries only while the spec generation is unchanged),
covered by `TestRollbackRetriesOnConflict` and `TestApplyRetriesStatusConflictButNotASpecChange`, and re-verified with the 22/22 run above.
Two script-level lessons are documented in TROUBLESHOOTING: restarts inside the recommender window veto reductions (correct behaviour), and kube-state-metrics lags 15-60 s after a change (the stale guard refuses meanwhile).

What this does **not** prove: behaviour under real load or latency (the workload is a busy loop, the SLO signal is synthetic), multi-node or managed clusters, long observation windows, or that the
percentile/headroom defaults are good for real applications.

## Result 2: Helm chart on a real cluster (`hack/lab-helm.sh`): 16 PASS, 0 FAIL

Evidence: `evidence/2026-11-09-lab-helm.log`.

| Check | Result |
|---|---|
| `helm install` with locally built operator and engine images: release deployed, both CRDs exist, operator + engine + Redis ready | PASS |
| Operator logs "observe-only mode"; arg `--observe-only=true` (chart default); Spot drain controller not started | PASS |
| A `KubeCostAutoscaler` on a 4-replica idle workload: decision "observe-only: would scale 4 -> 1", `ScalingActive=False/ObserveOnly`, replicas **stay 4** | PASS |
| `helm upgrade --set operator.observeOnly=false`: the same resource scales the Deployment to 1, `ScalingActive=True/Owner` | PASS |
| `helm rollback` to revision 1 restores `--observe-only=true`; `helm uninstall` removes the workloads and keeps the CRDs (Helm behaviour) | PASS |

**KubeCost is a MOCK here** (`hack/lab/mock-kubecost.py` returns an invented constant cost). The chart, apiserver, metrics-server, controllers and CRDs are real; the KubeCost contract is **not** validated.
Upgrade between chart versions was not tested (only a values change).

## Not covered by either lab run

Real KubeCost/OpenCost; real billing; GitOps controllers; HPA/VPA objects; multi-node scheduling; managed clouds (EKS/GKE/AKS); real traffic and latency; envtest and `-race` (no binaries/cgo);
kind/k3d (minikube was used because it was the available tool).

## Result 3: second real-cluster run (`hack/lab-e2e2.sh`): 23 PASS, 0 FAIL

Evidence: `evidence/2026-11-09-lab-e2e2.log`, `evidence/2026-11-09-lab-audit2.jsonl`, `evidence/2026-11-09-opencost-live-contract.log`.

| Check | Result |
|---|---|
| A **real HPA** scaling on CPU: the plan warns, `apply` is denied ("an HPA scales on CPU") | PASS |
| A **real VerticalPodAutoscaler object** (the real CRD; no VPA controller runs, the object is only seen) in Auto mode: the planner abstains naming the VPA | PASS |
| A workload carrying an Argo CD tracking annotation (Argo CD itself is not installed): `apply` is denied as managed by argocd | PASS |
| The exclusion label makes the planner abstain | PASS |
| **StatefulSet** end to end: `approve` stamps the identity the API server reports (`SelfSubjectReview`, user `minikube-user`); a policy whose `approvers` list excludes that user denies; with the right list the change applies (500m / 128Mi to 320m / 64Mi), pods are replaced, observation healthy; `rollback --kind StatefulSet` restores the values | PASS |
| GitOps proposal on a local git repo: a branch with the edit, original branch untouched, YAML comment preserved, nothing pushed | PASS |
| `report` counts one applied and one rolled-back change from the audit trail | PASS |
| `nodes` (single node: nothing removable) and `cost` (ESTIMATED basis, reconciled, budget exceeded flagged, a doubled node price shows as a deviation) | PASS |
| **Live KubeCost-contract test against a REAL OpenCost** (chart opencost 2.5.33, app 1.121.3; default pricing, so the numbers are not a bill): the client parsed a real allocation response | PASS |

Notes on the OpenCost run: OpenCost serves `/allocation/compute`; `/model/allocation` (the path this client uses, as KubeCost does) answered 404 directly on the API port but 200 through the OpenCost UI service
(port 9090), whose proxy exposes `/model/`. Point `--kubecost-url` at that service when using OpenCost. This is **OpenCost**, not Kubecost: Kubecost itself was not installed.
Prometheus needed an extra scrape job for OpenCost's own metrics, otherwise the allocation is empty (`hack/lab/` notes in docs/TROUBLESHOOTING.md).
A first attempt failed because my own lab workloads requested more memory than the node had (two pods Pending): the tool refused to plan without kube-state-metrics data and the StatefulSet rollout timed out and rolled back, which is the intended behaviour; the manifests were then shrunk.

## Result 4: minimal RBAC proven by impersonation (`hack/lab-rbac.sh`): 14 PASS, 0 FAIL

Evidence: `evidence/2026-11-09-lab-rbac.log`. `deploy/rbac/rightsize.yaml` defines `kubecost-rightsize-read` (cluster-wide list/get) and `kubecost-rightsize-apply` (update on workloads, bind per namespace).
As a read-only ServiceAccount the CLI can `plan`, `nodes` and `cost` but `approve` is Forbidden and `--execute` changes nothing; as the applier ServiceAccount (apply role in `lab` only, no create/delete, nothing in other namespaces)
it can approve, apply and roll back. This is the role of the person running the CLI, not of the operator.

## Result 5: interruption and recovery (`hack/lab-crash.sh`): 7 PASS, 0 FAIL

Evidence: `evidence/2026-11-09-lab-crash.log`. The process is killed (`kill -9`) in the middle of a 10-minute observation after the change was applied. The new values stay, the previous state is still on the object,
a second process refuses the same recommendation ID, and a brand-new process rolls back from the saved state alone; the audit shows `rolled_back` by the recovery actor.

## Also verified outside the lab: GitHub CI on the pushed branch

Run 42681084642 (commit b7ba429): jobs `engine`, `operator` (includes `go test -race -p 2 ./...`, the integration tests and `govulncheck`), `manifests`, `envtest` (kube-apiserver 1.33.4 + etcd), `e2e-kind` and `e2e-kca`: all success.
That closes the earlier NOT RUN items for `-race`, envtest and CI. It did not include the changes of the later commits listed in the CHANGELOG under "Added (second pass)".
