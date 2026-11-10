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
That closes the earlier NOT RUN items for `-race`, envtest and CI. The run on the later commit 2491f93 (StatefulSet/DaemonSet, identity-stamped approvals, propose, report, cost budgets) is also all success: engine, operator (race, integration, govulncheck), manifests, envtest, e2e-kind, e2e-kca. The run on 534927c (the opt-in applier CRD and controller, backtest, FOCUS adapter, the stale-cache fix) is all success as well, including the new RBAC render tests, envtest on a real kube-apiserver and the kind jobs.


## Result 6: REAL Argo CD and REAL Flux (`hack/lab-gitops.sh`): 15 PASS, 0 FAIL

Evidence: `evidence/2026-11-09-lab-gitops-argo-flux.log`. A lean minikube profile (Kubernetes 1.33.4) with the Argo CD core install (`stable` manifests) and Flux (`latest` install manifests) of 2026-11-09,
both syncing from an in-cluster git smart-HTTP server (`hack/lab/git-http`, an nginx + `git http-backend` image built locally). Versions: Argo CD v3.6.1; Flux source-controller v1.10.1 and kustomize-controller v1.10.1. The whole script was run twice from an empty cluster with the same 15 PASS (the second run is `evidence/2026-11-09-lab-gitops-argo-flux-run2.log`).
The recommendations here are **hand-made** (the lean cluster has no Prometheus); the point is detection, refusal and the GitOps path, not the numbers.

| Check | Result |
|---|---|
| Argo CD application Synced/Healthy and Flux Kustomization applied, each managing a Deployment | PASS |
| Marks they really write: Argo CD sets the annotation `argocd.argoproj.io/tracking-id`; Flux sets the labels `kustomize.toolkit.fluxcd.io/name` and `/namespace`. The tool detects both and `apply` is denied ("managed by argocd/flux"); `--execute` changes nothing | PASS |
| **A direct patch is reverted by both reconcilers** (Argo CD self-heal within the wait; Flux at its 1-minute interval): this is why the tool refuses | PASS |
| `rightsize propose` makes a branch with the edit (YAML comment kept, working branch untouched); after a merge and `git push`, **Argo CD and Flux both apply** 500m/128Mi to 250m/64Mi and nothing fights it; the old recommendation is then denied | PASS |

Two things found on the way: a first Argo CD attempt stayed `Unknown` because the core install has no `default` AppProject (the script now creates it); Flux's go-git client cannot read a dumb-HTTP git repository, hence the smart-HTTP server.

## Result 7: a real two-node cluster (`hack/lab-multinode.sh`): 9 PASS, 0 FAIL

Evidence: `evidence/2026-11-09-lab-multinode.log`. Two minikube nodes (Kubernetes 1.33.4, no Prometheus needed). The node report predicts which node could be emptied; the script then **drains it for real** (disposable cluster) and compares.

| Check | Result |
|---|---|
| Four small pods with a PodDisruptionBudget: the tool reports the worker as a removable candidate, flags the PDB and says a candidate is not a saving; after `kubectl drain` all pods run on the other node and none is Pending | PASS |
| Three pods requesting 4.5 CPU each: the tool reports nothing removable and says "does not fit"; draining anyway leaves pods Pending (none were Pending before) | PASS |

Lab note: minikube nodes advertise the host CPU count (12) as allocatable whatever `--cpus` says, so the "does not fit" case needs large requests.

## Result 8: the opt-in applier (WorkloadRecommendation CRD + controller) on a real cluster (`hack/lab-applier.sh`)

