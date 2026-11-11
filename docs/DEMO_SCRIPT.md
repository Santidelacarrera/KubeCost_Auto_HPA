# KubeCost Auto-HPA — 15-Minute Live Sales Demo Runbook

This runbook guides Sales Engineers and Solutions Architects through a live, high-impact demonstration of **KubeCost Auto-HPA** for prospective enterprise buyers (FinOps leads, VP of Infrastructure, Lead SREs).

---

## Demo Overview & Timeline

- **Duration:** 15 minutes
- **Environment:** Local `kind` cluster or staging EKS/GKE cluster
- **Tools Used:** `kubectl`, `hey` / `locust` (traffic generator), Grafana dashboard

| Time | Segment | Value Demonstrated |
|---|---|---|
| **00:00 – 03:00** | Scene 1: The Idle Waste Problem | Show baseline over-provisioned workload wasting 70%+ compute spend. |
| **03:00 – 06:00** | Scene 2: Non-Invasive `dryRun` Mode | Show zero-risk evaluation and real-time dollar recommendation telemetry. |
| **06:00 – 10:00** | Scene 3: Live In-Place Pod Resizing | Inject 5x traffic surge; watch latency protected & resources dynamically scale without pod restarts. |
| **10:00 – 13:00** | Scene 4: Spot Interruption Zero-Downtime | Simulate cloud spot preemption; watch graceful proactive migration. |
| **13:00 – 15:00** | Scene 5: Verified ROI & Q&A | Review Grafana savings ledger; show 34% cost drop; close with POC proposal. |

---

## Preparation Checklist (Prior to Call)

1. Ensure demo cluster is up with Prometheus and Grafana:
   ```bash
   kubectl get pods -n kubecost-auto-hpa-system
   ```
2. Verify Grafana dashboard is loaded:
   ```bash
   kubectl port-forward svc/grafana 3000:80 -n monitoring
   ```
3. Have a terminal split-screen ready:
   - **Left pane:** `kubectl get pods -w` and resource allocations
   - **Right pane:** Grafana dashboard with real-time `$/request` and `p99 latency`

---

## Scene 1: The Problem — Idle Spend & Static YAML (00:00 – 03:00)

### Talking Track:
> *"Today, most platform teams configure pod CPU and memory requests based on worst-case peak traffic. Let's look at this sample e-commerce service running right now."*

### Actions:
```bash
kubectl get deployment payment-service -o jsonpath='{.spec.template.spec.containers[0].resources}' | jq .
```
- **Point out:** The deployment requests `2000m` CPU and `4Gi` memory per pod.
- **Show Grafana:** Actual utilization is only `150m` CPU.
- **The Pitch:**
  > *"You are paying cloud providers for 92% idle capacity just as 'insurance' against traffic spikes. Over 100 nodes, that's \$80,000 to \$150,000 in pure annual waste."*

---

## Scene 2: Zero-Risk Shadow Baseline (`dryRun: true`) (03:00 – 06:00)

### Talking Track:
> *"Enterprise teams are often hesitant to let an automated tool resize pods in production. That's why KubeCost Auto-HPA features a zero-risk dry-run mode."*

### Actions:
Apply a sample `KubeCostAutoscaler` CRD in dry-run mode:
```yaml
apiVersion: autoscaling.kubecost.io/v1alpha1
kind: KubeCostAutoscaler
metadata:
  name: payment-service-scaler
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: payment-service
  dryRun: true
  minReplicas: 2
  maxReplicas: 10
  metrics:
    targetCostPerRequest: "0.00015" # $0.00015 / req
    maxP99LatencyMs: 150
```
```bash
kubectl apply -f scaler-dryrun.yaml
```
- Open Grafana **"KubeCost Auto-HPA: Dry-Run Financial Simulation"** panel.
- **Point out:** The engine immediately begins calculating recommended replica count and projected annual dollar savings without touching the live deployment.
- **The Pitch:**
  > *"You can run this in production for 2 weeks. It gives your FinOps and platform teams total visibility into projected savings with zero operational risk."*

---

## Scene 3: Live Actuation & Non-Disruptive In-Place Resizing (06:00 – 10:00)

### Talking Track:
> *"Now let's switch actuation to live mode (`dryRun: false`) and inject a simulated flash-sale traffic spike."*

### Actions:
1. Enable active scaling:
   ```bash
   kubectl patch kubecostautoscaler payment-service-scaler --type='merge' -p '{"spec":{"dryRun":false}}'
   ```
2. Start traffic load test:
   ```bash
   hey -z 60s -q 200 -c 50 http://payment-service.default.svc.cluster.local/checkout
   ```
3. Watch pod scaling in real time:
   ```bash
   kubectl get pods -l app=payment-service -o wide -w
   ```
4. **Point out in Grafana:**
   - As RPS surges from 10 to 1,000, p99 latency approaches the 150ms ceiling.
   - The Cost Engine detects the unit economics shift and scales pods up.
   - **Crucial highlight:** Pods resize **in-place** without terminating or dropping ongoing TCP sessions.
   - Once traffic subsides, the engine gently contracts resources down to the optimal cost-per-request baseline.

---

## Scene 4: Spot Instance Drainer Simulation (10:00 – 13:00)

### Talking Track:
> *"Spot instances can save 60%–80% on compute, but engineering teams fear sudden 2-minute cloud termination notices. Let's see how our native Spot Drainer handles a preemption notice."*

### Actions:
Simulate AWS EC2 Spot Interruption / GCP Preemption event:
```bash
kubectl annotate node demo-spot-worker-1 "kubecost.io/spot-interruption-notice=true"
```
- Watch the operator log and node status:
  ```bash
  kubectl get events -n kubecost-auto-hpa-system --sort-by='.metadata.creationTimestamp' | tail -n 10
  ```
- **Point out:**
  1. The node is immediately cordoned so no new pods are scheduled.
  2. Targeted eviction begins gracefully, allowing active in-flight requests to complete.
  3. Workloads are proactively spun up on On-Demand nodes before the 120-second cloud cutoff.
  4. Zero 502 or 504 errors recorded by client load tests.

---

## Scene 5: Verified ROI Summary & Next Steps (13:00 – 15:00)

### Talking Track:
> *"Let's look at the financial summary after running for just 10 minutes in this test cluster."*

### Actions:
- Open Grafana **"Executive FinOps Realized Savings"** panel.
- **Show the numbers:**
  - Initial baseline cost: **\$120/day**
  - Optimized KubeCost Auto-HPA cost: **\$76/day**
  - **Net compute reduction: 36.6%** with 100% latency SLA compliance.
- **Closing Proposition:**
  > *"We can deploy this exact setup into your non-production or staging cluster in 15 minutes today via Helm or Terraform under our 14-Day Free Pilot program. What does your schedule look like this Thursday to set up the dry-run baseline?"*
