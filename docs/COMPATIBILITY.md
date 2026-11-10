# Compatibility

Only what was actually run is listed as verified. Everything else is **not validated**, whatever the platform's reputation.

## Verified (2026-11-09, local lab)

| Component | Version | What ran |
|---|---|---|
| Kubernetes | v1.33.4 (minikube v1.39.0, docker driver, containerd 2.3.4, one node, Docker Desktop 29.8.0 on Windows 11/WSL2) | rightsize pipeline end to end (docs/LAB.md); Helm chart install (see LAB.md for the exact result) |
| Prometheus | chart `prometheus-community/prometheus` 29.46.0 (Prometheus v3.16.0) with its bundled kube-state-metrics 2.20.0, cAdvisor via kubelet | `cmd/recommend` read real series |
| metrics-server | v0.9.0 (minikube addon) | `kubectl top`, KubeCostAutoscaler demand source (autoscaler tests use fakes; see LAB.md) |
| Helm | v3.16.3 | `helm lint`, `helm template`, install on the lab |
| OpenCost | chart `opencost/opencost` 2.5.33, app 1.121.3, default pricing, behind the OpenCost UI service | the KubeCost client's live contract test passed against it (this is OpenCost, not Kubecost) |
| VerticalPodAutoscaler CRD | `autoscaling.k8s.io/v1` from kubernetes/autoscaler master (CRD only, no VPA controller) | detection of an Auto-mode VPA |
| HPA | `autoscaling/v2`, CPU utilisation | detection and refusal of a CPU change |
| StatefulSet | apps/v1, RollingUpdate | apply, observe, rollback |
| GitHub Actions (ubuntu) | `go test -race`, integration tests, govulncheck, envtest on kube-apiserver 1.33.4 + etcd 3.5.21, kind e2e, e2e-kca | all success on commit b7ba429 (see docs/LAB.md) |
| Argo CD | v3.6.1 (core install; `quay.io/argoproj/argocd:v3.6.1`) | detection from the real tracking annotation, self-heal reverting a direct patch, propose -> merge -> sync (docs/LAB.md result 6) |
| Flux | source-controller v1.10.1, kustomize-controller v1.10.1, helm-controller v1.7.1 | detection from the real labels, drift reverted at the interval, propose -> merge -> sync |
| Kubernetes, two nodes | v1.33.4 minikube, 2 nodes | node candidate analysis compared with a real drain (result 7) |
| Go | 1.27.3 (go.mod says 1.26.10) | build, vet, tests on Windows |
| Python | 3.12.10 | engine tests |

The controller tests against a real kube-apiserver 1.33.4 + etcd (envtest) ran in GitHub Actions on the pushed branch and passed; they were not run on this Windows machine (no Linux envtest binaries).

## Not validated

* **EKS, GKE, AKS** and any other managed Kubernetes: no cluster was available. The recommender/executor use only core APIs (apps/v1, autoscaling/v2, core/v1, policy/v1)
  and the VPA CRD if present, so they are *expected* to work, but that is an expectation, not a result.
* Kubernetes other than 1.33.x; multi-node clusters beyond two nodes; managed-cloud node pools and autoscalers (the node analysis never drains or removes anything).
* **Kubecost: partly validated.** A real Kubecost 2.8.7 was run (docs/LAB.md result 11): the response contract holds after a defect fix (the client must aggregate by `cluster,namespace,controllerKind,controller`), but on that lab install Kubecost published allocation data in batches and then stopped publishing for more than 75 minutes, so the freshness check (`failurePolicy.maxMetricAge`, default 5m) rejected it, as designed (fail closed). Not established: Kubecost's normal refresh cadence on a healthy install, and therefore which `maxMetricAge` works with it. Kubecost 2.9.x cannot be installed standalone (needs a federated object store); 3.x not tried. OpenCost 1.121.3 passes with the new aggregation. The client talks to `/model/allocation`; OpenCost needs its UI service for that path.
* HPA and VPA **controllers**: the objects are detected on a real cluster, but the VPA recommender/updater and an HPA reacting to a request change were not run together with a change.
* Windows: the Go tests ran here without `-race` (no cgo); `-race` ran in GitHub Actions on Linux.
* Prometheus Operator / kube-prometheus-stack label conventions: the queries assume the standard `namespace`, `pod`, `container` labels of cAdvisor and kube-state-metrics.

## Requirements of the rightsize pipeline

Prometheus scraping cAdvisor (`container_cpu_usage_seconds_total`, `container_memory_working_set_bytes`, `container_cpu_cfs_*`) and kube-state-metrics
(`kube_pod_container_status_restarts_total`, `..._last_terminated_reason`, `kube_pod_container_resource_requests|limits`). RBAC for `rightsize`: get/list deployments, update deployments,
list pods, list HPAs, list VPAs (optional), list nodes and PDBs for the read-only reports. The user running it needs those rights; no service account is created.
