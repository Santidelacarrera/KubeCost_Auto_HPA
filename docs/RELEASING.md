# Releasing

What the pipeline does and what a person still has to do. Nothing here has been run for a tagged
release yet: images are already published from `main`, the Helm chart is published only on tags.

## What `release.yml` publishes

| Trigger | Result |
|---|---|
| push to `main` | multi-arch (amd64/arm64) images `ghcr.io/<owner>/kubecost-auto-hpa-{operator,engine}` tagged `sha-<short>` and `latest` |
| push of a `v*` tag | the same images tagged with the version, plus the Helm chart as an OCI artifact at `oci://ghcr.io/<owner>/charts/kubecost-auto-hpa` |

## Cutting `v0.1.0`

1. `main` is green on `ci` (operator incl. `govulncheck`, envtest, manifests, engine, e2e-kind, e2e-kca) and the latest
   `release` run on `main` succeeded.
2. Move the `Unreleased` entries of `CHANGELOG.md` under the new version and date; keep `Chart.yaml`
   `version`/`appVersion` in step (the chart job overrides them with the tag, the file should still match).
3. Tag and push:

   ```sh
   git tag -a v0.1.0 -m "v0.1.0"
   git push origin v0.1.0
   ```

4. Watch the `release` run: both `images` jobs, then `chart`.
5. **Make the packages public** (first publication only): GHCR packages are created private. In the GitHub
   profile/organisation → Packages, open each of `kubecost-auto-hpa-operator`, `kubecost-auto-hpa-engine` and
   `charts/kubecost-auto-hpa` → Package settings → Change visibility → Public. Until then clusters
   need an image pull secret.
6. Verify from a clean machine:

   ```sh
   helm pull oci://ghcr.io/<owner>/charts/kubecost-auto-hpa --version 0.1.0
   docker pull ghcr.io/<owner>/kubecost-auto-hpa-operator:0.1.0
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

## Rollback

`helm rollback`, or delete the `KubeCostAutoscaler`: the controller stops writing replicas, and an operator-owned
fallback HPA (`kca-<name>`) is garbage-collected through its owner reference.
