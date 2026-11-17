# Local completion V3 — 2026-11-17

Branch `enterprise-local-completion-v3` starts at merged PR #58 (`050ff0d`).

## Scheduled retention

Setting `TELEMETRY_RETENTION_DAYS` now starts a server-owned background worker.
It scans tenants from persisted telemetry immediately, then every
`TELEMETRY_RETENTION_INTERVAL_SECONDS` (default 3600, range 1..86400).
It works without further ingestion and stops when the HTTP server closes.
Configuration is validated before binding the server. Retention remains opt-in.

Each tenant is processed independently. Invalid/expired required licenses cause
that tenant to be skipped and logged; failures do not grant deletion permission.
Valid plans cap the configured retention period. Existing preview, deduplication
tombstones and maintenance audit entries remain in place. This supersedes the
ingestion-only limitation in MAINTENANCE_IMPLEMENTATION.md.

Helm exposes `engine.retention.enabled`, `days` and `intervalSeconds`, with schema
bounds. Stop the worker before closing its store. The current single-active
control-plane restriction still applies. Large deletion batches and tombstone
archival remain future work; this does not implement HA.

## Payment boundary

The local subscription workflow accepts an injectable provider for contract and
failure testing. It refuses live transactions, declines, currency/amount/tenant
mismatches and preserves subscription state on transport failure. Successful
requests record a hash: a repeated idempotency key with changed input is rejected
before calling the provider. Existing legacy events lacking that hash fail
closed on replay. Provider simulation does not exercise an external gateway.
Any future gateway must implement provider-side idempotency for ambiguous
timeouts; this module does not claim distributed exactly-once charging.

## API contract

OpenAPI documents retention bounds and the dry-run default. Recommendation and
prediction requests describe either submitted samples or retained container
telemetry. The legacy API still needs a complete typed contract.

## Tests and remaining work

Tests exercise idle-tenant cleanup through a real HTTP server lifecycle, worker
shutdown, tenant-error isolation, payment decline/mismatch/timeout/replay and the
existing PostgreSQL migration/concurrency suite. Evidence is under
`docs/reports/evidence/enterprise-v3/`.

This increment does not complete consolidation execution, multi-replica runtime,
external notifications, organization lifecycle or the full upgrade/failure matrix.
No cloud resources or external payments were created.

Validation: integrated suite 163 passed without skips, 86.22% Python coverage.
After adding the transient-failure regression, all 9 maintenance tests passed.
Helm lint/render and Ruff passed. No Go code changed in this increment.
