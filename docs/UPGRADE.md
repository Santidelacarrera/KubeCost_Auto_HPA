# Versioning and upgrades

## Versions

The project follows [Semantic Versioning](https://semver.org). One version number covers the operator image, the engine image,
the `rightsize` binary and the Helm chart: the chart's `version` and `appVersion` are set to the tag at release time
([RELEASING.md](RELEASING.md)).

| Version | State | Notes |
|---|---|---|
| `v0.1.0` | Published 2026-11-05 (images and chart on GHCR, signed) | The only published release |
| `v0.2.0` | Prepared on the branch `release/v0.2.0`. **Not tagged, not published** | Everything since `v0.1.0` up to the eleventh pass |
| Unreleased | On the working branch | The twelfth pass ([CHANGELOG.md](../CHANGELOG.md)) |

While the major version is 0, a minor version (0.x) may change behaviour. Every such change is listed under "Changed" in the
changelog with what to do. Patch versions (0.x.y) contain fixes only.

## What "supported" means

| Version | Support |
|---|---|
| Latest minor | Fixes and security fixes |
| Previous minor | Security fixes for 90 days after the next minor is released |
| Older | None. Upgrade first |

Kubernetes: the versions the project runs in CI and validation, currently **1.33 to 1.36**. A Kubernetes minor is added once
the suite passes on it and dropped when upstream stops supporting it. See [COMPATIBILITY.md](COMPATIBILITY.md) for exactly
what ran where.

This is the policy the project commits to. There is no contractual SLA behind it ([SUPPORT.md](SUPPORT.md)).

## Upgrade procedure

1. Read the changelog between your version and the target, especially "Changed" and "Security".
2. Put the operator in observe-only if it is not (`operator.observeOnly=true`) and wait until no `WorkloadRecommendation` is in
   `Applying`.
3. Apply the CRDs of the target version. Helm installs CRDs once and never upgrades them:

   ```bash
   kubectl apply -f deploy/helm/kubecost-auto-hpa/crds/
   ```

4. Upgrade the release:

   ```bash
   helm upgrade kubecost-auto-hpa oci://ghcr.io/santidelacarrera/charts/kubecost-auto-hpa --version <target> \
     -n kubecost-auto-hpa-system --reuse-values
   ```

5. Check: pods ready, no restart loop, `kubectl get kubecostautoscaler,kubecosthpa,wrec -A` shows the same objects, conditions
   healthy.
6. Restore your previous mode.

Skipping step 3 is the one known way to break an upgrade: an operator that expects a CRD the cluster does not have restarts
until the CRD is applied. This was reproduced and is documented in [TROUBLESHOOTING.md](TROUBLESHOOTING.md).

Verified: upgrade from the chart before the applier existed, rollback, and re-upgrade, on the lab cluster ([LAB.md](LAB.md)
result 10); install, upgrade, rollback, uninstall on AKS (result 15). Not verified: an upgrade between two **published**
versions, because only one has been published.

## Rolling back an upgrade

```bash
helm rollback kubecost-auto-hpa <revision> -n kubecost-auto-hpa-system
```

CRDs are not rolled back by Helm. That is safe while fields are only added: the older operator ignores fields it does not
know. More in [ROLLBACK.md](ROLLBACK.md).

## CRD compatibility

* All kinds are `v1alpha1`, a single served and stored version. No conversion webhook.
* Minor versions add optional fields only. Objects written by an older version stay valid.
* A removal or rename will come as a new API version, with both served for at least one minor release.
* Recommendation records (the JSON in `spec.record` and in audit files) are additive too: a test checks that records written
  before a field existed still parse.

## Migrations

| From | To | What to do |
|---|---|---|
| `v0.1.0` | `v0.2.0` | Apply the CRDs (the `WorkloadRecommendation` kind is new). Policies gain `maxRecommendationAge` (default 7 days): an approval of an older recommendation is now refused, so re-plan anything older. No object needs editing |
| `v0.2.0` | next | Chart values `features.*` are deprecated and do nothing; remove them from your values files before the release that drops them. `KubeCostAutoscaler` objects that set `spec.predictive`, `spec.oomProtection` or `spec.nodeBinPacking` now show `UnsupportedFields=True`: remove the fields or ignore the condition; behaviour is unchanged. The Kustomize base now starts in observe-only: if you relied on it writing, remove `--observe-only` in your overlay deliberately. `deploy/rbac/rightsize.yaml` adds `list persistentvolumes` to the read role: re-apply it to get zone-aware node analysis |

## Deprecation policy

A deprecated value, flag or field keeps working for at least one minor version, is marked in the place people read (the
`values.yaml` comment, `--help`, the CRD description) and is listed in the changelog under "Deprecated" with its removal
version.
