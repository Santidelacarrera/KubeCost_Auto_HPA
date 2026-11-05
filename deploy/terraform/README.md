# Enterprise Terraform Module for KubeCost Auto-HPA

This module automates the production deployment of **KubeCost Auto-HPA** into any Kubernetes cluster (AWS EKS, GCP GKE, Azure AKS, or On-Prem) using the official OCI Helm chart:
`oci://ghcr.io/santidelacarrera/charts/kubecost-auto-hpa:0.1.0`

---

## Usage Example

```hcl
module "kubecost_auto_hpa" {
  source = "./deploy/terraform"

  namespace           = "kubecost-auto-hpa-system"
  create_namespace    = true
  chart_version       = "0.1.0"
  dry_run             = false # Set to true for initial 14-day zero-risk baseline
  operator_replicas   = 2
  engine_replicas     = 2
  redis_enabled       = true
  service_monitor_enabled = true

  custom_helm_values = {
    "engine.resources.limits.cpu" = "1000m"
    "engine.resources.limits.memory" = "512Mi"
  }
}
```

---

## Inputs

| Name | Type | Default | Description |
|---|---|:---:|---|
| `namespace` | string | `"kubecost-auto-hpa-system"` | Target Kubernetes namespace. |
| `create_namespace` | bool | `true` | Create namespace with restricted Pod Security Standards. |
| `chart_version` | string | `"0.1.0"` | OCI chart version. |
| `dry_run` | bool | `false` | Enable non-invasive financial shadow mode. |
| `operator_replicas` | number | `2` | Number of operator controller replicas. |
| `engine_replicas` | number | `2` | Number of FastAPI cost engine replicas. |
| `redis_enabled` | bool | `true` | Deploy internal telemetry cache. |
| `service_monitor_enabled` | bool | `true` | Scrape metrics with Prometheus Operator. |
| `custom_helm_values` | map(string) | `{}` | Optional Helm key-value overrides. |

---

## Outputs

| Name | Description |
|---|---|
| `namespace` | Deployed namespace. |
| `release_name` | Name of the Helm release. |
| `chart_version` | Version of the installed OCI chart. |
| `status` | Helm release deployment status. |
