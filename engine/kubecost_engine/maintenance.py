"""Tenant-scoped telemetry retention; billing, audit and command evidence is retained."""
import time
import uuid
import os


def sweep_if_due(cp, tenant):
    """Opt-in, ingestion-driven retention; no background worker or hidden default."""
    configured = os.environ.get("TELEMETRY_RETENTION_DAYS", "")
    if not configured:
        return
    days = int(configured)
    if not 1 <= days <= 3650:
        raise ValueError("TELEMETRY_RETENTION_DAYS must be in [1,3650]")
    now = time.time()
    with cp._lock:
        previous = getattr(cp, "_last_retention_sweep", {}).get(tenant, 0)
        if now - previous < 3600:
            return
        if os.environ.get("REQUIRE_LICENSE") == "true":
            from .enterprise import EnterpriseService
            days = min(days, EnterpriseService(cp).active_plan(tenant)["retention_days"])
        retain(cp, tenant, days, "system:retention", dry_run=False, now=now)
        if not hasattr(cp, "_last_retention_sweep"):
            cp._last_retention_sweep = {}
        cp._last_retention_sweep[tenant] = now


def retain(cp, tenant, days, principal, dry_run=True, now=None):
    if type(days) is not int or not 1 <= days <= 3650:
        raise ValueError("days must be an integer in [1,3650]")
    if type(dry_run) is not bool:
        raise ValueError("dry_run must be a boolean")
    now = time.time() if now is None else now
    cutoff = int(now - days * 86400)
    with cp._lock:
        result = cp._store.prune_telemetry(tenant, cutoff, dry_run)
        if not dry_run:
            cp._telemetry[tenant] = [r for r in cp._telemetry.get(tenant, []) if r.timestamp >= cutoff]
            # Keep event ownership/dedup maps: retention must not rearm old events.
            record = {"id": uuid.uuid4().hex, "created_at": now, "principal": principal, "days": days, **result}
            cp._store.put_object(tenant, "maintenance", record["id"], record)
    return {**result, "days": days, "dry_run": dry_run}
