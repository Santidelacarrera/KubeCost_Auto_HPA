"""Multi-Tenant SaaS Control Plane Service (Motor E — Section 13).

Provides:
- Backward-compatible v1 telemetry ingestion and multi-level hierarchical schema
  (Cluster -> Node -> Workload -> Pod -> Container).
- Strict tenant isolation (`_verify_tenant_access` & `client_id` scoping across all queries,
  clusters, commands, recommendations, and mutations).
- Cross-tenant resource IDOR protection (`_cluster_owners`, `_command_owners`, `_event_owners`).
- Event idempotency & deduplication via `event_id`.
- Agent registration, heartbeats, and policy distribution with versioning.
- PRE_WARM command dispatch, anti-replay, expiration checking, and ACK tracking.
- Decision audit log and FinOps net savings reporting per tenant & cluster.
"""

from __future__ import annotations

from dataclasses import asdict, dataclass, field
import hashlib
import math
import threading
import time
from typing import Any, Dict, List, Optional
import uuid

from .postgres import create_store
from .security import AuthorizationError, InputValidationError, validate_identifier

_SHARED_DEFAULT_CLUSTER_NAMES = frozenset(
    {
        "eks-prod-1",
        "eks-prod-us-east-1",
        "prod-us-east-1",
        "eks-staging-us-east-1",
        "eks-1",
    }
)


@dataclass
class AgentRegistration:
    client_id: str
    cluster_id: str
    agent_id: str
    agent_version: str
    cluster_version: str
    region: str = "us-east-1"
    cloud_provider: str = "aws"
    registered_at: float = field(default_factory=time.time)
    last_heartbeat_at: float = field(default_factory=time.time)
    execution_mode: str = "OBSERVE_ONLY"
    status: str = "ONLINE"


@dataclass
class PreWarmCommand:
    command_id: str
    client_id: str
    cluster_id: str
    workload: str
    action: str  # "PRE_WARM"
    requested_resources: Dict[str, str]
    recommended_replicas: int
    execute_at: int
    expires_at: int
    confidence: float
    reason: str
    policy_version: str
    status: str = "PENDING"  # PENDING | ACKNOWLEDGED | EXPIRED | REJECTED
    ack_status: Optional[str] = None
    ack_reason: Optional[str] = None
    ack_timestamp: Optional[float] = None


@dataclass
class NormalizedTelemetryRecord:
    schema_version: str
    event_id: str
    client_id: str
    cluster_id: str
    timestamp: int
    # Multi-level hierarchy
    agent_version: str
    cluster_version: str
    node_name: str
    node_instance_type: str
    spot_instance_risk: float
    namespace: str
    workload_name: str
    workload_kind: str
    pod_name: str
    container_name: str
    # Metrics
    rps: float
    successful_requests: float
    error_rate: float
    p50_latency_ms: float
    p95_latency_ms: float
    p99_latency_ms: float
    cpu_usage_cores: float
    cpu_requests: str
    cpu_limits: str
    mem_usage_bytes: int
    memory_requests: str
    memory_limits: str
    current_replicas: int


