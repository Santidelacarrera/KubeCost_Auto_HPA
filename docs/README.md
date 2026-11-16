# Documentation

Start with [STATUS.md](STATUS.md): it says, per feature, what is verified, experimental or only documented.

## Run it

| Document | What it covers |
|---|---|
| [INSTALL.md](INSTALL.md) | Helm and kubectl installation, GKE and AKS settings, upgrade, safe uninstall |
| [CONFIGURATION.md](CONFIGURATION.md) | The three operating modes, chart values, operator flags, the policy file |
| [GKE.md](GKE.md), [AKS.md](AKS.md) | What was validated on each provider, required permissions, what is pending |
| [UPGRADE.md](UPGRADE.md) | Versioning, supported versions, CRD compatibility, migrations |
| [ROLLBACK.md](ROLLBACK.md) | Rolling back a change, a recommendation, the chart |
| [OPERATIONS.md](OPERATIONS.md), [TROUBLESHOOTING.md](TROUBLESHOOTING.md) | Day-2 operation, alerts, common failures |
| [OBSERVABILITY.md](OBSERVABILITY.md) | Metrics, dashboards, proposed SLOs |
| [PILOT.md](PILOT.md) | A supervised pilot: steps and exit criteria |

## Understand it

| Document | What it covers |
|---|---|
| [ARCHITECTURE.md](ARCHITECTURE.md) | Components, data flow, execution flow, what was deliberately not built |
| [RECOMMENDER.md](RECOMMENDER.md) | The rightsizing algorithm, trend forecast, spikes, backtest |
| [KUBECOST_AUTOSCALER.md](KUBECOST_AUTOSCALER.md) | Cost-aware replica scaling |
| [SAFETY.md](SAFETY.md) | Policy, approvals, signed approvals, audit, rollback conditions |
| [NODES.md](NODES.md) | Node analysis, what GKE and AKS do, what this product never does |
| [FINOPS.md](FINOPS.md), [COST.md](COST.md) | Cost model, savings levels, billing exports, reconciliation |
| [GITOPS.md](GITOPS.md) | Argo CD and Flux |
| [API.md](API.md), [CRD.md](CRD.md) | CLI, engine HTTP API, custom resources |
| [adr/](adr/) | Architecture decision records |

## Trust it

| Document | What it covers |
|---|---|
| [STATUS.md](STATUS.md) | Feature status and maturity |
| [KNOWN_LIMITATIONS.md](KNOWN_LIMITATIONS.md) | What it does not do, and the operational risks |
| [RBAC.md](RBAC.md) | Every permission and why |
| [THREAT_MODEL.md](THREAT_MODEL.md), [SUPPLY_CHAIN.md](SUPPLY_CHAIN.md), [../SECURITY.md](../SECURITY.md) | Threats, controls, SBOM, scanning, reporting a vulnerability |
| [LAB.md](LAB.md), [reports/](reports/) | Every real-cluster run with its evidence file |
| [COMPATIBILITY.md](COMPATIBILITY.md) | Versions that were actually run |
| [BENCHMARKING.md](BENCHMARKING.md), [BENCHMARKS.md](BENCHMARKS.md) | Competitive benchmark method and harness; the simulated comparison |

## Maintain and sell it

| Document | What it covers |
|---|---|
| [SUPPORT.md](SUPPORT.md) | Supported versions, incident handling, recovery |
| [RELEASING.md](RELEASING.md), [../CHANGELOG.md](../CHANGELOG.md) | Release procedure and history |
| [ROADMAP.md](ROADMAP.md) | Prioritised backlog |
| [COMMERCIAL.md](COMMERCIAL.md), [LICENSE_KEYS.md](LICENSE_KEYS.md), [LICENSING.md](LICENSING.md) | Editions, pricing models, signed licence files, open-source licence review |
| [MVP.md](MVP.md) | What an MVP is for this product, what is done, what is left |

## Sales drafts (not evidence)

These were written as sales material before the product was measured. Each now opens with a notice saying so. Their figures are
models or targets, not results: `BUSINESS_CASE_AND_ROI.md`, `COMPETITIVE_BATTLECARD.md`, `PITCH_DECK.md`, `EXECUTIVE_ONE_PAGER.md`,
`DEMO_SCRIPT.md`, `POC_PLAYBOOK.md`, `SALES_OUTREACH_PLAYBOOK.md`, `DISCOVERY_CALL_GUIDE.md`, `GTM_COMMUNITY_LAUNCH_PACK.md`,
`STARTUP_SPINOFF_AND_BUSINESS_PLAN.md`, `SCALEOPS_REVERSE_ENGINEERING_AND_FEATURE_PARITY.md`,
`LOAD_TEST_AND_HIGH_CONCURRENCY_REPORT.md`, `ENTERPRISE_SECURITY_WHITE_PAPER.md`, `DATA_PROTECTION_AND_PRIVACY_COMPLIANCE.md`,
and the legal templates `DATA_PROCESSING_ADDENDUM_TEMPLATE.md` and `ENTERPRISE_ORDER_FORM_AND_MSA_TEMPLATE.md` (templates, not
reviewed by a lawyer).
