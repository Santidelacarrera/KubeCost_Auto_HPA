variable "namespace" {
  type        = string
  default     = "kubecost-auto-hpa-system"
  description = "Target Kubernetes namespace for KubeCost Auto-HPA components."
}

variable "create_namespace" {
  type        = bool
  default     = true
  description = "Whether to create the Kubernetes namespace."
}

variable "release_name" {
  type        = string
  default     = "kubecost-auto-hpa"
  description = "Helm release name."
}

variable "chart_version" {
  type        = string
  default     = "0.1.0"
  description = "Version of the KubeCost Auto-HPA Helm OCI chart."
}

variable "dry_run" {
  type        = bool
  default     = false
  description = "Enable dry-run mode for zero-risk financial baseline evaluation."
}

variable "operator_replicas" {
  type        = number
  default     = 2
  description = "Number of operator controller replicas for high availability."
}

variable "engine_replicas" {
  type        = number
  default     = 2
  description = "Number of FastAPI cost engine replicas."
}

variable "redis_enabled" {
  type        = bool
  default     = true
  description = "Whether to deploy the in-cluster Redis telemetry cache."
}

variable "service_monitor_enabled" {
  type        = bool
  default     = true
  description = "Enable Prometheus Operator ServiceMonitor for automatic metrics scraping."
}

variable "custom_helm_values" {
  type        = map(string)
  default     = {}
  description = "Optional additional Helm values key-value pairs."
}
