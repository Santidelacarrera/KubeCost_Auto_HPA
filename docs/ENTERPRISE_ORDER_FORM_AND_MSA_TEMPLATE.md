# Enterprise Order Form & Master Services Agreement (MSA) Template

This document provides the standard legal and procurement agreement template for **KubeCost Auto-HPA** commercial licenses, enterprise support subscriptions, and assisted Proof of Concept (POC) agreements.

---

## Part 1: Enterprise Order Form (Sample)

| Order Form Field | Enterprise Customer Specification |
|---|---|
| **Customer Legal Name:** | `[Customer Company Legal Entity, Inc.]` |
| **Customer Contact & Email:** | `[Name, Title, Email]` |
| **Billing Address:** | `[Company Billing Address]` |
| **Subscription Tier:** | `[ ] Enterprise Standard ($18/node/mo)`<br>`[ ] Enterprise Mission-Critical ($32/node/mo)`<br>`[ ] Value-Shared Model (18% verified savings)` |
| **Licensed Node Capacity:** | `[ e.g., 150 Kubernetes Nodes ]` |
| **Initial Term:** | `[ 12 Months / Annual Subscription ]` |
| **Billing Frequency:** | `[ Annual in Advance / Net 30 ]` |
| **Effective Date:** | `[ Date ]` |
| **Included Support SLA:** | Standard (4h initial response) or Mission-Critical (1h 24/7) |
| **Deployment Model:** | Self-hosted inside Customer Cloud (AWS / GCP / Azure / On-Prem) |

---

## Part 2: Master Services & Software License Agreement (MSA)

### 1. Definitions
- **"Software"** means the KubeCost Auto-HPA software system, including the Kubernetes Operator, the FastAPI FinOps Cost Engine, associated Helm charts, container images, and proprietary enterprise add-ons.
- **"Customer Environment"** means the private cloud VPCs, Kubernetes clusters, and compute nodes owned or managed by Customer.
- **"Node"** means any virtual machine or bare-metal server registered as a worker node within a Kubernetes cluster managed by the Software.

---

### 2. Commercial License Grant
Subject to the terms of this Agreement, Licensor grants to Customer a non-exclusive, non-transferable, worldwide commercial license during the Subscription Term to install, execute, and operate the Software solely within Customer Environments up to the licensed Node capacity specified on the applicable Order Form.

---

### 3. Data Privacy & Zero-Telemetry Guarantee
Licensor covenants and guarantees that:
1. The Software runs 100% locally within Customer's Kubernetes clusters and private networks.
2. The Software transmits **no external outbound telemetry, customer data, container metadata, or Personally Identifiable Information (PII)** to Licensor or third-party cloud services.
3. The Customer retains 100% exclusive ownership and confidentiality over all Prometheus metrics, financial data, and application workloads.

---

### 4. Support SLAs & Service Commitments
Licensor shall provide enterprise technical support based on the elected support tier:

| Severity Level | Definition | Response SLA | Target Workaround / Fix |
|---|---|:---:|:---:|
| **Severity 1 (Critical)** | Production cluster autoscaling halted; production service outage caused by Software | **< 1 hour (24/7/365)** | Continuous 24/7 until mitigated |
| **Severity 2 (High)** | Software degraded; operating in fallback mode; scaling delayed | **< 4 hours (Business Days)** | Within 24 hours |
| **Severity 3 (Medium)** | Non-production issue; minor defect; configuration questions | **< 1 business day** | Next release |
| **Severity 4 (Low)** | General feature requests and documentation inquiries | **< 2 business days** | Backlog roadmap review |

---

### 5. Intellectual Property & Commercial Indemnification
Licensor represents and warrants that the Software does not infringe any third-party patent, copyright, trademark, or trade secret. Licensor agrees to defend, indemnify, and hold harmless Customer against any third-party claim alleging that Customer's authorized use of the Software infringes any intellectual property rights, covering all damages and reasonable legal fees awarded against Customer.

---

### 6. The 60-Day ROI Performance Guarantee
If Customer enters into an annual Enterprise subscription and the Software fails to achieve verified cloud compute cost savings exceeding the annual license cost within sixty (60) days of live activation, Customer may terminate the Agreement upon written notice and receive a full, one hundred percent (100%) refund of all fees paid.

---

### 7. Limitation of Liability
Except for Licensor's indemnification obligations under Section 5 or breaches of confidentiality, neither party's aggregate liability under this Agreement shall exceed the total fees paid or payable by Customer under the applicable Order Form during the twelve (12) month period preceding the incident.

---

### 8. Signatures

**For Licensor:**  
Signature: __________________________  
Name: Santiago de la Carrera  
Title: Maintainer & Commercial Lead  
Date: _______________________________  

**For Customer:**  
Signature: __________________________  
Name: _______________________________  
Title: ________________________________  
Date: _______________________________  
