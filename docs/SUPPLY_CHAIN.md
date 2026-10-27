# Software supply chain: what exists and what has been verified

Status words: **VERIFIED** = executed here and the result checked; **PREPARED** = written and statically
checked but not executed (it needs Docker, a registry or GitHub); **BLOCKED** = cannot run in the authoring environment.
Nothing in this file has been published, signed in a real registry, or run on a tagged release.

| Item | Where | Status |
|---|---|---|
| Go SBOM (CycloneDX 1.6) of the operator binary | `cyclonedx-gomod app -main cmd` (also in `.github/workflows/supply-chain.yml`) | **VERIFIED**: generated here, 62 components, versions equal `go.mod` (e.g. controller-runtime v0.19.7, go-redis v9.23.0) |
| Image SBOM (SPDX) and SLSA provenance attached to the image | `release.yml`: `sbom: true`, `provenance: mode=max`, **tags only** | **PREPARED**: BuildKit attestations; not run |
| Image signing (keyless Sigstore/cosign) and verification | `release.yml` job `sign`, **tags only**, by digest | **PREPARED** (workflow linted with actionlint). Sign/verify **mechanics** VERIFIED with an ephemeral key on a file, including a tamper negative control; the keyless, registry and GitHub-OIDC path was not run |
| Chart signing | `release.yml` job `chart`, signs the pushed chart digest | **PREPARED** |
| Source vulnerability scan (govulncheck) | CI job `operator` | runs in CI on every push; **BLOCKED locally** (the sandbox cannot reach vuln.go.dev); last CI result was green |
| Image vulnerability scan (Trivy, HIGH/CRITICAL, fixable only) | `.github/workflows/supply-chain.yml` (manual + weekly) | **PREPARED**, never run; there is no Docker daemon here, and the action version was not verified to exist |
| Image-level SBOM from the final image contents (OS packages) | BuildKit SBOM above | **PREPARED** |
| Reproducible version/commit identification | `-ldflags -X main.version/commit` (Dockerfile `ARG VERSION/COMMIT`, `release.yml` `build-args`), `--version`, metric `kubecost_auto_hpa_build_info`, OCI labels from metadata-action | **VERIFIED** for the binary (a real build reports the injected values, test `TestVersionIsInjectedAtLinkTime`, `hack/release-dry-run.sh`); the Docker/CI propagation is **PREPARED** |
| Least-privilege workflow permissions | `ci.yml`: `contents: read`; `release.yml`: `contents: read` by default, `packages: write` only on jobs that push, `id-token: write` only on signing jobs | **VERIFIED** statically (actionlint) |
| Secrets in CI | only `GITHUB_TOKEN`; no repository secrets are referenced; pull requests from forks do not receive it | **VERIFIED** by reading the workflows |
| Build reproducibility | `-trimpath`, `CGO_ENABLED=0`, `go.sum`, module cache | partial: base images are floating tags (`golang:1.27`, `python:3.14-slim`, distroless `:nonroot`), actions are pinned by tag not by commit SHA; not byte-for-byte reproducible |

## Bugs found and fixed while preparing this

* **The chart job could not have worked.** `helm push` to `oci://ghcr.io/Santidelacarrera/charts` is rejected by helm itself
  (`invalid_reference: invalid repository`: OCI names must be lower-case). Reproduced with the real `helm` binary; the workflow now lower-cases the
  owner. The image jobs were never affected (metadata-action lower-cases). This was latent: no tagged release had ever run.

## How a consumer verifies a release (after the first tag; not possible today)

```bash
# image: signature by the release workflow of this repository
cosign verify ghcr.io/<owner>/kubecost-auto-hpa-operator:0.1.0 \
  --certificate-identity "https://github.com/<owner>/KubeCost_Auto_HPA/.github/workflows/release.yml@refs/tags/v0.1.0" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
# provenance and SBOM attestations stored by BuildKit
docker buildx imagetools inspect ghcr.io/<owner>/kubecost-auto-hpa-operator:0.1.0 --format '{{json .Provenance}}'
docker buildx imagetools inspect ghcr.io/<owner>/kubecost-auto-hpa-operator:0.1.0 --format '{{json .SBOM}}'
```

