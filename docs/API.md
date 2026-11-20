# Interfaces

Three interfaces exist: the Kubernetes custom resources ([CRD.md](CRD.md)), the `rightsize` command line, and the HTTP API of
the Python engine. There is no public REST API of the operator: its interface is the Kubernetes API.

## `rightsize` CLI

Read-only unless `--execute` is given together with an explicit `--context`. `rightsize <command> --help` lists the flags.

| Command | Reads | Writes | Purpose |
|---|---|---|---|
| `plan` | Prometheus, cluster | nothing (`--publish`: one `WorkloadRecommendation`) | Recommendation plus cluster facts (HPA, VPA, opt-out, GitOps) |
| `publish` | a recommendation file | one `WorkloadRecommendation` | Publish a record made elsewhere |
| `approve` | cluster | the approval (annotation, or `status.approval` with `--cr`) | Approve one exact recommendation ID; `--sign-key` signs it |
| `apply` | recommendation, policy, cluster | the workload, only with `--execute` | Policy check, apply, observe, automatic rollback |
| `rollback` | cluster | the workload | Restore the recorded previous state and verify it |
| `propose` | recommendation, a Git checkout | a commit on a new branch; `--push --open-pr` | GitOps change instead of a direct apply |
| `report` | audit file | nothing | Acceptance, rollback ratio, kept reductions, projected saving |
| `backtest` | Prometheus | nothing | Replay percentile/headroom settings on your history |
| `nodes` | cluster | nothing | Node candidate analysis; `--plan` adds a manual drain plan ([NODES.md](NODES.md)) |
| `cost` | cluster, optional billing file | nothing | Allocation, budgets, deviations |
| `reconcile` | two FOCUS billing exports | nothing | Before/after bill comparison and the saving's level ([FINOPS.md](FINOPS.md)) |
| `survey` | Prometheus, cluster | nothing (Markdown on stdout; `--out-dir` keeps each record) | One report for a namespace: `plan` for every container of every Deployment and StatefulSet, plus the node analysis |
| `render` | the JSON outputs of `plan`, `nodes`, `fleet`, `reconcile` | nothing (Markdown on stdout) | One readable report; every saving keeps its level |
| `fleet` | several clusters (`--contexts a,b,c`) | nothing | One read-only report over several clusters, with totals |
| `license issue`, `license verify` | a key, a licence file | `issue`: one licence file | Sign a licence (vendor side) and verify one offline ([LICENSE_KEYS.md](LICENSE_KEYS.md)) |
| `keygen` | — | a key pair file | Ed25519 key for signed approvals and for licences |
| `audit-verify` | audit file | nothing | Verify the audit hash chain |

Output is JSON on stdout, errors on stderr, non-zero exit on failure. `recommend` is the older single-purpose binary behind
`plan`.

## Engine HTTP API

The engine (`engine/`, Python) serves the operator's `KubeCostHPA` decisions. It listens on port 8000 inside the cluster; the
chart does not expose it outside.

### Authentication

Every `/v1/*` route needs a bearer credential when `ENGINE_TOKEN` is set (always, with `KUBECOST_ENV=production`): the engine
token (operator to engine), the admin token, or a tenant API key. Public routes: `/healthz`, `/health`, `/readyz`. Bodies are
limited to 1 MiB; `NaN` and `Infinity` are rejected; rate limits return `429` with `Retry-After`. TLS and mTLS:
`TLS_CERT`/`TLS_KEY`, `TLS_CLIENT_CA`. Environment reference: `.env.example`.

