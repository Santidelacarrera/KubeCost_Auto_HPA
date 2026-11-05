# Enterprise Security & Compliance White Paper

This white paper provides architecture details, trust boundaries, data privacy guarantees, and RBAC justification for enterprise CISOs, InfoSec teams, and compliance auditors evaluating **KubeCost Auto-HPA**.

---

## 1. Zero-Trust Architecture & Data Boundary

### 100% In-Cluster / Air-Gapped Operation
- **No External Telemetry:** KubeCost Auto-HPA runs entirely inside your virtual private cloud (VPC) / Kubernetes boundary. It contains **no telemetry, no tracking, and no external phoning home**.
- **Network Isolation:** Fully compatible with Kubernetes `NetworkPolicy` egress rules restricting internet access.
- **Air-Gapped Ready:** Can be mirrored to private container registries (ECR, GCR, Harbor, Artifactory) and installed in isolated environments without public internet access.

### Data Privacy & Zero PII Guarantee
- **No Application Payload Inspection:** The operator and engine only read operational metrics:
  - Pod CPU and memory request/limit values.
  - Aggregated Prometheus metric counters (e.g., HTTP request rates and latency histograms).
  - KubeCost namespace/controller cost allocation figures.
- **Zero Customer Data / PII:** The system never intercepts, logs, or processes HTTP request bodies, customer records, database credentials, or application data.

---

## 2. Pod Security Standards (PSS) & Container Hardening

All container workloads comply with the Kubernetes **PodSecurityStandard: Restricted** profile:

| Security Control | Operator Container | Engine Container | Redis (Cache) |
|---|:---:|:---:|:---:|
| **Non-Root Execution** | Yes (`UID 65532`) | Yes (`UID 65532`) | Yes (`UID 999`) |
| **Privilege Escalation** | `false` | `false` | `false` |
| **Linux Capabilities** | `drop: [ALL]` | `drop: [ALL]` | `drop: [ALL]` |
| **Read-Only Root Filesystem** | `true` | `true` | Ephemeral emptyDir only |
| **Seccomp Profile** | `RuntimeDefault` | `RuntimeDefault` | `RuntimeDefault` |
| **Base Image** | `gcr.io/distroless/static` (Zero OS CVEs) | `python:3.14-slim` (pip removed, 0 CVEs) | `redis:7.4-alpine` |

---

## 3. RBAC Least-Privilege Justification

The operator's `ClusterRole` permissions are strictly bounded and verified via automated test suites (`operator/internal/rbaccheck`):

| API Group | Resource | Verbs | Business & Security Justification |
|---|---|---|---|
| `""` (core) | `events` | `create, patch` | Publishing scaling audit logs to Kubernetes native events. |
| `""` (core) | `nodes` | `get, list, watch, patch` | Required **only** for Spot instance draining: cordoning and annotating nodes undergoing eviction. |
| `""` (core) | `pods` | `get, list, watch` | Observing current pod status and CPU/memory allocations. |
| `""` (core) | `pods/eviction` | `create` | Respectfully evicting pods from interrupted Spot nodes via the standard Eviction API (guaranteeing PDB adherence). |
| `""` (core) | `pods/resize` | `patch` | Executing in-place CPU/memory resizing without restarting containers (Kubernetes 1.33+ subresource). |
| `apps` | `deployments` | `get, list, watch` | Reading target deployment specifications and replica counts. |
| `apps` | `deployments/scale` | `get, update` | Updating target replicas through the native subresource. |
| `autoscaling.kubecost.io` | `kubecostautoscalers` | `get, list, watch` | Managing the custom resource specification. |
| `autoscaling.kubecost.io` | `*/status` | `get, update` | Writing decisions, cost indicators, and condition timestamps. |

---

## 4. Supply Chain Integrity & Cryptographic Attestation

To safeguard against software supply chain attacks, every release artifact adheres to modern software security standards:
- **SLSA Provenance (Level 3):** Built using verifiable GitHub Actions runners with maximum provenance mode.
- **Software Bill of Materials (SBOM):** Machine-readable SPDX and CycloneDX SBOMs generated at build time.
- **Keyless Sigstore / Cosign Signatures:** Every container image and Helm chart OCI artifact is cryptographically signed using GitHub OIDC identity and verifiable against the public Rekor transparency log.
- **Vulnerability Scans:** Automated scanning via Aqua Security Trivy and `govulncheck` in CI pipelines before publication.
