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
