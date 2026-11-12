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
(port 9090), whose proxy exposes `/model/`. Point `--kubecost-url` at that service when using OpenCost. This is **OpenCost**, not Kubecost (a real Kubecost was run later: result 11).
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
| **60 approved recommendations at once** (observe-only): published and approved in 30 s, all 62 objects reach `DryRun`, the workload is never written, the DryRun metric counted them, no reconcile errors, operator 49 MiB | PASS |
| **kube-apiserver stopped for about 40 s** while two changes were being observed: after it returned both finished `Verified`, each applied exactly once, no `Failed` | PASS |

**Final run: 26 passed, 0 failed** (one run of the final code, 2026-11-10 UTC).

Notes: the operator image used for the old-chart install is the current one (this checks chart upgrade mechanics, not an upgrade between operator versions).
Earlier runs of the script, kept honest: three runs failed on script bugs (readiness label, `patch` vs `update` verb, pod selection; later a race where the API server still enforced the admission policy a few seconds after Helm removed it, now retried for up to 60 s).
**One earlier run ended with both outage changes `RolledBack`** instead of `Verified`; that run did not record the reason. The likely cause, reproduced in a unit test, was that `Watch` counted the time it could not read the cluster against the 60 s rollout timeout used in the lab, so a rollout stalled by the outage looked like "rollout did not complete". That was fixed (`TestWatchDoesNotCountBlindTimeAgainstTheRolloutTimeout`) before the final run. Because the earlier failure was intermittent (the run before it ended `Verified`), one green run does not prove the fix; it is consistent with it.
Not covered: an outage longer than the tolerance on a real cluster (unit-tested), more than two simultaneous observations during the outage, a multi-replica operator with leader election.


## Result 11: a real Kubecost (`hack/lab-kubecost.sh`)

Evidence: `evidence/2026-11-10-kubecost-live-contract.log`. Minikube profile `kubecost-real`, Kubernetes 1.33.4, chart `kubecost/cost-analyzer` **2.8.7** (free tier, its own Prometheus), plus a real OpenCost 1.121.3 on the same cluster. Default prices for a laptop VM: no number here is a bill.

| Check | Result |
|---|---|
| Kubecost 2.9.7 (newest chart) installs standalone | **BLOCKED by the chart**: it refuses without a federated object store (S3-like); 2.9 only prepares the move to 3.0. 2.8.7 used instead |
| Kubecost 2.8.7 installs and prices the workload in `/model/allocation` | PASS (after about 6 min) |
| Live contract test, client as it was (`aggregate=controller`) | **FAIL: real defect found.** Kubecost returns only the `controller` property for that aggregation, so the workload was "not present" (fail closed); two same-named controllers in different namespaces would have been merged into one cost |
| Fix: aggregate by `cluster,namespace,controllerKind,controller`; both real answers kept as test fixtures | done, unit-tested |
| Contract after the fix against Kubecost (workload found, positive hourly cost 0.00684 USD/h) | PASS (sample at 12:34 UTC) |
| Freshness/coverage check against Kubecost (`maxMetricAge` 5m, coverage >= 50% of the window) | **FAIL, and correctly so**: Kubecost published in batches (data ending 12:10, then 12:30) and then published nothing more: its ingestion started at 12:29:37 and had not finished at 13:45. The sample aged out, then the 1 h window no longer contained the workload. Cause of the stall not determined |
| Same live test against the real OpenCost with the new aggregation | PASS (31.7 of 60 min covered, age 1 s) |

What this proves: the client's request and parsing now match a real Kubecost 2.8.7 and a real OpenCost. What it does not: that the operator can run on Kubecost data with the default `maxMetricAge` of 5 minutes. On this install it would have frozen (fail closed), which is safe but useless. Before using Kubecost as the cost source, measure its refresh cadence on your installation and set `failurePolicy.maxMetricAge` above it, knowing that older cost data means slower reactions.


## Result 12: a REAL managed cluster, GKE (`hack/gke-validate.sh`)

Evidence: `evidence/2026-11-10-gke-validation.log` (account, project and user names redacted). GKE 1.36.4-gke.2377000, zonal us-central1-a, `e2-standard-2` nodes, GKE cluster autoscaler 1-3 nodes, created in the owner's account for this run and deleted after it. Node price: **list price** from the Cloud Billing Catalog API (E2 core 0.02181159 USD/h + 8 x E2 RAM 0.00292353 USD/GiB-h = 0.06701142 USD/h).

