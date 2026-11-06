"""Nexus Telemetry Bus Producer for KubeCost Auto-HPA.

Conforms strictly to the Nexus Security & AI Lab telemetry contract
(2_Areas/Empresa/_Contrato_Telemetria.md):
- Destination: Redis Stream 'nexus:telemetry'
- Event Type: 'finops.scaling_decision'
- Source: 'kubecost_auto_hpa'
- Primary Key: (timestamp, event_id) with UUIDv7 idempotency
"""
from __future__ import annotations

import json
import logging
import os
from datetime import datetime, timezone
from typing import Any
import uuid

try:
    from uuid6 import uuid7
except ImportError:
    # Graceful fallback to RFC4122 uuid4 if uuid6 is missing
    uuid7 = uuid.uuid4  # type: ignore

try:
    import redis
except ImportError:
    redis = None  # type: ignore

logger = logging.getLogger("kubecost_engine.telemetry")

DEFAULT_STREAM = "nexus:telemetry"
DEFAULT_SOURCE = "kubecost_auto_hpa"
EVENT_TYPE_FINOPS = "finops.scaling_decision"
DEFAULT_TENANT_ID = "00000000-0000-0000-0000-000000000000"


class NexusTelemetryClient:
    """Publishes scaling decisions and financial telemetry to the Nexus stream."""

    def __init__(
        self,
        redis_url: str | None = None,
        stream: str = DEFAULT_STREAM,
        source: str = DEFAULT_SOURCE,
        default_tenant_id: str = DEFAULT_TENANT_ID,
    ) -> None:
        self.stream = os.environ.get("NEXUS_TELEMETRY_STREAM", stream)
        self.source = source
        self.default_tenant_id = os.environ.get("NEXUS_TENANT_ID", default_tenant_id)
        self._url = redis_url or os.environ.get("NEXUS_TELEMETRY_REDIS_URL") or os.environ.get("REDIS_URL")
        self._client: Any = None
        self._init_client()

    def _init_client(self) -> None:
        if not self._url or redis is None:
            return
        try:
            self._client = redis.Redis.from_url(
                self._url,
                decode_responses=True,
                socket_timeout=2.0,
                socket_connect_timeout=2.0,
            )
        except Exception as e:
            logger.warning("Failed to initialize Redis telemetry client: %s", e)
            self._client = None

    def publish_event(
        self,
        type_: str,
        severity: str,
        payload: dict[str, Any],
        tenant_id: str | None = None,
        event_id: str | None = None,
        ts: str | None = None,
    ) -> str | None:
        """Publishes an event conforming to the Nexus telemetry contract.

        Fail-safe: never raises exceptions or interrupts workload evaluation.
        """
        if self._client is None and self._url and redis is not None:
            self._init_client()

        if self._client is None:
            return None

        clean_tenant = tenant_id or self.default_tenant_id
        clean_event_id = event_id or str(uuid7())
        clean_ts = ts or datetime.now(timezone.utc).isoformat()

        fields = {
            "tenant_id": clean_tenant,
            "event_id": clean_event_id,
            "timestamp": clean_ts,
            "source": self.source,
            "type": type_,
            "severity": severity,
            "payload": json.dumps(payload, separators=(",", ":")),
        }

        try:
            return self._client.xadd(self.stream, fields, maxlen=2_000_000, approximate=True)
        except Exception as err:
            logger.warning("Telemetry publish dropped (fail-open): %s", err)
            return None

    def publish_scaling_decision(
        self,
        decision: dict[str, Any],
        request_context: dict[str, Any] | None = None,
        tenant_id: str | None = None,
    ) -> str | None:
        """Constructs a finops.scaling_decision event from an engine decision."""
        ctx = request_context or {}
        workload = ctx.get("workload") or ctx.get("name") or "unknown"
        namespace = ctx.get("namespace") or "default"

        # Determine severity based on OOM risk and cost pressure
        severity = "info"
        oom_dec = decision.get("oom_protection")
        if oom_dec and oom_dec.get("is_at_risk"):
            risk = oom_dec.get("risk_level", "").lower()
            if risk == "critical":
                severity = "critical"
            elif risk == "high":
                severity = "high"
            else:
                severity = "medium"
        elif decision.get("budget_frozen"):
            severity = "medium"

        replicas_dec = decision.get("replicas") or {}
        predictive_dec = decision.get("predictive") or {}
        memory_dec = decision.get("memory") or {}

        payload: dict[str, Any] = {
            "workload": workload,
            "namespace": namespace,
            "action": decision.get("action", "hold"),
            "cpu_millicores": decision.get("cpu_millicores"),
            "unit_cost": decision.get("cost_per_request"),
            "unit_cost_after": decision.get("cost_per_request_after"),
            "budget_frozen": decision.get("budget_frozen", False),
            "spot_eligible": decision.get("spot_eligible"),
            "reason": decision.get("reason", ""),
        }

        if replicas_dec:
            payload["replicas_target"] = replicas_dec.get("target")
            payload["replicas_reason"] = replicas_dec.get("reason")

        if predictive_dec:
            payload["predictive_prewarming"] = predictive_dec.get("is_prewarming", False)
            payload["recommended_replicas"] = predictive_dec.get("recommended_replicas")
            payload["confidence_score"] = predictive_dec.get("confidence_score")

        if memory_dec:
            payload["memory_target_mib"] = memory_dec.get("mib")
            payload["memory_action"] = memory_dec.get("action")

        if oom_dec:
            payload["oom_risk"] = oom_dec.get("is_at_risk", False)
            payload["oom_action"] = oom_dec.get("action", "hold")
            payload["oom_recommended_limit_mib"] = oom_dec.get("recommended_limit_mib")

        return self.publish_event(
            type_=EVENT_TYPE_FINOPS,
            severity=severity,
            payload=payload,
            tenant_id=tenant_id or ctx.get("tenant_id"),
        )


_global_telemetry_client: NexusTelemetryClient | None = None


def get_telemetry_client() -> NexusTelemetryClient:
    """Returns the process singleton telemetry client."""
    global _global_telemetry_client
    if _global_telemetry_client is None:
        _global_telemetry_client = NexusTelemetryClient()
    return _global_telemetry_client
