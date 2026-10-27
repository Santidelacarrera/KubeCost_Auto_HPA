# Releasing

What the pipeline does and what a person still has to do. Nothing here has been run for a tagged
release yet: images are already published from `main`, the Helm chart is published only on tags.

## What `release.yml` publishes

| Trigger | Result |
|---|---|
| push to `main` | multi-arch (amd64/arm64) images `ghcr.io/<owner>/kubecost-auto-hpa-{operator,engine}` tagged `sha-<short>` and `latest` |
| push of a `v*` tag | the same images tagged with the version, plus the Helm chart as an OCI artifact at `oci://ghcr.io/<owner>/charts/kubecost-auto-hpa` |

## Before the first tag: rehearsals (nothing is published)

| Step | Command | Needs | Status |
|---|---|---|---|
| Tag/version propagation, chart package, image references, OCI reference syntax, binary `--version` | `hack/release-dry-run.sh v0.1.0` | helm, go | **run and passing** where this was authored |
| Install, upgrade, rollback, uninstall of the chart on a throwaway kind cluster with locally built images | `hack/helm-rehearsal.sh` | docker, kind, kubectl, helm | **written, not run** (no Docker daemon there) |
| SBOM and image vulnerability scan | run the `supply-chain` workflow by hand (Actions tab) | GitHub | **not run** |
| Workflow syntax | `actionlint` | actionlint | clean |

Run the two unrun rehearsals and fix what they find **before** creating the tag.

## Cutting `v0.1.0` (requires your explicit go-ahead; it publishes)

1. `main` is green on `ci` and the latest `release` run on `main` succeeded; the rehearsals above passed.
2. Move the `Unreleased` entries of `CHANGELOG.md` under the new version and date; keep `Chart.yaml`
   `version`/`appVersion` in step (the chart job overrides them with the tag, the file should still match).
3. Tag and push (this is the step that publishes and signs):

   ```sh
   git tag -a v0.1.0 -m "v0.1.0"
   git push origin v0.1.0
   ```

4. Watch the `release` run: both `images` jobs, then `sign` and `chart`. Version, commit, SBOM, provenance and signatures
   are described in `docs/SUPPLY_CHAIN.md`; none of that path has run yet, so expect to iterate.
5. **Make the packages public** (first publication only): GHCR packages are created private. In the GitHub
   profile/organisation → Packages, open each of `kubecost-auto-hpa-operator`, `kubecost-auto-hpa-engine` and
   `charts/kubecost-auto-hpa` → Package settings → Change visibility → Public. Until then clusters
   need an image pull secret.
6. Verify from a clean machine, including the signature (`docs/SUPPLY_CHAIN.md`):

   ```sh
   helm pull oci://ghcr.io/<owner>/charts/kubecost-auto-hpa --version 0.1.0
   docker pull ghcr.io/<owner>/kubecost-auto-hpa-operator:0.1.0
   cosign verify ghcr.io/<owner>/kubecost-auto-hpa-operator:0.1.0 --certificate-identity ... --certificate-oidc-issuer https://token.actions.githubusercontent.com
   ```

## First install on a real cluster

Start in `dryRun` and read the decision before letting the controller act:

1. Install the chart with `--set kubecost.url=http://<kubecost-cost-analyzer>.<ns>:9090` (and `auth.kubecost.secretName` when
   KubeCost needs a bearer token; see `docs/OPERATIONS.md` and the `kubecost-credentials.example.yaml` sample).
2. Apply a `KubeCostAutoscaler` with `spec.dryRun: true` (see
   `operator/config/samples/autoscaling_v1alpha1_kubecostautoscaler.yaml`).
3. Check `kubectl get kubecostautoscaler -o wide` and `.status.lastDecision`, `.status.cost`, and the conditions. The
   KubeCost response shape was verified against OpenCost's source, not against a live KubeCost: a
   `Malformed`/`NoData` condition here is the signal to report.
4. Set `dryRun: false` once the decisions look right.

## Upgrade and rollback

```sh
# upgrade: Helm installs CRDs from crds/ only on first install and never changes them afterwards,
# so apply CRD changes yourself, first (additive changes are safe; check the release notes)
kubectl apply -f deploy/helm/kubecost-auto-hpa/crds/
helm upgrade <release> oci://ghcr.io/<owner>/charts/kubecost-auto-hpa --version <new> -n <ns> --reuse-values

# rollback the workloads and values
helm history <release> -n <ns>
helm rollback <release> <revision> -n <ns> --wait
```

* Helm does **not** roll CRDs back. A CRD change that removed or tightened a field cannot be undone by `helm rollback`; reapply the previous CRD yourself.
* Fastest safe stop without uninstalling: `kubectl annotate kubecostautoscalers --all -A autoscaling.kubecost.io/pause=true --overwrite` (decisions
  keep being reported, nothing is written; remember that nothing then scales the workload). Or delete a `KubeCostAutoscaler`: the controller stops
  writing replicas and an operator-owned fallback HPA (`kca-<name>`) is garbage-collected through its owner reference.
* `hack/helm-rehearsal.sh` exercises install, upgrade, rollback and uninstall on a throwaway kind cluster (written, not yet run).

## Test in an isolated cluster (procedure)

1. A disposable cluster (kind 1.33 or a scratch cloud cluster) with metrics-server; for the real KubeCost check, install KubeCost/OpenCost in it.
2. `hack/helm-rehearsal.sh` for the install/upgrade/rollback path.
3. Install the chart pointing at the cluster's KubeCost; create a `KubeCostAutoscaler` with `dryRun: true` on a test Deployment; run
   `go test -tags kubecost_live -run Live ./internal/kubecost` with `KUBECOST_LIVE_*` set (this is the first real contract check of the KubeCost integration).
4. Drive load on the test Deployment, watch `status.lastDecision` and the `kubecost_autoscaler_*` metrics for a full cycle, then set `dryRun: false`.
5. Follow `docs/BENCHMARK_PROCEDURE.md` for latency and error numbers; try the pause annotation, a KubeCost outage (network policy) and an operator restart.
