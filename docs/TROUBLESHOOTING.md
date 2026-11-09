# Troubleshooting

| Symptom | Cause | What to do |
|---|---|---|
| `cmd/recommend` exits 1: "kube-state-metrics series not found" | kube-state-metrics is not scraped, or the namespace/workload/container is wrong | check `kube_pod_container_status_restarts_total{namespace=..,container=..}` exists in Prometheus; the tool refuses to guess requests or OOMs |
| `cmd/recommend` exits 1: "prometheus unreachable" / "prometheus error" | Prometheus down, wrong URL, auth | fix connectivity; use `--prometheus-ca-file`, `--prometheus-bearer-token-file` or `PROMETHEUS_TOKEN`. No data means no recommendation |
| Recommendation `Abstained: true` | too few samples, window shorter than `--min-window`, stale metrics, idle workload, VPA Auto, exclusion | read `CPU.Reasons`; wait for more history. Do not lower `--min-samples` in production (the lab does) |
| A reduction shows `NoChange` with "reduction refused" | OOMKilled, restarts, throttling (or throttling not measured), SLO breach | fix the cause; the recommender never reduces in those cases |
| `rightsize apply` says "not approved" | the Deployment lacks `autoscaling.kubecost.io/approved-recommendation=<ID>` | a human reviews the recommendation and annotates the exact `ID` |
| "stale recommendation: live cpu request is X but it was computed for Y" | the workload changed, or kube-state-metrics lags after a change (about 15-60 s) | generate a new recommendation after metrics catch up |
| "workload is managed by argocd/flux" | GitOps would revert a direct change | change the manifest in Git (pull request); `allowGitOps` only if you accept the fight |
| "an HPA scales on CPU utilisation" | a CPU request change shifts the HPA target | change memory only, or set `allowCPUWithHPA` knowingly and never change replicas in the same step |
| "a VPA in Auto mode owns these requests" | two controllers must not own the same field | set the VPA to `Off` or exclude the workload |
| "could not be established (fail closed)" | an API call (deployments, HPAs, VPAs) failed, or no dynamic client | check RBAC/connectivity; the policy denies rather than guesses |
| `--execute requires an explicit --context` | safety: never act on the implicit current context | add `--context <name>` |
| rollback error "restored values are in place but the rollout did not complete" | the previous values were restored but the workload is still not healthy for another reason | investigate the workload; the values are already back |
| Operator does nothing after a Helm install | `operator.observeOnly` defaults to `true` | review decisions, then `--set operator.observeOnly=false` |
| `go test -race` fails to start | no cgo toolchain | install gcc, or run without `-race` |
| Lab on Windows: `kubectl exec ... /tmp/x` fails strangely | Git Bash rewrites paths | `MSYS_NO_PATHCONV=1` |
| OpenCost answers `{"code":200,"data":[{}]}` (empty) | Prometheus does not scrape OpenCost's own metrics (`node_total_hourly_cost` is absent), or OpenCost is only minutes old | add the scrape job (`hack/lab/prom-opencost.yaml`, installed by `hack/lab-opencost.sh`), wait about 10 minutes |
| KubeCost client gets 404 from OpenCost | the OpenCost API port serves `/allocation/compute`, not `/model/allocation` | use the OpenCost UI service (port 9090), which proxies `/model/...` |
| Pods Pending in the lab, `rightsize plan` says "kube-state-metrics series not found" | the lab node is out of memory (5 GB): Pending pods have no metrics | shrink the workloads' requests (see `hack/lab/more.yaml`); the tool refuses to plan without data, by design |
| `rightsize approve` fails with "cannot establish who you are" | the API server does not serve `SelfSubjectReview` (older than 1.28, or the API is disabled) or you may not call it | upgrade or use an identity-capable cluster; there is deliberately no flag to type a name in |
| `rightsize apply` says "the update strategy is OnDelete" | StatefulSet/DaemonSet pods are only replaced when deleted, so a template change would never roll out | switch the workload to RollingUpdate first |
| `rightsize ... --as` returns Forbidden | the identity lacks a role from `deploy/rbac/rightsize.yaml` | bind `kubecost-rightsize-read` cluster-wide and `kubecost-rightsize-apply` per authorised namespace |