| Check | Result |
|---|---|
| The pipeline's prerequisites and read-only analysis on GKE (`hack/cloud-validate.sh`: cluster, provider detection `gce://`, cAdvisor and kube-state-metrics series, plan, backtest, nodes, cost) | **9/9 PASS**. With production defaults (200 samples, 24 h) the planner correctly **abstained**: the cluster had 101 samples |
| Recommendation from real GKE metrics, approval with the identity GKE reports, controlled apply, SLO on the app's real latency histogram | PASS: 500m/128Mi -> 250m/64Mi on 6 replicas |
| Equivalent 5-minute windows before/after under real traffic (about 37 requests/s) | PASS: p95 4.75 ms before and after, no errors, no throttling, no restarts |
| **Measured infrastructure saving** (point 3): node count sampled every 30 s in two equivalent 20-minute windows | **3 nodes before, 2 after** (the GKE autoscaler removed one about 15 min after the change). 1.0000 -> 0.6667 node-hours: **0.0670 USD/hour less at list price (-33 %)** |
| Forced rollback (SLO threshold made absurdly tight) | **Run 1: FAIL, real defect.** The rollback started 1 s after the change, during the 6-replica rollout; every write conflicted with the Deployment controller's status updates, `retry.DefaultBackoff` (5 tries, ~50 ms) gave up, and the **degraded change stayed in place** (`rollback_failed` in the audit). Never seen on minikube with 2 replicas. **Fixed** (60 tries over ~12-18 s, each re-reading the object; regression test with a burst of 12 conflicts) |
| Same scenario again on GKE with the fix (run 2: full `hack/lab-traffic.sh`) | **11/11 PASS**: SLO breached, change rolled back, previous requests back |
| Backtest on this run's real history (point 4): 100 min, 70 train / 30 held out | CPU p95 +15 % (default): never exceeded, about 15 % waste; memory p99 +20 % (default): never exceeded, about 18 % waste; memory p90/p95 +5 % exceeded 1 % of the time. **Defaults kept** |

What this is and is not:
* It is the first run on a managed cloud cluster: the recommender, the policy, apply, observation, rollback and the read-only checks work on GKE, and a rightsizing change led the real cluster autoscaler to remove a real node.
* The saving is **measured** (node count from the cluster) and **priced at list price**: not an invoice (no credits, discounts, free tier, disks or network). It is a 20-minute window on a trivial, deliberately over-provisioned app; it says nothing about how much a real workload would save.
* The backtest used under two hours of constant synthetic traffic. It shows the defaults are safe on that series; it is **not** a basis for tuning them, so they were not changed. Tuning still needs weeks of real traffic with daily/weekly patterns.
* Not covered: EKS, AKS, the opt-in applier and the Helm chart on GKE, Kubecost on GKE, a FOCUS export (billing export not set up; the owner will do it).
* Clean-up: cluster deleted; no instances, disks, addresses or load balancers left. Fourteen APIs enabled with GKE stay enabled (the GKE API refused to be disabled right after the deletion, "service has resources"); enabled APIs without resources do not bill.

## Result 13: seven-day soak on GKE (`hack/gke-soak.sh`) — IN PROGRESS, no results yet

Started 2026-11-10T19:37:03Z: GKE zonal us-central1-a, `e2-standard-2`, autoscaler 1-3 nodes, cost allocation on; Prometheus 10 d retention on a 10 GiB disk; the app over-provisioned (4 x 500m) under a **synthetic** daily/weekly schedule (peak 12-21 UTC on weekdays, about 46 req/s measured at start; quiet nights and weekends). It is a schedule against a trivial app, not real users.
Billing: Google's detailed export and its native FOCUS export (preview) to BigQuery were enabled on 2026-11-10 at about 20:00 UTC, so the first hour of the run is not in them.
First look at the exports (2026-11-11 ~01:30 UTC): both tables exist, each with a single zero-cost `Adjustment` placeholder row; no usage yet. That row showed the account bills in **CLP** (rate 923.32 CLP/USD in the row), which the USD-only parser refuses: the report queries now convert per row with Google's rate and keep the CLP totals for the invoice.
Plan: day 1 (2026-11-11) `approve` with production defaults (it may abstain or be refused by policy: that is a valid outcome); daily read-only checks; day 7 (2026-11-17) `report`, then the cluster is deleted. Until that report exists nothing here is a result.


