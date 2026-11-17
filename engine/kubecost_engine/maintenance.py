"""Tenant-scoped telemetry retention; billing, audit and command evidence is retained."""
import time
import uuid
import os
import logging
import threading

logger = logging.getLogger(__name__)


class RetentionWorker:
    """Server-owned worker; scans persisted tenants even when ingestion is idle."""

    def __init__(self, cp, days, interval_seconds=3600):
        if type(days) is not int or not 1 <= days <= 3650:
            raise ValueError("retention days must be in [1,3650]")
        if type(interval_seconds) is not int or not 1 <= interval_seconds <= 86400:
            raise ValueError("retention interval must be in [1,86400]")
        self.cp, self.days, self.interval = cp, days, interval_seconds
        self._stop = threading.Event()
        self._thread = threading.Thread(target=self._run, name="telemetry-retention", daemon=True)

    def start(self):
        self._thread.start()

    def stop(self):
        self._stop.set()
        if self._thread.ident is not None:
            self._thread.join()

    def run_once(self):
        results = []
        for tenant in self.cp._store.telemetry_tenants():
            if self._stop.is_set():
                break
            try:
                days = self.days
                if os.environ.get("REQUIRE_LICENSE") == "true":
                    from .enterprise import EnterpriseService
                    days = min(days, EnterpriseService(self.cp).active_plan(tenant)["retention_days"])
                results.append(retain(self.cp, tenant, days, "system:scheduled-retention", dry_run=False))
            except Exception:
                # Isolate tenant failures; never silently interpret an invalid license as permission to delete.
                logger.exception("Scheduled retention failed for tenant %s", tenant)
        return results

    def _run(self):
        while not self._stop.is_set():
            try:
                self.run_once()
            except Exception:
                logger.exception("Scheduled retention scan failed")
            self._stop.wait(self.interval)


def configured_worker(cp):
    configured = os.environ.get("TELEMETRY_RETENTION_DAYS", "")
    if not configured:
        return None
    return RetentionWorker(cp, int(configured), int(os.environ.get("TELEMETRY_RETENTION_INTERVAL_SECONDS", "3600")))


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