## Not done

Pinning actions and base images by digest, an image scan on every pull request, a published Go SBOM next to each release, SLSA level claims, and a
Dependabot rule for pinned digests. Do not describe any image as "signed", "with SBOM" or "free of vulnerabilities" until the tag release has run
and the commands above succeed.

## Local evidence from the latest validation round (sandbox, not GitHub)

Everything below was run in the validation sandbox. None of it is a GitHub Actions run, and none of it is a published
artifact.

| Step | Result |
|---|---|
| Build of the operator image from `operator/Dockerfile` | **Not possible as written** in the sandbox: `go mod download` fails because the sandbox egress proxy re-signs TLS. Built with a sandbox-only layer that trusts the proxy CA (not part of the repo). Tag `kca-operator:local-test`. |
| Image user | `65532:65532` (non-root), entrypoint `/manager`, size ≈ 81 MB |
| Version metadata | `--version` prints `0.1.0-rc.test (commit b86308d, go1.27.3)` |
| Trivy 0.67.2 vulnerability scan (HIGH and CRITICAL, then all severities) | 6 OS packages (Debian 13.7) and 64 Go packages scanned: **0 findings**. Database from `mirror.gcr.io/aquasec/trivy-db:2`, not the official source; the database date is not recorded here |
| SBOM of the image (CycloneDX 1.6, Trivy) | 72 components, including `stdlib` |
| Push to a local registry, then sign with cosign v2.4.1 | **Ephemeral key pair**. Verify with the matching public key: pass. Verify with another key: fail (exit 12). **Keyless signing is not validated**: it needs GitHub OIDC and the public transparency log, which the sandbox cannot reach |
| `govulncheck` | Not run: `vuln.go.dev` returns `Forbidden` from the sandbox proxy (a policy denial; not worked around) |
| `hack/helm-rehearsal.sh` on kind | **Not run.** kind v0.27.0 with `kindest/node:v1.33.4` starts, but the control plane never comes up: `runc create failed: can't get final child's PID`. Plain and privileged containers work, so this is a nested-cgroup limitation of the sandbox, not a chart result. CI's `e2e-kind` and `e2e-kca` jobs on GitHub runners passed on b86308d |

Not verified by any of the above: the image on a registry you control, the SBOM as a release asset, provenance
attestations, and keyless signing with the release workflow.

## Keyless signing and provenance: what can be checked where

| Check | Locally (no GitHub) | Needs an authorised CI run |
|---|---|---|
| The workflow syntax and permissions | Yes: `actionlint`, reading `release.yml` | No |
| `id-token: write` only on the signing jobs | Yes: grep of `release.yml` (lines 60–63 and 92–95) | No |
| `cosign sign` and `cosign verify` with an ephemeral key | Yes (done: verify with the right key passes, with a wrong key fails) | No |
| Keyless `cosign sign --yes` with GitHub OIDC | **No.** Needs the OIDC token of a real run and Fulcio | Yes: a run on a tag or `workflow_dispatch` in a test repository or fork |
| Transparency log entry (Rekor) | **No.** The sandbox cannot reach the public log | Yes, same run |
| SLSA provenance from BuildKit (`provenance: mode=max`) | **No.** Only generated on `v*` tags | Yes: a tag build in a test repository, then `docker buildx imagetools inspect` and the attestation check |
| SBOM attestation on the registry image | **No** (the local SBOM is a file, not an attestation) | Yes, same tag build |

**No permanent secrets are needed.** Keyless signing uses the job's OIDC identity. Do not add a long-lived signing key
to the repository or to repository secrets to make a check pass; the ephemeral-key check above is only a test of the
mechanism.

**Requirements for the authorised run:** a test repository or fork where tag creation is allowed, a registry the
workflow can push to, and an owner who accepts that the image is pushed there. The run is a release rehearsal. It is
not a publication.

**Not validated until that run has passed:** keyless signing, the Rekor entry, the provenance attestation, and the SBOM
attestation. Until then the release workflow is reviewed but not run.