## Result 14: a REAL managed cluster, AKS (`hack/aks-validate.sh`)

Two runs on 2026-11-10, Azure free account, AKS free tier, Kubernetes v1.36.5, `Standard_D2as_v4` nodes (2 vCPU, 8 GiB; Linux list price 0.096 USD/h in eastus from the Azure Retail Prices API), autoscaler 1-2 nodes. Each run created its resource group and deleted it at the end (verified with `az group list`). Evidence: docs/reports/evidence/2026-11-10-aks-validation.log (both runs, redacted).

* **Both runs: 14 passed, 1 failed.** Passed: read-only checks 9/9 (`azure://` provider ids, cAdvisor, kube-state-metrics, plan, backtest, nodes, estimated cost report); recommend, approve, apply 500m/128Mi -> 250m/64Mi under real HTTP traffic (~37 req/s), after-window equal on rps, p95 latency (4.75 ms both), errors, throttling, restarts; the forced rollback driven by the real latency SLO worked and restored the previous requests. No code defect found on AKS.
* **Failed: no node was removed, so no infrastructure saving was measured** (0.6667 node-hours per 20 min before and after; 0.0 USD). The autoscaler's own status said `scaleDown: NoCandidates`. Requests per node after the change (run 2): node A 1737m of ~1.9 CPU (91%), node B 625m (32%). AKS's own kube-system pods (two coredns, two metrics-server, coredns-autoscaler, konnectivity agents, CSI and CNI daemonsets) plus Prometheus fill most of one 2-vCPU node, so the 1000m the change freed was not enough to fit node B's pods onto node A. Capacity was one cause; result 17 found a second one (AKS's autoscaler profile defaults to `skip-nodes-with-system-pods=true`, and AKS places non-DaemonSet kube-system pods such as konnectivity-agent on any node) and, with that changed, measured a node removed.
* Not covered by this result: the opt-in applier and the Helm chart (done since, result 15), Kubecost (result 16), an Azure FOCUS cost export, more than ~1.5 hours of history.
* Side effect: `az aks create --generate-ssh-keys` created an SSH key pair in the operator's home directory. Azure also creates an empty `NetworkWatcherRG` (no billable resources).

## Result 15: the opt-in applier and the Helm chart on a REAL managed cluster, AKS (`hack/aks-applier.sh`)

