# GitOps

Status: **detection implemented (verified on a real cluster with the annotation only; Argo CD and Flux themselves were never installed); a local-git proposal flow (`rightsize propose`) is implemented and tested; opening a pull request is NOT implemented. No change is ever made directly to a GitOps-managed workload.**

## Detection

`execute.Inspect` marks a Deployment as GitOps-managed when it carries:

* Argo CD: annotation or label `argocd.argoproj.io/tracking-id` or `argocd.argoproj.io/instance`;
* Flux: annotation or label `kustomize.toolkit.fluxcd.io/name` or `helm.toolkit.fluxcd.io/name`.

Argo CD's default tracking label (`app.kubernetes.io/instance`) is deliberately *not* used: Helm and many other tools set it too, so it would flag unrelated workloads.

## Behaviour

`safety.Evaluate` denies the change ("managed by argocd/flux ... use a pull request") unless the policy sets `allowGitOps: true`. Reason: a direct patch is either reverted by the reconciler
at the next sync (wasted change, flapping) or hides drift. `allowGitOps` exists for clusters where the reconciler is paused or `ignoreDifferences` covers the resources; it is not a recommendation.

## `rightsize propose` (implemented, local git only)

`rightsize propose --recommendation rec.json --repo <dir> --file k8s/api.yaml` edits the container's requests in that manifest (multi-document YAML, comments and key order preserved),
commits on a new branch `rightsize/<kind>-<name>-<id>` with the recommendation summary in the message, and returns to the branch you were on. It never pushes, never opens a PR and never contacts a cluster.
It refuses: an abstained or no-op recommendation, a manifest whose values differ from the ones the recommendation was computed for (stale), a lowered or newly created limit,
an ambiguous match (two documents for the same workload), uncommitted changes in the file, and paths outside the repository. Helm templates and Kustomize patches are not edited.
Verified: unit tests with temporary repositories and `hack/lab-e2e2.sh` step 6.

## What is still needed for a full integration (not built)

1. Editors for Helm values and Kustomize patches (only plain YAML manifests are edited), and a step that opens the pull request with the recommendation JSON attached.
2. After merge, the same observation window and rollback rules, but rollback = revert commit.
3. Mapping from a live object to its source file (Argo CD `Application` source, Flux `Kustomization`/`HelmRelease`), which differs per repo layout.

Until then, for GitOps workloads use `cmd/recommend` as a report and make the change in Git by hand.