class SaaSControlPlane:
    """Thread-safe multi-tenant SaaS Control Plane state and business logic."""

    def __init__(self, db_path: Optional[str] = None) -> None:
        self._lock = threading.RLock()
        self._store = create_store(db_path=db_path)
        if hasattr(self._store, "acquire_control_plane"):
            self._store.acquire_control_plane()
        # Keyed by client_id for strict tenant isolation
        self._agents: Dict[str, Dict[str, AgentRegistration]] = {}
        self._telemetry: Dict[str, List[NormalizedTelemetryRecord]] = {}
        self._seen_event_ids: Dict[str, set[str]] = {}
        self._policies: Dict[str, Dict[str, Dict[str, Any]]] = {}
        self._commands: Dict[str, Dict[str, PreWarmCommand]] = {}
        self._executed_command_ids: Dict[str, set[str]] = {}
        self._audit_logs: Dict[str, List[Dict[str, Any]]] = {}
        self._tenant_tokens: Dict[str, str] = {}
        self._executed_ui_recs_by_tenant: Dict[str, set[str]] = {}
        # Cross-tenant resource ownership maps to block IDOR by resource ID
        self._cluster_owners: Dict[str, str] = {}
        self._command_owners: Dict[str, str] = {}
        self._event_owners: Dict[str, str] = {}
        self._decision_owners: Dict[str, str] = {}
        recovery = self._store.recovery_records()
        for tenant, state in recovery["runtime"]:
            self._agents[tenant] = {k: AgentRegistration(**v) for k, v in state["agents"].items()}
            self._policies[tenant] = state["policies"]
            for cluster in self._agents[tenant]:
                self._bind_or_verify_cluster_owner(tenant, cluster, register=True)
        for raw in recovery["telemetry"]:
            # Older raw payloads remain in the database; only normalized records can be restored.
            if "cpu_usage_cores" not in raw:
                continue
            record = NormalizedTelemetryRecord(**raw)
            self._telemetry.setdefault(record.client_id, []).append(record)
            self._seen_event_ids.setdefault(record.client_id, set()).add(record.event_id)
            self._event_owners[record.event_id] = record.client_id
            self._bind_or_verify_cluster_owner(record.client_id, record.cluster_id, register=True)
        for raw in recovery["commands"]:
            command = PreWarmCommand(**raw)
            self._commands.setdefault(command.client_id, {})[command.command_id] = command
            self._command_owners[command.command_id] = command.client_id
            if command.status != "PENDING":
                self._executed_command_ids.setdefault(command.client_id, set()).add(command.command_id)
        for raw in recovery["audit"]:
            tenant = raw["client_id"]
            self._audit_logs.setdefault(tenant, []).append(raw)
            self._decision_owners[raw["decision_id"]] = tenant

    def _persist_runtime(self, tenant):
        state = {"agents": {k: asdict(v) for k, v in self._agents.get(tenant, {}).items()}, "policies": self._policies.get(tenant, {})}
        previous = self._store.list_objects(tenant, "runtime")
        self._store.put_object(tenant, "runtime", "state", state, previous[0]["version"] if previous else 0)


    def register_tenant_token(self, client_id: str, token: str) -> None:
        with self._lock:
            self._tenant_tokens[client_id] = token

    def _verify_tenant_access(
        self, client_id: str, provided_tenant_header: Optional[str] = None
    ) -> str:
        """Centralized multi-tenant boundary enforcement: authenticated tenant must match client_id."""
        if not client_id or not isinstance(client_id, str) or not client_id.strip():
            raise InputValidationError("client_id is required for multi-tenant isolation")
        clean_client = validate_identifier(client_id.strip(), "client_id")
        if provided_tenant_header is not None and str(provided_tenant_header).strip():
            clean_hdr = validate_identifier(str(provided_tenant_header).strip(), "X-Tenant-ID")
            if clean_hdr != clean_client:
                raise AuthorizationError(
                    "Tenant isolation violation: authenticated tenant is not authorized for the requested tenant resource"
                )
        return clean_client

    def validate_tenant_access(
        self, client_id: str, provided_tenant_header: Optional[str] = None
    ) -> None:
        """Backward-compatible alias for `_verify_tenant_access`."""
        self._verify_tenant_access(client_id, provided_tenant_header)

    def _bind_or_verify_cluster_owner(self, client_id: str, cluster_id: str, register: bool = False) -> None:
        """Prevent cross-tenant access to a cluster_id registered by another tenant."""
        if not cluster_id:
            return
        clean_cluster = validate_identifier(cluster_id.strip(), "cluster_id")
        if clean_cluster in _SHARED_DEFAULT_CLUSTER_NAMES:
            return
        owner = self._cluster_owners.get(clean_cluster)
        if owner is not None and owner != client_id:
            raise AuthorizationError(
                "Tenant isolation violation: cluster resource belongs to another organization"
            )
        if register and owner is None:
            self._cluster_owners[clean_cluster] = client_id

    def verify_cluster_access(
        self, client_id: str, cluster_id: str, tenant_header: Optional[str] = None
    ) -> None:
        """Public check that `client_id` matches `tenant_header` and is allowed to access `cluster_id`."""
        clean_client = self._verify_tenant_access(client_id, tenant_header)
        with self._lock:
            self._bind_or_verify_cluster_owner(clean_client, cluster_id, register=False)

    def register_agent(self, payload: Dict[str, Any], tenant_header: Optional[str] = None) -> Dict[str, Any]:
        raw_client = str(payload.get("client_id") or payload.get("tenant_id") or "").strip()
        client_id = self._verify_tenant_access(raw_client, tenant_header)
        cluster_id = str(payload.get("cluster_id", "")).strip()
        if not cluster_id:
            raise InputValidationError("cluster_id is required")
        cluster_id = validate_identifier(cluster_id, "cluster_id")

        agent_id = validate_identifier(
            str(payload.get("agent_id") or f"agent-{cluster_id}-{uuid.uuid4().hex[:8]}"),
            "agent_id",
        )
        now = time.time()
        reg = AgentRegistration(
            client_id=client_id,
            cluster_id=cluster_id,
            agent_id=agent_id,
            agent_version=str(payload.get("agent_version", "v2.0.0"))[:64],
            cluster_version=str(payload.get("cluster_version", "v1.33.0"))[:64],
            region=validate_identifier(str(payload.get("region", "us-east-1")), "region"),
            cloud_provider=validate_identifier(str(payload.get("cloud_provider", "aws")), "cloud_provider"),
            registered_at=now,
            last_heartbeat_at=now,
            execution_mode=validate_identifier(str(payload.get("execution_mode", "OBSERVE_ONLY")), "execution_mode"),
            status="ONLINE",
        )
        with self._lock:
            from .enterprise import EnterpriseService
            EnterpriseService(self).enforce_license(client_id, new_cluster=cluster_id not in self._agents.get(client_id, {}))
            self._bind_or_verify_cluster_owner(client_id, cluster_id, register=True)
            self._agents.setdefault(client_id, {})[cluster_id] = reg
            self._policies.setdefault(client_id, {}).setdefault(
                cluster_id,
                {
                    "policy_version": "2026-11-01.v1",
                    "client_id": client_id,
                    "cluster_id": cluster_id,
                    "execution_mode": reg.execution_mode,
                    "slo_p99_ms": 200.0,
                    "max_increase_percent": 100.0,
                    "max_decrease_percent": 20.0,
                    "min_replicas": 2,
                    "max_replicas": 50,
                    "enable_prewarm": True,
                    "enable_spot_drain": False,
                    "pricing_model": "VALUE_SHARED",
                    "value_shared_fee_pct": 0.22,
                    "fixed_price_per_node_month_usd": 25.0,
                },
            )
            self._persist_runtime(client_id)
        return asdict(reg)

    def heartbeat(self, payload: Dict[str, Any], tenant_header: Optional[str] = None) -> Dict[str, Any]:
        raw_client = str(payload.get("client_id") or payload.get("tenant_id") or "").strip()
        client_id = self._verify_tenant_access(raw_client, tenant_header)
        cluster_id = str(payload.get("cluster_id", "")).strip()
        if not cluster_id:
            raise InputValidationError("cluster_id is required")
        cluster_id = validate_identifier(cluster_id, "cluster_id")

        now = time.time()
        with self._lock:
            self._bind_or_verify_cluster_owner(client_id, cluster_id, register=True)
            reg = self._agents.get(client_id, {}).get(cluster_id)
            if not reg:
                from .enterprise import EnterpriseService
                EnterpriseService(self).enforce_license(client_id, new_cluster=True)
                reg = AgentRegistration(
                    client_id=client_id,
                    cluster_id=cluster_id,
                    agent_id=validate_identifier(
                        str(payload.get("agent_id", f"agent-{cluster_id}")), "agent_id"
                    ),
                    agent_version=str(payload.get("agent_version", "v2.0.0"))[:64],
                    cluster_version=str(payload.get("cluster_version", "v1.33.0"))[:64],
                    registered_at=now,
                    last_heartbeat_at=now,
                )
                self._agents.setdefault(client_id, {})[cluster_id] = reg
            else:
                reg.last_heartbeat_at = now
                reg.status = validate_identifier(str(payload.get("status", "ONLINE")), "status")
                if "execution_mode" in payload:
                    reg.execution_mode = validate_identifier(
                        str(payload["execution_mode"]), "execution_mode"
                    )

            self._persist_runtime(client_id)
            pending_cmds = [
                asdict(c)
                for c in self._commands.get(client_id, {}).values()
                if c.cluster_id == cluster_id and c.status == "PENDING" and c.expires_at > int(now)
            ]

        return {
            "status": "ok",
            "client_id": client_id,
            "cluster_id": cluster_id,
            "server_timestamp": int(now),
            "pending_commands_count": len(pending_cmds),
        }

    def ingest_telemetry(self, payload: Dict[str, Any], tenant_header: Optional[str] = None) -> Dict[str, Any]:
        """Ingests both legacy v1 payloads (Section 13.1) and extended multi-level schemas (Section 13.2)."""
        raw_client = str(payload.get("client_id") or payload.get("tenant_id") or "").strip()
        client_id = self._verify_tenant_access(raw_client, tenant_header)
        cluster_id = str(payload.get("cluster_id", "")).strip()
        if not cluster_id:
            raise InputValidationError("cluster_id is required")
        cluster_id = validate_identifier(cluster_id, "cluster_id")

        metrics = payload.get("metrics")
        if not isinstance(metrics, dict):
            raise InputValidationError("metrics object is required")

        timestamp = int(payload.get("timestamp") or int(time.time()))
        rps = float(metrics.get("rps", 0.0))
        p99 = float(metrics.get("p99_latency_ms", 0.0))
        cpu_cores = float(metrics.get("cpu_usage_cores", 0.0))
        mem_bytes = int(metrics.get("mem_usage_bytes", 0))
        replicas = int(metrics.get("current_replicas", 1))

        if not (math.isfinite(rps) and math.isfinite(p99) and math.isfinite(cpu_cores)):
            raise InputValidationError("telemetry metrics must be finite numbers")
        if rps < 0 or p99 < 0 or cpu_cores < 0 or mem_bytes < 0 or replicas < 0:
            raise InputValidationError("telemetry metrics must be non-negative")

        raw_event_id = payload.get("event_id")
        if not raw_event_id:
            workload = str(payload.get("workload_name") or metrics.get("workload_name") or "default/workload")
            digest_src = f"{client_id}:{cluster_id}:{workload}:{timestamp}:{rps}:{p99}:{cpu_cores}:{mem_bytes}"
            event_id = "evt-" + hashlib.sha256(digest_src.encode("utf-8")).hexdigest()[:16]
        else:
            event_id = validate_identifier(str(raw_event_id), "event_id")

        with self._lock:
            self._bind_or_verify_cluster_owner(client_id, cluster_id, register=True)
            evt_owner = self._event_owners.get(event_id)
            if evt_owner is not None and evt_owner != client_id:
                raise AuthorizationError(
                    "Tenant isolation violation: telemetry event_id belongs to another organization"
                )
            self._event_owners[event_id] = client_id

            seen = self._seen_event_ids.setdefault(client_id, set())
            error_rate = float(payload.get("error_rate", metrics.get("error_rate", 0.0)))
            if not math.isfinite(error_rate):
                raise InputValidationError("error_rate must be finite")
            error_rate = max(0.0, min(1.0, error_rate))
            default_succ = rps * (1.0 - error_rate)
            successful_requests = float(
                payload.get("successful_requests", metrics.get("successful_requests", default_succ))
            )

            rec = NormalizedTelemetryRecord(
                schema_version=str(payload.get("schema_version", "v1.2"))[:32],
                event_id=event_id,
                client_id=client_id,
                cluster_id=cluster_id,
                timestamp=timestamp,
                agent_version=str(payload.get("agent_version", "v2.0.0"))[:64],
                cluster_version=str(payload.get("cluster_version", "v1.33.0"))[:64],
                node_name=str(payload.get("node_name", metrics.get("node_name", "node-unknown")))[:128],
                node_instance_type=str(
                    payload.get("node_instance_type", metrics.get("node_instance_type", "m6i.xlarge"))
                )[:64],
                spot_instance_risk=float(
                    payload.get("spot_instance_risk", metrics.get("spot_instance_risk", 0.0))
                ),
                namespace=str(payload.get("namespace", metrics.get("namespace", "default")))[:128],
                workload_name=str(
                    payload.get("workload_name", metrics.get("workload_name", "default-workload"))
                )[:128],
                workload_kind=str(
                    payload.get("workload_kind", metrics.get("workload_kind", "Deployment"))
                )[:64],
                pod_name=str(payload.get("pod_name", metrics.get("pod_name", "")))[:128],
                container_name=str(payload.get("container_name", metrics.get("container_name", "app")))[:128],
                rps=rps,
                successful_requests=successful_requests,
                error_rate=error_rate,
                p50_latency_ms=float(payload.get("p50_latency_ms", metrics.get("p50_latency_ms", p99 * 0.35))),
                p95_latency_ms=float(payload.get("p95_latency_ms", metrics.get("p95_latency_ms", p99 * 0.75))),
                p99_latency_ms=p99,
                cpu_usage_cores=cpu_cores,
                cpu_requests=str(payload.get("cpu_requests", metrics.get("cpu_requests", f"{int(cpu_cores*1000)}m")))[:32],
                cpu_limits=str(payload.get("cpu_limits", metrics.get("cpu_limits", f"{int(cpu_cores*1500)}m")))[:32],
                mem_usage_bytes=mem_bytes,
                memory_requests=str(
                    payload.get("memory_requests", metrics.get("memory_requests", f"{mem_bytes // (1024*1024)}Mi"))
                )[:32],
                memory_limits=str(
                    payload.get("memory_limits", metrics.get("memory_limits", f"{int(mem_bytes * 1.25) // (1024*1024)}Mi"))
                )[:32],
                current_replicas=replicas,
            )
            for value in (rec.successful_requests, rec.spot_instance_risk, rec.p50_latency_ms, rec.p95_latency_ms):
                if not math.isfinite(value) or value < 0:
                    raise InputValidationError("telemetry metrics must be finite and non-negative")
            from .enterprise import EnterpriseService
            EnterpriseService(self).enforce_inventory(client_id, rec)
            if event_id in seen or not self._store.save_telemetry_event(client_id, event_id, cluster_id, timestamp, asdict(rec)):
                seen.add(event_id)
                return {"status": "duplicate_ignored", "event_id": event_id, "client_id": client_id, "cluster_id": cluster_id, "deduplicated": True}
            seen.add(event_id)
            bucket = self._telemetry.setdefault(client_id, [])
            bucket.append(rec)
            if len(bucket) > 5000:
                del bucket[: len(bucket) - 5000]

        from .maintenance import sweep_if_due
        sweep_if_due(self, client_id)

        return {
            "status": "ingested",
            "event_id": event_id,
            "client_id": client_id,
            "cluster_id": cluster_id,
            "schema_version": rec.schema_version,
            "deduplicated": False,
            "normalized": asdict(rec),
        }

    def get_telemetry_events(
        self,
        client_id: str,
        cluster_id: Optional[str] = None,
        event_id: Optional[str] = None,
        tenant_header: Optional[str] = None,
    ) -> List[Dict[str, Any]]:
        """Query telemetry records strictly isolated to the authenticated tenant."""
        clean_client = self._verify_tenant_access(client_id, tenant_header)
        with self._lock:
            if cluster_id:
                self._bind_or_verify_cluster_owner(clean_client, cluster_id, register=False)
            if event_id:
                clean_evt = validate_identifier(event_id, "event_id")
                evt_owner = self._event_owners.get(clean_evt)
                if evt_owner is not None and evt_owner != clean_client:
                    raise AuthorizationError(
                        "Tenant isolation violation: telemetry event belongs to another organization"
                    )
            records = [asdict(r) for r in self._telemetry.get(clean_client, [])]
        if cluster_id:
            records = [r for r in records if r.get("cluster_id") == cluster_id]
        if event_id:
            records = [r for r in records if r.get("event_id") == event_id]
        return records

    def get_policy(self, client_id: str, cluster_id: str, tenant_header: Optional[str] = None) -> Dict[str, Any]:
        clean_client = self._verify_tenant_access(client_id, tenant_header)
        clean_cluster = validate_identifier(cluster_id or "eks-prod-1", "cluster_id")
        with self._lock:
            self._bind_or_verify_cluster_owner(clean_client, clean_cluster, register=False)
            pol = self._policies.get(clean_client, {}).get(clean_cluster)
            if pol:
                return dict(pol)
            return {
                "policy_version": "2026-11-01.default",
                "client_id": clean_client,
                "cluster_id": clean_cluster,
                "execution_mode": "OBSERVE_ONLY",
                "slo_p99_ms": 200.0,
                "max_increase_percent": 100.0,
                "max_decrease_percent": 20.0,
                "min_replicas": 2,
                "max_replicas": 50,
                "enable_prewarm": True,
                "enable_spot_drain": False,
            }

    def update_policy(self, payload: Dict[str, Any], tenant_header: Optional[str] = None) -> Dict[str, Any]:
        """Update scaling policy for a tenant's cluster, enforcing strict tenant isolation."""
        raw_client = str(payload.get("client_id") or payload.get("tenant_id") or "").strip()
        clean_client = self._verify_tenant_access(raw_client, tenant_header)
        clean_cluster = validate_identifier(str(payload.get("cluster_id") or "eks-prod-1"), "cluster_id")
        slo = float(payload.get("slo_p99_ms", 200.0))
        if not math.isfinite(slo) or slo <= 0 or slo > 60000:
            raise InputValidationError("slo_p99_ms must be between 1 and 60000")
        min_rep = int(payload.get("min_replicas", 2))
        max_rep = int(payload.get("max_replicas", 50))
        if min_rep < 1 or max_rep < min_rep or max_rep > 10000:
            raise InputValidationError("invalid replica bounds in policy")
        for flag in ("enable_prewarm", "enable_spot_drain"):
            if flag in payload and type(payload[flag]) is not bool:
                raise InputValidationError(f"{flag} must be a boolean")
        if "execution_mode" in payload and payload["execution_mode"] not in ("OBSERVE_ONLY", "ASSISTED", "AUTOMATED", "Observe", "Canary", "Active"):
            raise InputValidationError("unsupported execution_mode")

        with self._lock:
            self._bind_or_verify_cluster_owner(clean_client, clean_cluster, register=True)
            pol = self._policies.setdefault(clean_client, {}).setdefault(
                clean_cluster,
                {
                    "policy_version": "2026-11-01.v2",
                    "client_id": clean_client,
                    "cluster_id": clean_cluster,
                },
            )
            pol.update(
                {
                    "policy_version": str(payload.get("policy_version", "2026-11-01.v2"))[:64],
                    "client_id": clean_client,
                    "cluster_id": clean_cluster,
                    "execution_mode": validate_identifier(
                        str(payload.get("execution_mode", pol.get("execution_mode", "OBSERVE_ONLY"))),
                        "execution_mode",
                    ),
                    "slo_p99_ms": slo,
                    "min_replicas": min_rep,
                    "max_replicas": max_rep,
                    "enable_prewarm": payload.get("enable_prewarm", pol.get("enable_prewarm", True)),
                    "enable_spot_drain": payload.get("enable_spot_drain", pol.get("enable_spot_drain", False)),
                }
            )
            self._persist_runtime(clean_client)
            return dict(pol)

    def issue_prewarm_command(
        self, payload: Dict[str, Any], tenant_header: Optional[str] = None
    ) -> Dict[str, Any]:
        """Creates and queues a Section 7.5 PRE_WARM command for the Go agent."""
        raw_client = str(payload.get("client_id") or payload.get("tenant_id") or "default-tenant").strip()
        client_id = self._verify_tenant_access(raw_client, tenant_header)
        cluster_id = str(payload.get("cluster_id", "")).strip()
        workload = str(payload.get("workload", "")).strip()
        if not cluster_id or not workload:
            raise InputValidationError("cluster_id and workload are required for PRE_WARM")
        cluster_id = validate_identifier(cluster_id, "cluster_id")
        workload = validate_identifier(workload, "workload", allow_slash=True)

        now = int(time.time())
        command_id = validate_identifier(
            str(payload.get("command_id") or f"cmd-prewarm-{uuid.uuid4().hex[:10]}"),
            "command_id",
        )
        execute_at = int(payload.get("execute_at") or now)
        expires_at = int(payload.get("expires_at") or (execute_at + 900))
        req_res = payload.get("requested_resources") or {"cpu": "1500m", "memory": "2Gi"}
        confidence = float(payload.get("confidence", 0.91))
        if not math.isfinite(confidence) or confidence < 0.0 or confidence > 1.0:
            raise InputValidationError("confidence must be between 0.0 and 1.0")

        cmd = PreWarmCommand(
            command_id=command_id,
            client_id=client_id,
            cluster_id=cluster_id,
            workload=workload,
            action="PRE_WARM",
            requested_resources={
                "cpu": str(req_res.get("cpu", "1500m"))[:32],
                "memory": str(req_res.get("memory", "2Gi"))[:32],
            },
            recommended_replicas=int(payload.get("recommended_replicas", 6)),
            execute_at=execute_at,
            expires_at=expires_at,
            confidence=confidence,
            reason=str(payload.get("reason", "Predicted seasonal traffic spike"))[:256],
            policy_version=str(payload.get("policy_version", "2026-11-01.v1"))[:64],
        )
        with self._lock:
            self._bind_or_verify_cluster_owner(client_id, cluster_id, register=True)
            existing_cmd_owner = self._command_owners.get(command_id)
            if existing_cmd_owner is not None and existing_cmd_owner != client_id:
                raise AuthorizationError(
                    "Tenant isolation violation: command_id belongs to another organization"
                )
            self._command_owners[command_id] = client_id
            existing = self._commands.get(client_id, {}).get(command_id)
            if existing is not None:
                if (existing.cluster_id, existing.workload, existing.requested_resources, existing.recommended_replicas) != (cmd.cluster_id, cmd.workload, cmd.requested_resources, cmd.recommended_replicas):
                    raise InputValidationError("command_id already exists with different content")
                return asdict(existing)
            self._commands.setdefault(client_id, {})[command_id] = cmd
            self._store.upsert_prewarm_command(
                client_id, command_id, cluster_id, cmd.status, cmd.expires_at, asdict(cmd)
            )
        return asdict(cmd)

    def get_pending_commands(
        self, client_id: str, cluster_id: str, now_ts: Optional[int] = None, tenant_header: Optional[str] = None
    ) -> List[Dict[str, Any]]:
        clean_client = self._verify_tenant_access(client_id, tenant_header)
        clean_cluster = validate_identifier(cluster_id or "eks-prod-1", "cluster_id")
        now = int(now_ts if now_ts is not None else time.time())
        out: List[Dict[str, Any]] = []
        with self._lock:
            self._bind_or_verify_cluster_owner(clean_client, clean_cluster, register=False)
            for cmd in self._commands.get(clean_client, {}).values():
                if cmd.cluster_id != clean_cluster:
                    continue
                if cmd.status == "PENDING" and cmd.expires_at <= now:
                    cmd.status = "EXPIRED"
                    self._store.upsert_prewarm_command(clean_client, cmd.command_id, cmd.cluster_id, cmd.status, cmd.expires_at, asdict(cmd))
                if cmd.status == "PENDING":
                    out.append(asdict(cmd))
        return out

    def acknowledge_command(
        self, payload: Dict[str, Any], tenant_header: Optional[str] = None
    ) -> Dict[str, Any]:
        """Processes Go Agent ACK for PRE_WARM commands with expiry, tenant ownership, and anti-replay protection."""
        raw_client = str(payload.get("client_id") or payload.get("tenant_id") or "").strip()
        client_id = self._verify_tenant_access(raw_client, tenant_header)
        command_id = str(payload.get("command_id", "")).strip()
        if not command_id:
            raise InputValidationError("command_id is required")
        command_id = validate_identifier(command_id, "command_id")

        now = int(payload.get("timestamp") or time.time())
        agent_status = validate_identifier(str(payload.get("status", "EXECUTED")).upper(), "status")
        agent_reason = str(payload.get("reason", "Executed by Go agent"))[:256]

        with self._lock:
            cmd_owner = self._command_owners.get(command_id)
            if cmd_owner is not None and cmd_owner != client_id:
                raise AuthorizationError(
                    "Tenant isolation violation: cannot acknowledge command owned by another organization"
                )

            executed_set = self._executed_command_ids.setdefault(client_id, set())
            if command_id in executed_set:
                return {
                    "command_id": command_id,
                    "client_id": client_id,
                    "ack_result": "DUPLICATE_IGNORED",
                    "reason": "Command was already acknowledged previously (anti-replay protection)",
                }

            cmd = self._commands.get(client_id, {}).get(command_id)
            if not cmd:
                raise KeyError("command_id not found for tenant")

            if cmd.expires_at <= now:
                cmd.status = "EXPIRED"
                cmd.ack_status = "REJECTED_EXPIRED"
                cmd.ack_reason = f"Command expired at {cmd.expires_at} (current={now})"
                cmd.ack_timestamp = float(now)
                self._store.upsert_prewarm_command(client_id, cmd.command_id, cmd.cluster_id, cmd.status, cmd.expires_at, asdict(cmd))
                return {
                    "command_id": command_id,
                    "client_id": client_id,
                    "ack_result": "REJECTED_EXPIRED",
                    "reason": cmd.ack_reason,
                }

            executed_set.add(command_id)
            cmd.status = "ACKNOWLEDGED" if agent_status in ("EXECUTED", "ACCEPTED") else "REJECTED"
            cmd.ack_status = agent_status
            cmd.ack_reason = agent_reason
            cmd.ack_timestamp = float(now)
            self._store.upsert_prewarm_command(client_id, cmd.command_id, cmd.cluster_id, cmd.status, cmd.expires_at, asdict(cmd))

            return {
                "command_id": command_id,
                "client_id": client_id,
                "cluster_id": cmd.cluster_id,
                "workload": cmd.workload,
                "ack_result": agent_status,
                "reason": agent_reason,
            }

    def record_audit_decision(
        self, payload: Dict[str, Any], tenant_header: Optional[str] = None
    ) -> Dict[str, Any]:
        raw_client = str(payload.get("client_id") or payload.get("tenant_id") or "default-tenant").strip()
        client_id = self._verify_tenant_access(raw_client, tenant_header)
        cluster_id = validate_identifier(str(payload.get("cluster_id", "eks-prod-1")), "cluster_id")
        decision_id = validate_identifier(
            str(payload.get("decision_id") or f"dec-{uuid.uuid4().hex[:10]}"),
            "decision_id",
        )
        econ_delta = float(payload.get("economic_delta_usd_hour", 0.0))
        if not math.isfinite(econ_delta):
            raise InputValidationError("economic_delta_usd_hour must be a finite number")

        entry = {
            "decision_id": decision_id,
            "client_id": client_id,
            "cluster_id": cluster_id,
            "workload": validate_identifier(
                str(payload.get("workload", "default/workload")), "workload", allow_slash=True
            ),
            "decision_type": validate_identifier(
                str(payload.get("decision_type", "VERTICAL_RESIZE")), "decision_type"
            ),
            "execution_mode": validate_identifier(
                str(payload.get("execution_mode", "OBSERVE_ONLY")), "execution_mode"
            ),
            "before_state": payload.get("before_state", {}),
            "after_state": payload.get("after_state", {}),
            "economic_delta_usd_hour": econ_delta,
            "reason": str(payload.get("reason", ""))[:256],
            "timestamp": int(payload.get("timestamp") or time.time()),
        }
        with self._lock:
            self._bind_or_verify_cluster_owner(client_id, cluster_id, register=True)
            dec_owner = self._decision_owners.get(decision_id)
            if dec_owner is not None and dec_owner != client_id:
                raise AuthorizationError(
                    "Tenant isolation violation: decision_id belongs to another organization"
                )
            self._decision_owners[decision_id] = client_id
            logs = self._audit_logs.setdefault(client_id, [])
            logs.append(entry)
            self._store.save_audit_decision(client_id, decision_id, cluster_id, entry["timestamp"], entry)
        return entry

    def get_audit_decisions(
        self,
        client_id: str,
        cluster_id: Optional[str] = None,
        tenant_header: Optional[str] = None,
        decision_id: Optional[str] = None,
    ) -> List[Dict[str, Any]]:
        clean_client = self._verify_tenant_access(client_id, tenant_header)
        with self._lock:
            if cluster_id:
                clean_cluster = validate_identifier(cluster_id, "cluster_id")
                self._bind_or_verify_cluster_owner(clean_client, clean_cluster, register=False)
            if decision_id:
                clean_dec = validate_identifier(decision_id, "decision_id")
                dec_owner = self._decision_owners.get(clean_dec)
                if dec_owner is not None and dec_owner != clean_client:
                    raise AuthorizationError(
                        "Tenant isolation violation: decision_id belongs to another organization"
                    )
            logs = list(self._audit_logs.get(clean_client, []))
        if cluster_id:
            logs = [x for x in logs if x.get("cluster_id") == cluster_id]
        if decision_id:
            logs = [x for x in logs if x.get("decision_id") == decision_id]
        return logs

    def get_cast_dashboard(
        self,
        client_id: str = "default-tenant",
        cluster_id: str = "eks-prod-us-east-1",
        tenant_header: Optional[str] = None,
    ) -> Dict[str, Any]:
        """Returns the Fase 10 Cast AI + Tremor UI payload strictly scoped to the authenticated tenant."""
        clean_client = self._verify_tenant_access(client_id, tenant_header)
        clean_cluster = validate_identifier(cluster_id or "eks-prod-us-east-1", "cluster_id")
        with self._lock:
            self._bind_or_verify_cluster_owner(clean_client, clean_cluster, register=False)
            records = [asdict(r) for r in self._telemetry.get(clean_client, []) if r.cluster_id == clean_cluster]
        latest = {}
        for row in sorted(records, key=lambda r: r["timestamp"]):
            latest[(row["namespace"], row["workload_name"], row["pod_name"], row["container_name"])] = row
        groups = {}
        for row in latest.values():
            group = groups.setdefault(row["namespace"], {"namespace": row["namespace"], "cpu_used_cores": 0, "memory_used_gib": 0})
            group["cpu_used_cores"] += row["cpu_usage_cores"]
            group["memory_used_gib"] += row["mem_usage_bytes"] / 1024**3
        recommendations = [r for r in self._store.list_objects(clean_client, "rightsizing") if r["cluster_id"] == clean_cluster]
        return {
            "client_id": clean_client, "cluster_id": clean_cluster,
            "data_source": "ingested_telemetry", "has_data": bool(records),
            "latest_observation": max((r["timestamp"] for r in records), default=None),
            "pain_number_cards": {"current_monthly_spend_usd": None, "projected_optimized_spend_usd": None, "net_monthly_savings_usd": None, "realized_1click_savings_usd": None},
            "namespace_bar_chart": list(groups.values()), "actionable_recommendations": recommendations,
        }

    def execute_ui_recommendation(
        self,
        payload_or_client_id: Dict[str, Any] | str,
        recommendation_type: Optional[str] = None,
        target: Optional[str] = None,
        tenant_header: Optional[str] = None,
    ) -> Dict[str, Any]:
        """Translates a 1-click UI recommendation into an actionable operator audit/command.

        Mandatory authorization check (`_verify_tenant_access`) runs BEFORE any state mutation.
        Supports both `execute_ui_recommendation(payload_dict, tenant_header=...)`
        and `execute_ui_recommendation(client_id, recommendation_type, target, tenant_header=...)`.
        """
        if isinstance(payload_or_client_id, dict):
            payload = payload_or_client_id
            raw_client = str(
                payload.get("client_id") or payload.get("tenant_id") or tenant_header or "default-tenant"
            ).strip()
            rec_id = str(payload.get("recommendation_id") or payload.get("recommendation_type") or "").strip()
            cluster_id = str(payload.get("cluster_id", "eks-prod-us-east-1")).strip()
            workload = str(payload.get("workload") or payload.get("target") or rec_id).strip()
            action_type = str(
                payload.get("action_type") or payload.get("recommendation_type") or "ONE_CLICK_OPTIMIZATION"
            ).strip()
        else:
            raw_client = str(payload_or_client_id or "").strip()
            rec_id = str(recommendation_type or "").strip()
            cluster_id = "eks-prod-us-east-1"
            workload = str(target or rec_id).strip()
            action_type = str(recommendation_type or "ONE_CLICK_OPTIMIZATION").strip()

        clean_client = self._verify_tenant_access(raw_client, tenant_header)
        if not rec_id:
            raise InputValidationError("recommendation_id is required")
        clean_rec_id = validate_identifier(rec_id, "recommendation_id")
        clean_cluster = validate_identifier(cluster_id, "cluster_id")
        clean_workload = validate_identifier(workload, "workload", allow_slash=True)
        clean_action = validate_identifier(action_type, "action_type")

        with self._lock:
            self._bind_or_verify_cluster_owner(clean_client, clean_cluster, register=False)
        raise InputValidationError("Direct UI execution is unavailable: no verified Kubernetes executor is connected to this recommendation. Use the operator workflow; no action or savings recorded.")


_GLOBAL_CONTROL_PLANE = SaaSControlPlane()


def get_control_plane() -> SaaSControlPlane:
    return _GLOBAL_CONTROL_PLANE
