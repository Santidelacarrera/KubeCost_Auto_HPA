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
