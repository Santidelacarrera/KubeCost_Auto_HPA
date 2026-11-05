# KubeCost Auto-HPA: Production Reference Examples

This directory provides copy-paste, production-grade custom resource manifests (`KubeCostAutoscaler`) for common enterprise autoscaling patterns.

---

## Directory Overview

| File | Scenario | Key Features Highlighted |
|---|---|---|
| **[`01-dry-run-evaluation.yaml`](01-dry-run-evaluation.yaml)** | 14-Day POC Evaluation | `dryRun: true` mode. Calculates financial telemetry without changing live pods. |
| **[`02-production-cost-autoscaler.yaml`](02-production-cost-autoscaler.yaml)** | High-Traffic Microservice | Active scaling, stabilization anti-flapping, and automated fallback to native HPA. |
| **[`03-spot-instance-workload.yaml`](03-spot-instance-workload.yaml)** | Spot / Preemptible Fleet | Spot economic weighting (`spotMultiplier: "0.35"`) and preemption buffering. |
| **[`04-budget-capped-service.yaml`](04-budget-capped-service.yaml)** | Cost-Constrained Service | Hard dollar budget ceilings (`maxHourlyCostUSD`) with emergency headroom overrides. |

---

## Quickstart Guide

### 1. Apply the Custom Resource
```bash
kubectl apply -f examples/01-dry-run-evaluation.yaml
```

### 2. Inspect Autoscaler Decision and Status
```bash
kubectl describe kubecostautoscaler payment-service-dryrun
```

Look for the `Status.Decision` block:
```yaml
Status:
  Decision:
    Type:           Hold
    Replicas:       2
    Reason:         demand steady at 45% (target 70%); hourly cost $0.62 within $1.50 cap
  Dry Run:          true
  Current Replicas: 2
  Conditions:
    - Type:         Ready
      Status:       "True"
      Reason:       SignalsValid
```

### 3. Temporarily Pause Autoscaling (Emergency Override)
To freeze scaling immediately without deleting the CRD:
```bash
kubectl annotate kubecostautoscaler payment-service-dryrun "autoscaling.kubecost.io/pause=true"
```
To resume:
```bash
kubectl annotate kubecostautoscaler payment-service-dryrun "autoscaling.kubecost.io/pause-"
```
