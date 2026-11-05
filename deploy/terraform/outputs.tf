output "namespace" {
  value       = var.namespace
  description = "Namespace where KubeCost Auto-HPA is deployed."
}

output "release_name" {
  value       = helm_release.kubecost_auto_hpa.name
  description = "Helm release name of the deployment."
}

output "chart_version" {
  value       = helm_release.kubecost_auto_hpa.version
  description = "Version of the deployed Helm chart."
}

output "status" {
  value       = helm_release.kubecost_auto_hpa.status
  description = "Status of the Helm release."
}
