terraform {
  required_version = ">= 1.3.0"
  required_providers {
    helm = {
      source  = "hashicorp/helm"
      version = ">= 2.9.0"
    }
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = ">= 2.20.0"
    }
  }
}

resource "kubernetes_namespace" "kubecost_auto_hpa" {
  count = var.create_namespace ? 1 : 0

  metadata {
    name = var.namespace
    labels = {
      "app.kubernetes.io/name"       = "kubecost-auto-hpa"
      "app.kubernetes.io/managed-by" = "terraform"
      "pod-security.kubernetes.io/enforce" = "restricted"
    }
  }
}

resource "helm_release" "kubecost_auto_hpa" {
  name       = var.release_name
  repository = "oci://ghcr.io/santidelacarrera/charts"
  chart      = "kubecost-auto-hpa"
  version    = var.chart_version
  namespace  = var.namespace

  create_namespace = false

  set {
    name  = "dryRun"
    value = tostring(var.dry_run)
  }

  set {
    name  = "operator.replicaCount"
    value = var.operator_replicas
  }

  set {
    name  = "engine.replicaCount"
    value = var.engine_replicas
  }

  set {
    name  = "redis.enabled"
    value = tostring(var.redis_enabled)
  }

  set {
    name  = "serviceMonitor.enabled"
    value = tostring(var.service_monitor_enabled)
  }

  dynamic "set" {
    for_each = var.custom_helm_values
    content {
      name  = set.key
      value = set.value
    }
  }

  depends_on = [
    kubernetes_namespace.kubecost_auto_hpa
  ]
}
