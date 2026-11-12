# GitOps

Status: **detection and the GitOps path are verified against a REAL Argo CD (core install) and a REAL Flux on Kubernetes 1.33.4 (docs/LAB.md result 6): both are detected from the marks they really write, a direct patch is reverted by both (which is why the tool refuses), and `rightsize propose` -> merge -> push makes both controllers apply the change. Opening a pull request on a hosted Git service is NOT implemented. No change is ever made directly to a GitOps-managed workload.**

## Detection

`execute.Inspect` marks a Deployment as GitOps-managed when it carries:

* Argo CD: annotation or label `argocd.argoproj.io/tracking-id` or `argocd.argoproj.io/instance`;
* Flux: annotation or label `kustomize.toolkit.fluxcd.io/name` or `helm.toolkit.fluxcd.io/name`.

Argo CD's default tracking label (`app.kubernetes.io/instance`) is deliberately *not* used: Helm and many other tools set it too, so it would flag unrelated workloads.

## Behaviour

`safety.Evaluate` denies the change ("managed by argocd/flux ... use a pull request") unless the policy sets `allowGitOps: true`. Reason: a direct patch is either reverted by the reconciler
at the next sync (wasted change, flapping) or hides drift. `allowGitOps` exists for clusters where the reconciler is paused or `ignoreDifferences` covers the resources; it is not a recommendation.

## `rightsize propose`

`rightsize propose --recommendation rec.json --repo <dir> --file <path> [--format manifest|helm-values|kustomize]` writes the change where the repository declares it, commits on a new branch
`rightsize/<kind>-<name>-<id>` with the recommendation summary (every container, confidence, the ESTIMATE and its risks) in the message, and returns to the branch you were on. It never contacts a cluster.

* `--format manifest` (default): edits the Deployment/StatefulSet/DaemonSet document (multi-document YAML, comments and key order preserved).
* `--format helm-values --helm-key app=resources,proxy=sidecar.resources`: edits the chart's values at the key each container's resources live under. Every changed container must be mapped, the key must end in `.resources`, and the path to it must already exist (the values layout is the chart's contract and is never invented).
* `--format kustomize --file overlays/prod/kustomization.yaml [--base-file k8s/api.yaml]`: writes a strategic-merge patch `rightsize-<kind>-<name>.yaml` next to the kustomization and lists it under `patches` (once). With `--base-file` the recommendation is checked against the base exactly like a manifest edit; without it the result says the current values were not checked. A later recommendation updates the same patch and is checked against the values in it.
* A combined record (`rightsize plan --container app,proxy`) changes every container it names.
* `--push [--remote origin]` pushes the branch; `--open-pr [--base main]` then opens a pull request with the GitHub CLI (`gh pr create`, your own `gh` login), title and body from the commit. Both are **off by default**; a PR is never attempted without a push, and a failed PR says the branch is already pushed.

It refuses: an abstained or no-op recommendation, values in the repository that differ from the ones the recommendation was computed for (stale), a lowered or newly created limit,
an ambiguous match (two documents for the same workload), uncommitted changes in a file it would write, and paths outside the repository.
Verified: unit tests with temporary repositories, a local bare remote for `--push` and a fake PR opener (`operator/internal/gitops`), and `hack/lab-e2e2.sh` step 6 (manifest format). Not verified: `gh pr create` against GitHub from this command.

## What is still needed for a full integration (not built)

1. After merge, the same observation window and rollback rules, but rollback = revert commit.
2. Mapping from a live object to its source file (Argo CD `Application` source, Flux `Kustomization`/`HelmRelease`), which differs per repo layout: today you pass `--file`.

Until then, merging the pull request is the apply step, and watching the rollout after the merge is up to the reconciler and you.
