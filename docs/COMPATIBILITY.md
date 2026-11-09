# Compatibility

Only what was actually run is listed as verified. Everything else is **not validated**, whatever the platform's reputation.

## Verified (2026-11-09, local lab)

| Component | Version | What ran |
|---|---|---|
| Kubernetes | v1.33.4 (minikube v1.39.0, docker driver, containerd 2.3.4, one node, Docker Desktop 29.8.0 on Windows 11/WSL2) | rightsize pipeline end to end (docs/LAB.md); Helm chart install (see LAB.md for the exact result) |
| Prometheus | chart `prometheus-community/prometheus` 29.46.0 (Prometheus v3.16.0) with its bundled kube-state-metrics 2.20.0, cAdvisor via kubelet | `cmd/recommend` read real series |
| metrics-server | v0.9.0 (minikube addon) | `kubectl top`, KubeCostAutoscaler demand source (autoscaler tests use fakes; see LAB.md) |
| Helm | v3.16.3 | `helm lint`, `helm template`, install on the lab |
| Go | 1.27.3 (go.mod says 1.26.10) | build, vet, tests on Windows |
| Python | 3.12.10 | engine tests |

An earlier pass (docs/ENGINEERING_REPORT.md) ran the controller tests against a real kube-apiserver 1.33.4 + etcd (envtest) on another machine. That was not re-run here (no envtest binaries).

## Not validated

* **EKS, GKE, AKS** and any other managed Kubernetes: no cluster was available. The recommender/executor use only core APIs (apps/v1, autoscaling/v2, core/v1, policy/v1)
  and the VPA CRD if present, so they are *expected* to work, but that is an expectation, not a result.
* Kubernetes other than 1.33.x; multi-node clusters (the node-capacity analysis is only unit-tested for >1 node).
* KubeCost / OpenCost (live contract test exists, never run: `docs/PILOT.md` section 11).
* Argo CD and Flux: detection is by label/annotation and unit-tested; no GitOps controller was installed.
* HPA and VPA objects: detection unit-tested with fakes; not exercised against real HPA/VPA controllers.
* Windows is the only OS the Go tests were run on in this pass; `-race` needs cgo and was not run.
* Prometheus Operator / kube-prometheus-stack label conventions: the queries assume the standard `namespace`, `pod`, `container` labels of cAdvisor and kube-state-metrics.

## Requirements of the rightsize pipeline

Prometheus scraping cAdvisor (`container_cpu_usage_seconds_total`, `container_memory_working_set_bytes`, `container_cpu_cfs_*`) and kube-state-metrics
(`kube_pod_container_status_restarts_total`, `..._last_terminated_reason`, `kube_pod_container_resource_requests|limits`). RBAC for `rightsize`: get/list deployments, update deployments,
list pods, list HPAs, list VPAs (optional), list nodes and PDBs for the read-only reports. The user running it needs those rights; no service account is created.
