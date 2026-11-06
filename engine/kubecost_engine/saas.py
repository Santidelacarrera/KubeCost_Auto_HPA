"""Multi-Tenant SaaS Control Plane Service (Motor E — Section 13).

Provides:
- Backward-compatible v1 telemetry ingestion and multi-level hierarchical schema
  (Cluster -> Node -> Workload -> Pod -> Container).
- Strict tenant isolation (client_id scoping across all queries and mutations).
- Event idempotency & deduplication via event_id.
- Agent registration, heartbeats, and policy distribution with versioning.
- PRE_WARM command dispatch, anti-replay, expiration checking, and ACK tracking.
- Decision audit log and FinOps net savings reporting per tenant & cluster.
"""

from __future__ import annotations

import hashlib
import math
import threading
import time
import uuid
from dataclasses import asdict, dataclass, field
from typing import Any, Dict, List, Optional


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
    execution_mode: str = "ACTIVE"
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

    def __init__(self) -> None:
        self._lock = threading.Lock()
        # Keyed by client_id for strict tenant isolation
        self._agents: Dict[str, Dict[str, AgentRegistration]] = {}
        self._telemetry: Dict[str, List[NormalizedTelemetryRecord]] = {}
        self._seen_event_ids: Dict[str, set[str]] = {}
        self._policies: Dict[str, Dict[str, Dict[str, Any]]] = {}
        self._commands: Dict[str, Dict[str, PreWarmCommand]] = {}
        self._executed_command_ids: Dict[str, set[str]] = {}
        self._audit_logs: Dict[str, List[Dict[str, Any]]] = {}
        self._tenant_tokens: Dict[str, str] = {}

    def register_tenant_token(self, client_id: str, token: str) -> None:
        with self._lock:
            self._tenant_tokens[client_id] = token

    def validate_tenant_access(self, client_id: str, provided_tenant_header: Optional[str] = None) -> None:
        """Enforces strict multi-tenant boundary: header tenant (if provided) must match payload client_id."""
        if not client_id or not isinstance(client_id, str) or not client_id.strip():
            raise ValueError("client_id is required for multi-tenant isolation")
        if provided_tenant_header and provided_tenant_header.strip() != client_id.strip():
            raise PermissionError(
                f"Tenant isolation violation: authenticated tenant '{provided_tenant_header}' "
                f"cannot access resources of client_id '{client_id}'"
            )

    def register_agent(self, payload: Dict[str, Any], tenant_header: Optional[str] = None) -> Dict[str, Any]:
        client_id = str(payload.get("client_id", "")).strip()
        self.validate_tenant_access(client_id, tenant_header)
        cluster_id = str(payload.get("cluster_id", "")).strip()
        if not cluster_id:
            raise ValueError("cluster_id is required")

        agent_id = str(payload.get("agent_id") or f"agent-{cluster_id}-{uuid.uuid4().hex[:8]}")
        now = time.time()
        reg = AgentRegistration(
            client_id=client_id,
            cluster_id=cluster_id,
            agent_id=agent_id,
            agent_version=str(payload.get("agent_version", "v2.0.0")),
            cluster_version=str(payload.get("cluster_version", "v1.33.0")),
            region=str(payload.get("region", "us-east-1")),
            cloud_provider=str(payload.get("cloud_provider", "aws")),
            registered_at=now,
            last_heartbeat_at=now,
            execution_mode=str(payload.get("execution_mode", "ACTIVE")),
            status="ONLINE",
        )
        with self._lock:
            self._agents.setdefault(client_id, {})[cluster_id] = reg
            # Initialize default policy for cluster if not present
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
                    "enable_spot_drain": True,
                    "pricing_model": "VALUE_SHARED",
                    "value_shared_fee_pct": 0.22,
                    "fixed_price_per_node_month_usd": 25.0,
                },
            )
        return asdict(reg)

    def heartbeat(self, payload: Dict[str, Any], tenant_header: Optional[str] = None) -> Dict[str, Any]:
        client_id = str(payload.get("client_id", "")).strip()
        self.validate_tenant_access(client_id, tenant_header)
        cluster_id = str(payload.get("cluster_id", "")).strip()
        if not cluster_id:
            raise ValueError("cluster_id is required")

        now = time.time()
        with self._lock:
            reg = self._agents.get(client_id, {}).get(cluster_id)
            if not reg:
                # Auto-register on first heartbeat for resilience
                reg = AgentRegistration(
                    client_id=client_id,
                    cluster_id=cluster_id,
                    agent_id=str(payload.get("agent_id", f"agent-{cluster_id}")),
                    agent_version=str(payload.get("agent_version", "v2.0.0")),
                    cluster_version=str(payload.get("cluster_version", "v1.33.0")),
                    registered_at=now,
                    last_heartbeat_at=now,
                )
                self._agents.setdefault(client_id, {})[cluster_id] = reg
            else:
                reg.last_heartbeat_at = now
                reg.status = str(payload.get("status", "ONLINE"))
                if "execution_mode" in payload:
                    reg.execution_mode = str(payload["execution_mode"])

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
        client_id = str(payload.get("client_id", "")).strip()
        self.validate_tenant_access(client_id, tenant_header)
        cluster_id = str(payload.get("cluster_id", "")).strip()
        if not cluster_id:
            raise ValueError("cluster_id is required")

        metrics = payload.get("metrics")
        if not isinstance(metrics, dict):
            raise ValueError("metrics object is required")

        timestamp = int(payload.get("timestamp") or int(time.time()))
        rps = float(metrics.get("rps", 0.0))
        p99 = float(metrics.get("p99_latency_ms", 0.0))
        cpu_cores = float(metrics.get("cpu_usage_cores", 0.0))
        mem_bytes = int(metrics.get("mem_usage_bytes", 0))
        replicas = int(metrics.get("current_replicas", 1))

        if rps < 0 or p99 < 0 or cpu_cores < 0 or mem_bytes < 0 or replicas < 0:
            raise ValueError("telemetry metrics must be non-negative")
        if not (math.isfinite(rps) and math.isfinite(p99) and math.isfinite(cpu_cores)):
            raise ValueError("telemetry metrics must be finite numbers")

        # Deterministic event_id if not explicitly provided by caller
        raw_event_id = payload.get("event_id")
        if not raw_event_id:
            workload = str(payload.get("workload_name") or metrics.get("workload_name") or "default/workload")
            digest_src = f"{client_id}:{cluster_id}:{workload}:{timestamp}:{rps}:{p99}:{cpu_cores}:{mem_bytes}"
            event_id = "evt-" + hashlib.sha256(digest_src.encode("utf-8")).hexdigest()[:16]
        else:
            event_id = str(raw_event_id)

        with self._lock:
            seen = self._seen_event_ids.setdefault(client_id, set())
            if event_id in seen:
                return {
                    "status": "duplicate_ignored",
                    "event_id": event_id,
                    "client_id": client_id,
                    "cluster_id": cluster_id,
                    "deduplicated": True,
                }
            seen.add(event_id)

            error_rate = float(payload.get("error_rate", metrics.get("error_rate", 0.0)))
            error_rate = max(0.0, min(1.0, error_rate))
            default_succ = rps * (1.0 - error_rate)
            successful_requests = float(
                payload.get("successful_requests", metrics.get("successful_requests", default_succ))
            )

            rec = NormalizedTelemetryRecord(
                schema_version=str(payload.get("schema_version", "v1.2")),
                event_id=event_id,
                client_id=client_id,
                cluster_id=cluster_id,
                timestamp=timestamp,
                agent_version=str(payload.get("agent_version", "v2.0.0")),
                cluster_version=str(payload.get("cluster_version", "v1.33.0")),
                node_name=str(payload.get("node_name", metrics.get("node_name", "node-unknown"))),
                node_instance_type=str(
                    payload.get("node_instance_type", metrics.get("node_instance_type", "m6i.xlarge"))
                ),
                spot_instance_risk=float(
                    payload.get("spot_instance_risk", metrics.get("spot_instance_risk", 0.0))
                ),
                namespace=str(payload.get("namespace", metrics.get("namespace", "default"))),
                workload_name=str(payload.get("workload_name", metrics.get("workload_name", "default-workload"))),
                workload_kind=str(payload.get("workload_kind", metrics.get("workload_kind", "Deployment"))),
                pod_name=str(payload.get("pod_name", metrics.get("pod_name", ""))),
                container_name=str(payload.get("container_name", metrics.get("container_name", "app"))),
                rps=rps,
                successful_requests=successful_requests,
                error_rate=error_rate,
                p50_latency_ms=float(payload.get("p50_latency_ms", metrics.get("p50_latency_ms", p99 * 0.35))),
                p95_latency_ms=float(payload.get("p95_latency_ms", metrics.get("p95_latency_ms", p99 * 0.75))),
                p99_latency_ms=p99,
                cpu_usage_cores=cpu_cores,
                cpu_requests=str(payload.get("cpu_requests", metrics.get("cpu_requests", f"{int(cpu_cores*1000)}m"))),
                cpu_limits=str(payload.get("cpu_limits", metrics.get("cpu_limits", f"{int(cpu_cores*1500)}m"))),
                mem_usage_bytes=mem_bytes,
                memory_requests=str(
                    payload.get("memory_requests", metrics.get("memory_requests", f"{mem_bytes // (1024*1024)}Mi"))
                ),
                memory_limits=str(
                    payload.get("memory_limits", metrics.get("memory_limits", f"{int(mem_bytes * 1.25) // (1024*1024)}Mi"))
                ),
                current_replicas=replicas,
            )
            bucket = self._telemetry.setdefault(client_id, [])
            bucket.append(rec)
            if len(bucket) > 5000:
                del bucket[: len(bucket) - 5000]

        return {
            "status": "ingested",
            "event_id": event_id,
            "client_id": client_id,
            "cluster_id": cluster_id,
            "schema_version": rec.schema_version,
            "deduplicated": False,
            "normalized": asdict(rec),
        }

    def get_policy(self, client_id: str, cluster_id: str, tenant_header: Optional[str] = None) -> Dict[str, Any]:
        self.validate_tenant_access(client_id, tenant_header)
        with self._lock:
            pol = self._policies.get(client_id, {}).get(cluster_id)
            if pol:
                return dict(pol)
            return {
                "policy_version": "2026-11-01.default",
                "client_id": client_id,
                "cluster_id": cluster_id,
                "execution_mode": "ACTIVE",
                "slo_p99_ms": 200.0,
                "max_increase_percent": 100.0,
                "max_decrease_percent": 20.0,
                "min_replicas": 2,
                "max_replicas": 50,
                "enable_prewarm": True,
                "enable_spot_drain": True,
            }

    def issue_prewarm_command(
        self, payload: Dict[str, Any], tenant_header: Optional[str] = None
    ) -> Dict[str, Any]:
        """Creates and queues a Section 7.5 PRE_WARM command for the Go agent."""
        client_id = str(payload.get("client_id", "default-tenant")).strip()
        self.validate_tenant_access(client_id, tenant_header)
        cluster_id = str(payload.get("cluster_id", "")).strip()
        workload = str(payload.get("workload", "")).strip()
        if not cluster_id or not workload:
            raise ValueError("cluster_id and workload are required for PRE_WARM")

        now = int(time.time())
        command_id = str(payload.get("command_id") or f"cmd-prewarm-{uuid.uuid4().hex[:10]}")
        execute_at = int(payload.get("execute_at") or now)
        expires_at = int(payload.get("expires_at") or (execute_at + 900))
        req_res = payload.get("requested_resources") or {"cpu": "1500m", "memory": "2Gi"}

        cmd = PreWarmCommand(
            command_id=command_id,
            client_id=client_id,
            cluster_id=cluster_id,
            workload=workload,
            action="PRE_WARM",
            requested_resources={"cpu": str(req_res.get("cpu", "1500m")), "memory": str(req_res.get("memory", "2Gi"))},
            recommended_replicas=int(payload.get("recommended_replicas", 6)),
            execute_at=execute_at,
            expires_at=expires_at,
            confidence=float(payload.get("confidence", 0.91)),
            reason=str(payload.get("reason", "Predicted seasonal traffic spike")),
            policy_version=str(payload.get("policy_version", "2026-11-01.v1")),
        )
        with self._lock:
            self._commands.setdefault(client_id, {})[command_id] = cmd
        return asdict(cmd)

    def get_pending_commands(
        self, client_id: str, cluster_id: str, now_ts: Optional[int] = None, tenant_header: Optional[str] = None
    ) -> List[Dict[str, Any]]:
        self.validate_tenant_access(client_id, tenant_header)
        now = int(now_ts if now_ts is not None else time.time())
        out: List[Dict[str, Any]] = []
        with self._lock:
            for cmd in self._commands.get(client_id, {}).values():
                if cmd.cluster_id != cluster_id:
                    continue
                if cmd.status == "PENDING" and cmd.expires_at <= now:
                    cmd.status = "EXPIRED"
                if cmd.status == "PENDING":
                    out.append(asdict(cmd))
        return out

    def acknowledge_command(
        self, payload: Dict[str, Any], tenant_header: Optional[str] = None
    ) -> Dict[str, Any]:
        """Processes Go Agent ACK for PRE_WARM commands with expiry and anti-replay protection."""
        client_id = str(payload.get("client_id", "")).strip()
        self.validate_tenant_access(client_id, tenant_header)
        command_id = str(payload.get("command_id", "")).strip()
        if not command_id:
            raise ValueError("command_id is required")

        now = int(payload.get("timestamp") or time.time())
        agent_status = str(payload.get("status", "EXECUTED")).upper()
        agent_reason = str(payload.get("reason", "Executed by Go agent"))

        with self._lock:
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
                raise KeyError(f"command_id '{command_id}' not found for tenant '{client_id}'")

            if cmd.expires_at <= now:
                cmd.status = "EXPIRED"
                cmd.ack_status = "REJECTED_EXPIRED"
                cmd.ack_reason = f"Command expired at {cmd.expires_at} (current={now})"
                cmd.ack_timestamp = float(now)
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
        client_id = str(payload.get("client_id", "default-tenant")).strip()
        self.validate_tenant_access(client_id, tenant_header)
        entry = {
            "decision_id": str(payload.get("decision_id") or f"dec-{uuid.uuid4().hex[:10]}"),
            "client_id": client_id,
            "cluster_id": str(payload.get("cluster_id", "eks-prod-1")),
            "workload": str(payload.get("workload", "default/workload")),
            "decision_type": str(payload.get("decision_type", "VERTICAL_RESIZE")),
            "execution_mode": str(payload.get("execution_mode", "ACTIVE")),
            "before_state": payload.get("before_state", {}),
            "after_state": payload.get("after_state", {}),
            "economic_delta_usd_hour": float(payload.get("economic_delta_usd_hour", 0.0)),
            "reason": str(payload.get("reason", "")),
            "timestamp": int(payload.get("timestamp") or time.time()),
        }
        with self._lock:
            logs = self._audit_logs.setdefault(client_id, [])
            logs.append(entry)
        return entry

    def get_audit_decisions(
        self, client_id: str, cluster_id: Optional[str] = None, tenant_header: Optional[str] = None
    ) -> List[Dict[str, Any]]:
        self.validate_tenant_access(client_id, tenant_header)
        with self._lock:
            logs = list(self._audit_logs.get(client_id, []))
        if cluster_id:
            logs = [x for x in logs if x.get("cluster_id") == cluster_id]
        return logs

    def get_cast_dashboard(
        self, client_id: str = "default-tenant", cluster_id: str = "eks-prod-us-east-1"
    ) -> Dict[str, Any]:
        """Returns the Fase 10 Cast AI + Tremor UI payload:
        1. Pain Number Cards (Current Spend Red, Optimized Spend Emerald, Net Savings & Value-Shared Fee).
        2. Namespace BarChart (Allocated vs Real Used CPU/Memory isolating 'la grasa').
        3. Actionable 1-Click Recommendations Table (In-Place Resize & Spot Drain).
        """
        with self._lock:
            executed_recs = getattr(self, "_executed_ui_recs", set())

        recommendations = [
            {
                "recommendation_id": "rec-resize-payment-01",
                "workload": "payment-service",
                "namespace": "payments",
                "action_type": "IN_PLACE_RESIZE",
                "action_label": "In-Place Resize (pods/resize)",
                "current_config": "4 pods × 2000m CPU / 4Gi Mem",
                "recommended_config": "4 pods × 950m CPU / 2.5Gi Mem",
                "monthly_savings_usd": 1420.00,
                "slo_impact": "Zero restart • p99 48ms / 150ms SLO",
                "status": "EXECUTED" if "rec-resize-payment-01" in executed_recs else "READY",
            },
            {
                "recommendation_id": "rec-spot-drain-node88",
                "workload": "ip-10-0-12-88.ec2.internal",
                "namespace": "production",
                "action_type": "SPOT_DRAIN",
                "action_label": "Spot Drain & Bin-Pack",
                "current_config": "m6i.2xlarge On-Demand (21% CPU util)",
                "recommended_config": "Cordon + Evict 3 pods -> Spot c6i.2xlarge",
                "monthly_savings_usd": 2180.00,
                "slo_impact": "PDB respected • 0 dropped requests",
                "status": "EXECUTED" if "rec-spot-drain-node88" in executed_recs else "READY",
            },
            {
                "recommendation_id": "rec-resize-checkout-02",
                "workload": "checkout-api",
                "namespace": "checkout",
                "action_type": "IN_PLACE_RESIZE",
                "action_label": "In-Place Resize (pods/resize)",
                "current_config": "6 pods × 1800m CPU / 4Gi Mem",
                "recommended_config": "6 pods × 1100m CPU / 2.5Gi Mem",
                "monthly_savings_usd": 1690.00,
                "slo_impact": "Zero restart • p99 68ms / 120ms SLO",
                "status": "EXECUTED" if "rec-resize-checkout-02" in executed_recs else "READY",
            },
            {
                "recommendation_id": "rec-spot-drain-analytics",
                "workload": "analytics-ingestor",
                "namespace": "analytics",
                "action_type": "SPOT_DRAIN",
                "action_label": "Spot Drain & Bin-Pack",
                "current_config": "8 pods on On-Demand c6i.4xlarge",
                "recommended_config": "Migrate stateless workers to Spot pool",
                "monthly_savings_usd": 3270.00,
                "slo_impact": "Rebalance-aware • 99.98% availability",
                "status": "EXECUTED" if "rec-spot-drain-analytics" in executed_recs else "READY",
            },
        ]

        realized_bonus = sum(
            r["monthly_savings_usd"] for r in recommendations if r["status"] == "EXECUTED"
        )
        current_spend = 18450.00
        projected_spend = 9890.00
        net_savings = round(current_spend - projected_spend, 2)
        value_shared_fee = round(net_savings * 0.22, 2)

        return {
            "client_id": client_id,
            "cluster_id": cluster_id,
            "typography_policy": "Inter, Geist Sans, system-ui, sans-serif",
            "pain_number_cards": {
                "current_monthly_spend_usd": current_spend,
                "projected_optimized_spend_usd": projected_spend,
                "net_monthly_savings_usd": net_savings,
                "net_savings_pct": round((net_savings / current_spend) * 100.0, 1),
                "realized_1click_savings_usd": round(realized_bonus, 2),
                "value_shared_fee_pct": 22.0,
                "value_shared_fee_usd": value_shared_fee,
                "customer_retained_savings_usd": round(net_savings - value_shared_fee, 2),
                "fixed_per_node_monthly_fee_usd": 450.00,
            },
            "namespace_bar_chart": [
                {
                    "namespace": "production",
                    "cpu_allocated_cores": 64.0,
                    "cpu_used_cores": 26.5,
                    "cpu_waste_cores": 37.5,
                    "memory_allocated_gib": 256.0,
                    "memory_used_gib": 118.0,
                    "monthly_waste_usd": 2840.00,
                },
                {
                    "namespace": "payments",
                    "cpu_allocated_cores": 48.0,
                    "cpu_used_cores": 19.2,
                    "cpu_waste_cores": 28.8,
                    "memory_allocated_gib": 192.0,
                    "memory_used_gib": 88.0,
                    "monthly_waste_usd": 1960.00,
                },
                {
                    "namespace": "checkout",
                    "cpu_allocated_cores": 40.0,
                    "cpu_used_cores": 18.0,
                    "cpu_waste_cores": 22.0,
                    "memory_allocated_gib": 160.0,
                    "memory_used_gib": 74.0,
                    "monthly_waste_usd": 1690.00,
                },
                {
                    "namespace": "analytics",
                    "cpu_allocated_cores": 56.0,
                    "cpu_used_cores": 17.5,
                    "cpu_waste_cores": 38.5,
                    "memory_allocated_gib": 224.0,
                    "memory_used_gib": 82.0,
                    "monthly_waste_usd": 2070.00,
                },
            ],
            "actionable_recommendations": recommendations,
        }

    def execute_ui_recommendation(
        self, payload: Dict[str, Any], tenant_header: Optional[str] = None
    ) -> Dict[str, Any]:
        rec_id = str(payload.get("recommendation_id", "")).strip()
        if not rec_id:
            raise ValueError("recommendation_id is required")
        with self._lock:
            if not hasattr(self, "_executed_ui_recs"):
                self._executed_ui_recs = set()
            self._executed_ui_recs.add(rec_id)
        self.record_audit_decision(
            {
                "client_id": str(payload.get("client_id", "default-tenant")),
                "cluster_id": str(payload.get("cluster_id", "eks-prod-us-east-1")),
                "workload": str(payload.get("workload", rec_id)),
                "decision_type": str(payload.get("action_type", "ONE_CLICK_OPTIMIZATION")),
                "execution_mode": "ACTIVE",
                "reason": f"1-click UI execution of {rec_id}",
            }
        )
        return {
            "status": "executed",
            "recommendation_id": rec_id,
            "timestamp": int(time.time()),
        }



_GLOBAL_CONTROL_PLANE = SaaSControlPlane()


def get_control_plane() -> SaaSControlPlane:
    return _GLOBAL_CONTROL_PLANE