Evidence: `evidence/2026-11-11-aks-applier-helm.log` (all four runs, redacted). 2026-11-11, Azure free account, AKS free tier, Kubernetes v1.36.5, one `Standard_D4as_v4` node (4 vCPU, 16 GiB: the free account's regional quota is 4 vCPU), images built from this checkout and pushed to a Basic Azure Container Registry attached to the cluster. Each run created its resource group and deleted it at the end (checked with `az group list`).

* **Helm lifecycle (`hack/helm-lifecycle.sh`): PASS 4/4** on AKS: install (revision 1 accepted by the apiserver), upgrade (revision 2 adds exactly the dashboard ConfigMap), rollback to revision 1, uninstall (release objects removed, CRDs kept by design). The first attempt failed only because the script called `python3`, which does not exist on Windows; it now counts revisions without Python.
* **Applier, run 4: 27 passed, 0 failed, all 8 scenarios in ONE run** (on minikube, result 8, scenarios 1-7 and 8 passed in two separate runs): publish changes nothing; a publisher cannot approve (Forbidden), an approver can; observe-only gives DryRun; applied and Verified (500m/256Mi -> 250m/128Mi) with the previous state recorded; an object without an honest `published-by` stamp is refused by the API server, and a correctly stamped one in another namespace is Denied by the operator (`own namespace`); the kill switch denies without a restart and lifting it lets the same object be applied; an operator restart mid-observation does not re-apply; a real container crash after the change is rolled back automatically, with the restart named and a Warning event.
* **Runs 1-3 failed, and every failure was in the lab scripts, not in the operator or the chart.** That is stated because a reader could otherwise take a green fourth run as luck:
  * run 1 (16 passed, 10 failed): the lab workload's CPU requests (2 x 1 CPU plus three copies) did not fit next to Prometheus and the operator on one 4-vCPU node, and the Prometheus port-forward dropped. Added `LAB_SMALL=1` (500m/256Mi requests; usage is ~0.2 CPU, so a decrease is still due) and `PF=1` (re-open the port-forward before each plan).
  * run 2 (20 passed, 6 failed): scenario 5 still created its cross-namespace object without a `published-by` stamp. Since the hardening pass the chart's ValidatingAdmissionPolicy refuses that at creation, so the object never reached the operator; the lab script predated the policy (minikube runs of `lab-applier.sh` were before it). The scenario now checks that admission refusal and then tests the operator's own namespace check with an honestly stamped object approved by `rs-appr`.
  * run 3 (22 passed, 5 failed): scenario 8 never injected the crash. `MSYS_NO_PATHCONV=1` (needed for `kubectl exec ... touch /tmp/crash` in Git Bash) also stops the KUBECONFIG path from being translated, and the AKS scripts use their own kubeconfig file, so `kubectl` reported `context "kubecost-applier" does not exist`; KUBECONFIG is now exported as a Windows path. Scenario 6 got a `Low`-confidence recommendation for its copy, which the policy (`minConfidence: Medium`) correctly refused after the kill switch was lifted; the lab now waits for a Medium or High recommendation. Scenario 8 also retries the injection until a restart is seen and dumps diagnostics if no rollback follows.
* Not shown by this: anything about savings, real traffic patterns or long-running behaviour; one node only, so no multi-node scheduling on AKS for the applier.

## Result 16: a real Kubecost on a REAL managed cluster, AKS: contract and refresh cadence (`hack/aks-kubecost.sh`)

Evidence: `evidence/2026-11-11-aks-kubecost-cadence.log`. Two runs on 2026-11-11, each on a fresh AKS 1.36.5 cluster (one `Standard_D4as_v4`), chart `kubecost/cost-analyzer` **2.8.7**, free tier, its bundled Prometheus, no cloud integration configured. Workload `shop/payments-api`. Goal (docs/ROADMAP.md P0 item 3): measure how often Kubecost refreshes the data the operator reads, to choose `failurePolicy.maxMetricAge` from a measurement. Both resource groups were deleted at the end.

| Check | Run 1 | Run 2 |
|---|---|---|
| Kubecost installs and prices the workload in `/model/allocation` | PASS (4 min) | PASS (4 min) |
| Contract test right after that | FAIL, correctly: the client's coverage check refused 5 of 60 window minutes (fail closed) | same |
| Data served for the workload, sampled every 5 min for 2 h | 2 batches (ends 7.5 min apart), then **nothing from ~45 min after the install** | **identical**: data ending 14:17:30, then 14:25:00, then 15 samples of `{"code":200,"data":[null]}` from 15:01 to 16:12 |
| Age of the newest data while there was any | max 30.5 min | max 31.2 min |
| Kubecost pods during the empty answers | not recorded | Running, **0 restarts**, node at 34% CPU requests |
| Contract test again after 2 h | not run | FAIL: `no_data: workload not present in allocation data` (fail closed) |

**What was measured: Kubecost 2.8.7 in this configuration stops publishing allocation data about 10 minutes after its first data, on a healthy managed cluster, answering HTTP 200 with an empty body.** This is the third time (also result 11, on minikube) and the first on a cluster where node size and pod restarts are ruled out. The cost-model log repeats, every 5 minutes, `error getting disks: compute.DisksClient#List ... InvalidSubscriptionId` (Kubecost's Azure provider has no subscription configured) and a spot-sizing error; whether that causes the stall was **not determined** (it would not explain the minikube stall, which had no Azure provider). Not tried: Kubecost with the Azure cloud integration configured, chart 2.9.x (needs an object store) or 3.x.

What it means for the operator: no `maxMetricAge` can be chosen from this data, because there is no steady refresh to measure. The operator's client behaved as designed: it failed closed every time and never accepted stale or missing cost. **Do not use Kubecost 2.8.7 free tier, installed like this, as a live cost source**; use Prometheus-based pricing, or measure your own Kubecost installation first (`hack/aks-kubecost.sh` does that in about 2.5 hours).

## Result 17: infrastructure saving MEASURED on AKS: one node removed after the change (`hack/aks-validate.sh`, retry of result 14)

Evidence: `evidence/2026-11-11-aks-node-saving-retry.log` (redacted). 2026-11-11, Azure free account, AKS free tier, Kubernetes v1.36.5, `Standard_D2as_v4` nodes (list price 0.096 USD/h), cluster autoscaler 1-2 nodes with **`--cluster-autoscaler-profile skip-nodes-with-system-pods=false`** (now the script's default, `CA_PROFILE=""` keeps AKS's default), app at 2 replicas x 500m. The resource group was deleted at the end (`az group list` shows only `azure-signup-rg` and `NetworkWatcherRG`).

Why result 14 removed no node: besides capacity, AKS's autoscaler profile defaults to `skip-nodes-with-system-pods=true`, so a node holding any kube-system pod that is not a DaemonSet is never a scale-down candidate, and AKS schedules such pods (konnectivity-agent replicas, coredns, metrics-server) on whichever node has room.

* **Saving measured: PASS.** Before the change the app's requests needed 2 nodes (0.6667 node-hours per 20 min); after `500m/128Mi -> 250m/64Mi` the autoscaler removed one, and the 20-minute window after had 1 node (0.3333 node-hours). At list price: 0.064 -> 0.032 USD per 20 min, **0.096 USD/hour less, 50% of this cluster's node cost**. Measured from the node count and priced at list price; it is not an invoice (no discounts, credits, disks or network).
* Read-only cloud-validate and backtest on this run's history: 9/9 and PASS.
* **The inner traffic before/after comparison: 6 passed, 3 failed, and the 3 failures are a measurement gap, not a regression**: every after-window query returned `NaN` (requests/s, p95, restarts all `nan`), so "rps comparable", "p95 did not regress" and "no restarts" could not be evaluated and the script counts that as FAIL. The applier itself reported the change applied and healthy under the real-latency SLO, and result 14's two runs, on the same setup, measured the same change with p95 4.75 ms before and after. The likely cause is the Prometheus port-forward being lost while pods moved; not confirmed.
* What it does not show: that this generalises. It is one run, two small nodes, a deliberately over-provisioned app, and a non-default autoscaler setting that an operator must choose consciously (it lets the autoscaler evict kube-system pods that are not DaemonSets).

## Result 18: two cost sources side by side on AKS: Kubecost 2.8.7 with custom pricing, and OpenCost (`hack/aks-costsource.sh`)

Evidence: `evidence/2026-11-11-aks-cost-sources.log` (redacted). 2026-11-11, one fresh AKS 1.36.5 cluster (`Standard_D4as_v4`), workload `shop/payments-api`, both sources sampled every 5 minutes for 2 hours for the newest data they serve for that workload, then the client's live contract test against each. Resource group deleted at the end (`az group list`: only `azure-signup-rg`, `NetworkWatcherRG`).

| | Kubecost 2.8.7, `customPricesEnabled=true` | OpenCost (chart `opencost/opencost`, its own Prometheus) |
|---|---|---|
| Samples with data for the workload | **4 of 24**: data ending 18:45 and 18:55, then `{"code":200,"data":[null]}` from 19:04 to the end | **22 of 24** (one 502 while the pod started; one at 20:00:14, see below) |
| Age of the newest data | max 4.1 min while there was any | **0 min** in every sample |
| Pods | Running, 0 restarts for 123 min | Running |
| Live contract test after 2 h | **FAIL** `no_data: workload not present` (fail closed) | **PASS** (coverage 41.5 min, age 0 s) |

* **Kubecost 2.8.7 stalled again, identically**: its first data, then nothing about 10 minutes later, HTTP 200 with an empty body, healthy pods. This is the fourth reproduction (result 11 on minikube, result 16 twice). Custom pricing did **not** stop the Azure calls: the cost-model still logged `compute.DisksClient#List ... InvalidSubscriptionId` (63 errors in its last 400 log lines) and the spot-sizing error, so this run neither confirms nor rules out that error as the cause. What is established: Kubecost 2.8.7 free tier, installed from its chart with defaults (with or without custom pricing), does not keep serving allocation data on AKS.
* **OpenCost served fresh data for the whole 2 hours and passed the contract.** It is the open-source cost model Kubecost is built on and answers the same `/model/allocation` API, so the operator reads it with no code change.
* **A client defect found by this run and fixed**: the client asked for `window=1h`. Kubecost and OpenCost share one window parser (opencost `core/pkg/opencost/window.go`) that rounds a duration window forward to its unit, so `1h` is the **current clock hour**, not the last 60 minutes. The data shows it: OpenCost's cost for the workload grew within each hour and dropped back at 19:04 and 20:05, and the sample at 20:00:14 had no allocation for the workload at all. With the default `minCoverage 0.5` the operator would have refused every sample in the first half of every hour. The client now always sends whole minutes (`60m` = the last 60 minutes, ending at the next whole minute); unit and contract tests updated.
* **The fix, verified live** (2026-11-12, OpenCost with its own Prometheus on the minikube lab, workload `lab/shop`, `.lab/opencost-window/`): at 00:10:35 UTC, ten minutes into the hour, `window=1h` returned the allocation from 00:00:00 to 00:10:34 (**10.57 minutes**, which the operator's coverage check would refuse) and `window=60m` returned 23:11:00 to 00:10:00 (**59 minutes**). The client's live contract test, which now sends `60m`, PASSED at that moment (minutes 59.0, age 46 s). Chosen on purpose: before half past the hour is exactly when the old request failed.
* Not shown: Kubecost 2.9.x/3.x or with a real Azure integration, OpenCost pricing accuracy against an Azure invoice.

**Recommendation now in the docs: use OpenCost (or Prometheus-based pricing) as the live cost source; do not use Kubecost 2.8.7 free tier installed from its chart defaults.**

## Result 19: the recommender against simulated shoppers, a compressed week (`hack/lab-synthetic-users.sh`, minikube): 4 passed, 0 failed

Evidence: `evidence/2026-11-12-synthetic-users.log` (redacted). 2026-11-11/12, the minikube lab, Deployment `lab/shop` (2 replicas, 1 CPU / 256 MiB requests, a real HTTP latency histogram) and `lab/users`, a generator of shopper sessions over several routes with a daily and a weekly cycle and random flash sales. One simulated day = 60 minutes, 7 days = 7 hours. Only that history was given to the recommender.

* **Backtest (trained on the history before the last 2 simulated days, tested on those 2, unseen)**: CPU, the default p95 + 15% gives 0.42 cores, exceeded **0%** of the test samples, peak ratio 0.96 (the unseen peak was 96% of the recommendation). Memory, the same default gives 16.0 MiB, **exceeded 2.9%** of the test samples and the unseen peak was 1.35x the recommendation; p99 + 15% gave 0% exceeded, peak ratio 0.89. Takeaway: on this load the CPU default holds, the memory default is tight; memory has limits (the change keeps them), but a request below real use means the scheduler packs pods tighter than they need.
* **Plan, apply and observe (run 3)**: the recommender (production minimums, 200 samples) gave Medium confidence, CPU 1 -> 0.5, memory 256 -> 128 MiB (limits kept). Applied with the SLO on the store's real latency (p95 > 250 ms or 5xx > 1% breaches) and observed for 60 minutes, one full simulated day: **kept, verified** (audit: `decision`, `applied`, `verified` after 1h0m). p95 over one simulated day **86 ms before, 75 ms after**; CPU throttled periods 0.96%; 0 restarts. 4 PASS, 0 FAIL.
* **Two runs that do not count, and why**:
  * Run 1 abstained (Low confidence): the production minimum history is 24 h, and the compressed week is 7 h. Correct behaviour. The script now uses one simulated day as the minimum window (`MIN_WINDOW`).
  * Run 2 applied the same change, but its apply call was broken because I edited the script while bash was still reading it (bash reads scripts incrementally), so it ran **without the SLO arguments**. Not a valid result. The store was restored to 1 CPU / 256 MiB, the annotations removed, and run 3 ran from a frozen copy of the script.
* **Gaps in this run**: the error ratio is `nan` before and after (the 5xx query returned no series; the generator saw no 5xx, but "no errors" is not measured by this run), and no flash sale fell inside the observation hour, so the change was not tested against a burst after it was applied (bursts were in the training history and the backtest).
* What it is not: real users or real weeks. A compressed, generated week answers "does the recommender survive bursty, cyclic, multi-route load", not "are the defaults right for production". P0 item 1 stays open.

## Result 20: the ninth-pass features on a real cluster (`.lab/multi/run.sh`, minikube): 10 passed, 0 failed after one defect fixed

Evidence: `evidence/2026-11-12-p1-real-cluster.log`. 2026-11-12, the minikube lab (Kubernetes 1.36, its Prometheus), namespace `multi`, Deployment `web` with two containers: `app` (500m/256Mi, busy) and `proxy` (300m/128Mi, idle sidecar), 2 replicas, 15 minutes of history. Each step used the CLI built from this branch.

| Check | Run 1 | Run 2 (after the fix) |
|---|---|---|
| `rightsize keygen` | PASS | PASS |
| `plan --container app,proxy` gives ONE record (app 500m->250m, 256->128 MiB; proxy 300m->150m, 128->64 MiB; Medium) | PASS | PASS |
| policy `requireSignedApproval`: an unsigned approval is refused | PASS | PASS |
| `approve --sign-key` writes the signature annotation; apply accepted under the same policy | PASS | PASS |
| both containers changed in ONE rollout | FAIL, a wrong check (see below) | **PASS**: one new ReplicaSet |
| requests changed and kept through a 2-minute observation | PASS | PASS |
| `audit-verify` on the real audit file | **FAIL: a real defect** | **PASS** |
| an edited line is detected | PASS (line 2) | PASS (line 2) |
| `report --format prometheus` | PASS | PASS |

* **Defect found here and fixed (commit 2f4051a)**: every real `applied` event failed `audit-verify`. Its details hold the previous resources as a typed map of structs, whose JSON keys follow the struct order; a reader decodes into `map[string]any` and re-encodes them sorted, so the hash could never match. The unit test had used plain integers in the details. The chain now hashes each event in its decoded form, and the test uses the executor's real detail types.
* Run 1's "one spec update" check was wrong, not the product: a Deployment's `metadata.generation` also increases on annotation writes (the approval and the applied markers), so it went 2 -> 4. Rollouts are now counted as new ReplicaSets: exactly one (`kubectl get rs` in the evidence).
* Not covered: the operator's applier with a signed `status.approval` (the CLI path ran; the operator path is unit-tested), `--audit-url` against a real collector, `propose --open-pr` against GitHub, the FOCUS adapters on a real export with node rows. The first three: result 21.

## Result 21: the three ninth-pass paths that had only unit tests, run for real: 11/11 on minikube, and two defects found and fixed

Evidence: `evidence/2026-11-12-p1-verify.log` (`hack/lab-p1-verify.sh`, minikube lab) and `evidence/2026-11-12-gitops-open-pr.log` (GitHub). 2026-11-12, operator image and CLI built from this branch.

| Check | Result |
|---|---|
| **Operator applier**, policy `requireSignedApproval` with one approver key: an **unsigned** `status.approval` (`approve --cr`) | **Denied**, reason `signed approval required: the approval carries no signature`; the workload untouched |
| the same with a **signed** approval (`approve --cr --sign-key`) | signature in `status.approval.signature`, **Verified**, requests 500m/128Mi -> 350m/64Mi |
| **`--audit-url`** to a real HTTP collector (`hack/lab/audit-collector.py`: bearer token, first POST refused with 503) during `approve` and `apply --execute` | all 4 events delivered after the retry; the collector's copy is **byte-for-byte the local chained file**; `audit-verify` passes on it (4 chained, no break); nothing reported undelivered |
| **`propose --push --open-pr`** against GitHub (this repository, base `claude/bold-carson-5479vm`) | the branch was pushed and the PR opened with the commit's title and body (PRs #44 and #45, both closed on purpose, branches deleted) |

* **Defect 1, found while writing the collector check, fixed**: with `--audit-file` and `--audit-url` together, the collector received the events **without** `prev`/`hash` (the HTTP sink sat next to the chain, not after it), so the shipped copy could not be verified, which defeats the point of shipping it. The chain now forwards each event after chaining it (unit test `TestAuditChainForwardsChainedEvents`); the check above is the verification.
* **Defect 2, found by the first real `--open-pr` (PR #44), fixed**: the change re-encoded the whole file, which re-indented every list and also rewrote the other Deployment in the same file, so a reviewer saw a 30-line diff for a 1-line change. Propose now replaces only the changed values in the file text (keeping the file's quoting) and checks that the result parses to the edited tree; it re-encodes only when a key must be added. PR #45, from the fixed binary, is a **one-line diff** (unit test `TestProposeChangesOnlyTheChangedLines`).
* Not covered: the operator's `--audit-url` (https only; needs a collector with a certificate the operator trusts), a WORM / object-lock bucket behind the collector, a signed approval forged by editing `status.approval` on the cluster (unit-tested).
