# GitOps

Status: **detection implemented (unit-tested only); no change is ever made to a GitOps-managed workload; pull-request flow NOT implemented.**

## Detection

`execute.Inspect` marks a Deployment as GitOps-managed when it carries:

* Argo CD: annotation or label `argocd.argoproj.io/tracking-id` or `argocd.argoproj.io/instance`;
* Flux: annotation or label `kustomize.toolkit.fluxcd.io/name` or `helm.toolkit.fluxcd.io/name`.

Argo CD's default tracking label (`app.kubernetes.io/instance`) is deliberately *not* used: Helm and many other tools set it too, so it would flag unrelated workloads.

## Behaviour

`safety.Evaluate` denies the change ("managed by argocd/flux ... use a pull request") unless the policy sets `allowGitOps: true`. Reason: a direct patch is either reverted by the reconciler
at the next sync (wasted change, flapping) or hides drift. `allowGitOps` exists for clusters where the reconciler is paused or `ignoreDifferences` covers the resources; it is not a recommendation.

## What would be needed for a real integration (not built)

1. A writer that turns an approved recommendation into a patch of the manifest in the repository (Helm values, Kustomize patch or plain YAML) and opens a pull request carrying the recommendation JSON.
2. After merge, the same observation window and rollback rules, but rollback = revert commit.
3. Mapping from a live object to its source file (Argo CD `Application` source, Flux `Kustomization`/`HelmRelease`), which differs per repo layout.

Until then, for GitOps workloads use `cmd/recommend` as a report and make the change in Git by hand.