Evidence: `evidence/2026-11-09-lab-applier-full-run-scenarios-1-7.log` (full run: all scenario 0-7 checks PASS, scenario 8 FAIL because of the lab's crash injection) and
`evidence/2026-11-09-lab-applier-scenario-8-rerun.log` (`FROM=8`: all PASS after fixing the injection). **There is no single 26/26 run**: scenarios 1-7 and scenario 8 passed in two runs of the same final operator code.

| Check | Result |
|---|---|
| Install with the applier on for namespace `lab` only and observe-only on; CRD installed | PASS |
| `plan --publish` creates the object, phase `Pending`, workload untouched | PASS |
| **Approval is its own permission**: a ServiceAccount that can publish is refused (Forbidden) when approving; one with the approve role succeeds and the recorded approver is the identity the API server reports | PASS |
| Observe-only: the approved object becomes `DryRun`, nothing is written | PASS |
| Observe-only off (`helm upgrade`): the same object is applied, observed 90 s and `Verified` (1 CPU / 1Gi to 500m / 512Mi); previous state on the workload; approver and times on the object | PASS |
| An object in another namespace carrying a record for `lab` is `Denied` ("own namespace") | PASS |
| The kill switch in the policy ConfigMap acts without a restart (`Denied`, nothing written); lifting it lets the same object proceed to `Verified` | PASS |
| Operator pod deleted while a change is being observed: after restart the object reaches `Verified` and `last-applied-at` is unchanged (applied exactly once) | PASS |
| A container crash after the change: phase `RolledBack`, previous requests restored, reason names the restart, a Warning event on the object | PASS (rerun) |

**Three real defects found only on the cluster, all fixed and covered by unit tests:**
1. The policy ConfigMap was read through the manager's cached client, which LISTs and WATCHes every ConfigMap; the Role (get by name) rightly forbids that, so the operator logged `forbidden` forever and approved objects stayed `Pending`. It now does a direct GET by name.
2. A reconcile queued behind a finished one read a cached copy one status update behind (still `Applying`), re-evaluated the change and **overwrote the final `RolledBack` with `Denied`**. Decisions now use an uncached read and a final phase can never be overwritten (`TestAFinishedOutcomeIsNeverOverwrittenEvenWithAStaleCache`).
3. The recommender lowered confidence to Low because of the coefficient of variation of a ~1 MB working set (and, earlier, of 5-15 millicore CPU) far below the configured floors; variability below the floors is now ignored.

Not covered: more than a handful of objects, API server outages, a managed cluster.


## Result 9: real HTTP traffic and real latency, equivalent windows before and after (`hack/lab-traffic.sh`)

Evidence: `evidence/2026-11-09-lab-traffic-latency.log` (a second clean run reproduced the first within a few percent). The Prometheus example app exposes a real `http_request_duration_seconds` histogram, two load generators call it at a constant rate, and Prometheus scrapes it through pod annotations.
The workload is trivial and over-provisioned on purpose; this is a laptop VM, **not** a production-like service.

| Metric (5-minute windows, 2 replicas) | Before | After (6+ minutes after the change) |
|---|---|---|
| Requests per second | 35.4 | 37.0 (comparable: within 25%) |
| p95 latency | 4.75 ms | 4.75 ms |
| CPU throttled ratio | 0 | 0 |
| CPU usage p95 (cores, whole workload) | 0.032 | 0.031 |
| Container restarts | 0 | 0 |
| Requests per replica | 500m / 128Mi | **250m / 64Mi** (applied through the policy gate, 120 s observation with an SLO query on the real p95) |
| Requested cost per hour | 0.0320 USD | 0.0160 USD (**ESTIMATE**: requests x replicas x the flag prices 0.031 USD per core-hour and 0.004 per GiB-hour) |
| Requested cost per 1k requests | 0.00025 USD | 0.00012 USD (**ESTIMATE**) |

So for this workload halving the requests changed neither latency, throttling nor restarts. A second run with the SLO query's threshold made absurdly tight on purpose (p95 > 0.1 microsecond, still the real metric) breached and rolled back automatically, with the previous requests verified.
The error ratio is `NaN` in both windows because the app produced no non-2xx series; it is reported as unknown, not as zero.

What this does **not** show: any saving on a bill (the numbers above are allocation of requests at flag prices, and one node cannot release capacity), behaviour of a real application under load spikes, a weekly cycle, or tail latency beyond p95 on a trivial route.
The first check of the script ("the app serves real traffic") failed in that log because of a quoting bug in the check itself (traffic was 35 rps in the next snapshot); it is fixed in the script.


## Result 10: hardening on a real cluster (`hack/lab-hardening.sh`)

Evidence: `evidence/2026-11-09-lab-hardening.log`. Lean minikube profile, Kubernetes 1.33.4, hand-made recommendations (no Prometheus).

| Check | Result |
|---|---|
| **Helm upgrade from the previous chart** (git commit 534c64d, before all this work) to the current one, `helm rollback`, and upgrade again | PASS |
| **Known limitation, verified:** `helm upgrade` does not install a CRD that is new in `crds/`; the new CRD has to be applied by hand (`kubectl apply -f deploy/helm/kubecost-auto-hpa/crds/`) and the operator restarted. Rollback keeps the CRD (Helm never deletes CRDs) | PASS |
| **Separation of duties by the API server** (alice publishes, bob approves, by impersonation): forged `published-by` denied; alice approving her own denied; bob writing an approval in alice's name denied; bob's proper approval accepted; the operator's own status writes still pass (phase DryRun); `published-by` cannot be rewritten even by an admin; with the switch off alice can approve her own | PASS |
| **60 approved recommendations at once** (observe-only): published and approved in about 37 s, all reach `DryRun`, the workload is never written, the DryRun metric counted them, no reconcile errors, operator about 50 MiB | PASS |
| **kube-apiserver stopped for about 40 s** while two changes were being observed: after it returned both finished `Verified`, each applied exactly once, no `Failed` | PASS |

Notes: the operator image used for the old-chart install is the current one (this checks chart upgrade mechanics, not an upgrade between operator versions); the run shown is the last of several runs of the same script after fixing script bugs (readiness label, `patch` vs `update` verb, pod selection), and the first complete run also produced all but those checks.
Not covered: an outage longer than the tolerance on a real cluster (unit-tested), more than two simultaneous observations during the outage, a multi-replica operator with leader election.