Requests require a single decimal `Content-Length`; transfer encoding and
duplicate identity headers or JSON keys are rejected. Stalled bodies return
408 when possible. See [HTTP limits](CONFIGURATION.md#engine-http-limits).
The enterprise rightsizing API preserves existing CPU/memory limits while
recommending request reductions and rejects non-boolean OOM signals.

### Routes that compute on the caller's data

| Route | Purpose |
|---|---|
| `POST /v1/evaluate` | The decision for one `KubeCostHPA`: shrink, grow or hold, with cost per request, budget freeze, Spot eligibility, and the optional predictive and OOM-protection results. This is the route the operator uses |
| `POST /v1/cluster/binpack` | Bin-packing computation on a topology you send. Analysis only; nothing is drained |
| `GET`/`POST /v1/finops/guardrail` | Budget burn-rate check on the figures you send |
| `GET /v1/audit/decisions`, `GET /v1/telemetry/events` | Decisions and events the engine recorded for the authenticated tenant |
| `GET`/`POST /v1/policies`, `GET /v1/commands/pending`, `POST /v1/telemetry/ingest`, `POST /v1/agents/heartbeat` | Control-plane state for a multi-tenant deployment. Experimental; not used by the chart |
| `POST /v1/auth/keys`, `POST /v1/auth/keys/revoke`, `GET /v1/auth/keys` | Manage tenant API keys. Listing requires an authorized engine or admin token, or a tenant API key with `keys:manage` |
| `GET /v1/auth/credential-expiry` | Expiry of the credential used for the request |
| `GET /metrics` | Prometheus metrics of the engine |

### Credential expiry

Call `GET /v1/auth/credential-expiry` with the credential in `Authorization: Bearer …` or `X-API-Key`. User-bound and machine API keys can check their expiry with any scope. The response uses `Cache-Control: no-store` and contains only these fields.

| Field | Meaning |
|---|---|
| `credential_type` | `api_key`, `engine_token`, or `admin_token` |
| `expires_at` | Unix timestamp in UTC seconds, or `null` for engine and admin tokens configured in the environment |
| `remaining_seconds` | Seconds to expiry on the server clock, bounded at zero, or `null` for environment tokens |

Missing, invalid, expired, and revoked credentials return `401`. A conflicting tenant returns `403`. Optional `client_id`, `tenant_id`, and `X-Tenant-ID` values are checked by the normal tenant gate.

### Routes that answer from sample data

These compute from built-in example inputs, static reference tables, or figures in the query string. They never read a
connected cluster or a provider bill. Every `GET` answer carries the header `X-KubeCost-Data-Source: sample` and a
`data_source` field saying so.

`/v1/ui/cast-dashboard`, `/v1/pricing/lookup`, `/v1/billing/invoice`, `/v1/gpu/optimize`, `/v1/greenops/report`,
`/v1/gameday/simulate`, `/v1/cluster/topology`, `/v1/cast/*`.

* `/v1/pricing/lookup` reads a static table of list prices in the source code. It is not a provider price API and is not kept
  current. The table still has AWS rows from earlier work; AWS is not a supported integration.
* `/v1/billing/invoice` returns a **pricing preview** shaped like an invoice, from a savings figure the caller supplies. Its
  answer says `simulation: true` and `charge_created: false`. Nothing is charged.
* `POST /v1/billing/stripe-webhook` verifies a Stripe signature and records the event. No Stripe account is connected and no
  product flow depends on it ([COMMERCIAL.md](COMMERCIAL.md)).

## Metrics

Operator metrics (`:8080/metrics`): `kubecost_autoscaler_*`, `kubecost_hpa_*`, `kubecost_recommendation_*`,
`kubecost_spot_drains_total`. Meaning and alerts: [OBSERVABILITY.md](OBSERVABILITY.md).

## Stability

| Interface | Stability |
|---|---|
| CRDs `v1alpha1` | Alpha: fields may change between minor versions, with a migration note ([UPGRADE.md](UPGRADE.md)) |
| `rightsize` flags and JSON output | Additive changes only inside a minor version |
| Recommendation record (JSON) | Additive: old records keep parsing (tested) |
| Engine `POST /v1/evaluate` | Stable inside a minor version; operator and engine are released together |
| Engine sample routes | No stability promise |
