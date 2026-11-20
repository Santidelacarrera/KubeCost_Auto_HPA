"""Persistent enterprise API workflows. Kubernetes execution remains with the operator."""

from __future__ import annotations

from dataclasses import asdict
import base64
import hashlib
import hmac
import json
import os
import time
import uuid

from . import finops, optimization
from .security import AuthorizationError, validate_identifier


PLANS = {
    "Starter": {
        "clusters": 1,
        "nodes": 10,
        "workloads": 50,
        "users": 3,
        "retention_days": 14,
        "features": ["observe"],
        "support": "business_hours",
    },
    "Professional": {
        "clusters": 3,
        "nodes": 50,
        "workloads": 250,
        "users": 10,
        "retention_days": 30,
        "features": ["observe", "assisted"],
        "support": "business_hours",
    },
    "Business": {
        "clusters": 10,
        "nodes": 250,
        "workloads": 2000,
        "users": 50,
        "retention_days": 90,
        "features": ["observe", "assisted", "automated"],
        "support": "priority",
    },
    "Enterprise": {
        "clusters": 100,
        "nodes": 10000,
        "workloads": 50000,
        "users": 1000,
        "retention_days": 365,
        "features": ["observe", "assisted", "automated"],
        "support": "contract",
    },
}


def verify_license(token, tenant, secret, now=None):
    """Server-side HMAC license validation; signing key must never reach agents/browsers."""
    if len(secret) < 32:
        raise ValueError("LICENSE_SIGNING_SECRET must contain at least 32 characters")
    if not isinstance(token, str) or len(token) > 16384:
        raise ValueError("invalid license")
    try:
        body, signature = token.split(".")
        expected = hmac.new(secret.encode(), body.encode(), hashlib.sha256).hexdigest()
        if not hmac.compare_digest(expected, signature):
            raise ValueError("invalid license signature")
        claims = json.loads(base64.urlsafe_b64decode(body + "=" * (-len(body) % 4)))
        now = time.time() if now is None else now
        if claims["tenant"] != tenant or claims["plan"] not in PLANS:
            raise ValueError("license organization or plan mismatch")
        validate_identifier(claims["id"], "license_id")
        optimization.number(claims["expires_at"], "expires_at", now + 0.001)
        optimization.number(claims["issued_at"], "issued_at", 0, now)
        return claims
    except (KeyError, TypeError, UnicodeError, json.JSONDecodeError) as exc:
        raise ValueError("malformed license") from exc


class EnterpriseService:
    def __init__(self, control_plane):
        self.cp = control_plane
        self.store = control_plane._store

    def usage(self, tenant):
        with self.cp._lock:
            events = self.store.telemetry_inventory(tenant)
            clusters = len(self.cp._agents.get(tenant, {}))
        return {
            "clusters": clusters,
            "nodes": len(
                {
                    (r["cluster_id"], r["node_name"])
                    for r in events
                    if r["node_name"] not in (None, "node-unknown")
                }
            ),
            "workloads": len(
                {(r["cluster_id"], r["namespace"], r["workload_name"]) for r in events}
            ),
            "users": sum(u["status"] == "active" for u in self.store.list_objects(tenant, "user")),
            "basis": "registered_clusters_and_retained_telemetry_inventory",
        }

    def active_plan(self, tenant):
        active = [
            r
            for r in self.store.list_objects(tenant, "license")
            if r.get("status") == "active" and r["claims"]["expires_at"] > time.time()
        ]
        if not active:
            raise AuthorizationError("an unexpired license is required")
        claims = max(active, key=lambda r: r["created_at"])["claims"]
        return PLANS[claims["plan"]]

    def enforce_inventory(self, tenant, record):
        if os.environ.get("REQUIRE_LICENSE") != "true":
            return
        plan = self.active_plan(tenant)
        if record.cluster_id not in self.cp._agents.get(tenant, {}):
            raise AuthorizationError("register the cluster before licensed telemetry ingestion")
        events = self.store.telemetry_inventory(tenant)
        nodes = {(r["cluster_id"], r["node_name"]) for r in events if r["node_name"] not in (None, "node-unknown")}
        workloads = {(r["cluster_id"], r["namespace"], r["workload_name"]) for r in events}
        if record.node_name != "node-unknown":
            nodes.add((record.cluster_id, record.node_name))
        workloads.add((record.cluster_id, record.namespace, record.workload_name))
        if len(nodes) > plan["nodes"] or len(workloads) > plan["workloads"]:
            raise AuthorizationError("licensed node or workload inventory limit reached")

    def enforce_license(self, tenant, feature="observe", new_cluster=False):
        if os.environ.get("REQUIRE_LICENSE") != "true":
            return
        plan = self.active_plan(tenant)
        if feature not in plan["features"]:
            raise AuthorizationError("feature is not included in the active plan")
        if new_cluster and self.usage(tenant)["clusters"] >= plan["clusters"]:
            raise AuthorizationError("licensed cluster limit reached")

    def _save(self, tenant, kind, value):
        value = {**value, "id": uuid.uuid4().hex, "created_at": time.time()}
        return self.store.put_object(tenant, kind, value["id"], value)

    def _cluster(self, tenant, payload):
        cluster = validate_identifier(payload["cluster_id"], "cluster_id")
        self.cp.verify_cluster_access(tenant, cluster, tenant_header=tenant)
        return cluster

    def get(self, route, tenant, query):
        if route == "alerts":
            now = time.time()
            alerts = []
            with self.cp._lock:
                agents = list(self.cp._agents.get(tenant, {}).values())
            for agent in agents:
                if now - agent.last_heartbeat_at > 120:
                    alerts.append({"id": "offline:" + agent.cluster_id, "cluster_id": agent.cluster_id, "severity": "warning", "status": "active", "reason": "heartbeat_missing", "last_observed_at": agent.last_heartbeat_at})
            for row in self.store.list_objects(tenant, "rightsizing"):
                if now - row["created_at"] <= 3600:
                    for anomaly in row.get("anomalies", []):
                        alerts.append({"id": row["id"] + ":" + anomaly, "cluster_id": row["cluster_id"], "workload": row["workload"], "severity": "warning", "status": "active", "reason": anomaly, "last_observed_at": row["created_at"]})
            acknowledgements = {r["id"]: r for r in self.store.list_objects(tenant, "alert_ack")}
            for alert in alerts:
                ack = acknowledgements.get(alert["id"])
                alert["ack_version"] = ack["version"] if ack else 0
                if ack and ack["observation"] == alert["last_observed_at"]:
                    alert["status"] = "acknowledged"
                    alert["acknowledged_by"] = ack["principal"]
            return {**self._page(alerts, query), "evaluated_at": now, "source": "heartbeat_and_recent_recommendations"}
        if route == "usage":
            return self.usage(tenant)
        if route == "plans":
            return {"plans": PLANS, "pricing_status": "proposal_no_live_charges"}
        if route == "clusters":
            with self.cp._lock:
                items = [asdict(a) for a in self.cp._agents.get(tenant, {}).values()]
            for row in items:
                row["status"] = (
                    "OFFLINE"
                    if time.time() - row["last_heartbeat_at"] > 120
                    else row["status"]
                )
            return self._page(items, query)
        if route == "costs":
            batches = self.store.list_objects(tenant, "cost_import")
            rows = [r for batch in batches for r in batch["records"]]
            return {
                "kind": "imported_billing",
                "groups": finops.summarize(
                    rows, query.get("dimension", ["cluster_id"])[0]
                ),
                "savings": {
                    "estimated": None,
                    "projected": None,
                    "observed": None,
                    "invoice_reconciled": None,
                },
            }
        kind = {
            "rightsizing": "rightsizing",
            "predictions": "prediction",
            "predictions/backtests": "backtest",
            "consolidation/plans": "consolidation",
            "licenses": "license",
            "reconciliations": "reconciliation",
            "subscriptions": "subscription",
        }.get(route)
        if kind:
            items = self.store.list_objects(tenant, kind)
            if route == "consolidation/plans" and "id" in query:
                object_id = validate_identifier(query["id"][0], "plan id")
                items = [item for item in items if item["id"] == object_id]
            if kind in ("subscription", "license"):
                for item in items:
                    if item.get("status") != "revoked" and item.get("claims", item).get("expires_at", 0) <= time.time():
                        item["status"] = "expired"
            if query.get("cluster_id"):
                cluster = self._cluster(tenant, {"cluster_id": query["cluster_id"][0]})
                items = [r for r in items if r.get("cluster_id") == cluster]
            return self._page(items, query)
        raise KeyError(route)

    @staticmethod
    def _page(items, query):
        limit = int(query.get("limit", ["50"])[0])
        offset = int(query.get("offset", ["0"])[0])
        if not 1 <= limit <= 200 or offset < 0:
            raise ValueError("limit must be 1..200 and offset nonnegative")
        return {
            "items": items[offset : offset + limit],
            "total": len(items),
            "offset": offset,
            "limit": limit,
            "next_offset": offset + limit if offset + limit < len(items) else None,
        }

    def post(self, route, tenant, payload, principal):
        if route == "maintenance/retention":
            from .maintenance import retain
            days = payload["days"]
            if os.environ.get("REQUIRE_LICENSE") == "true" and days > self.active_plan(tenant)["retention_days"]:
                raise ValueError("retention exceeds plan limit")
            return retain(self.cp, tenant, days, principal, payload.get("dry_run", True))
        if route == "alerts/ack":
            alert_id = payload["id"]
            alerts = self.get("alerts", tenant, {"limit": ["200"]})["items"]
            alert = next((a for a in alerts if a["id"] == alert_id), None)
            if alert is None:
                raise ValueError("active alert not found")
            return self.store.put_object(tenant, "alert_ack", alert_id, {"id": alert_id, "principal": principal, "created_at": time.time(), "observation": alert["last_observed_at"]}, int(payload.get("version", 0)))
        provenance = {"source": "submitted_samples"}
        if route in ("rightsizing", "predictions", "predictions/backtest") and payload.get("source") == "telemetry":
            from .history import resolve_samples

            self._cluster(tenant, payload)
            payload, provenance = resolve_samples(self.store, tenant, payload, route)
        if route not in ("licenses/activate", "licenses/revoke", "subscriptions/sandbox"):
            feature = (
                payload.get("mode", "observe-only")
                if route == "consolidation/plans"
                else "assisted" if route == "consolidation/execution"
                else "observe"
            )
            self.enforce_license(
                tenant, "observe" if feature == "observe-only" else feature
            )
        if route == "subscriptions/sandbox":
            from .subscriptions import transition

            return transition(self.store, tenant, payload, PLANS)
        if route == "predictions/backtest":
            cluster = self._cluster(tenant, payload)
            workload = validate_identifier(payload["workload"], "workload", allow_slash=True)
            return self._save(tenant, "backtest", {**optimization.backtest(payload), "cluster_id": cluster, "workload": workload, "provenance": provenance})
        if route in ("rightsizing", "predictions"):
            cluster = self._cluster(tenant, payload)
            workload = validate_identifier(
                payload["workload"], "workload", allow_slash=True
            )
            result = (
                optimization.rightsizing(payload)
                if route == "rightsizing"
                else optimization.forecast(payload)
            )
            return self._save(
                tenant,
                "rightsizing" if route == "rightsizing" else "prediction",
                {**result, "cluster_id": cluster, "workload": workload, "provenance": provenance},
            )
        if route == "predictions/observe":
            prediction = self._find(tenant, "prediction", payload["id"])
            observed = optimization.number(
                payload["observed_demand"], "observed_demand"
            )
            ts = optimization.number(
                payload["timestamp"],
                "timestamp",
                prediction["target_timestamp"] - 30,
                min(time.time() + 30, prediction["target_timestamp"] + 30),
            )
            if (
                prediction["predicted_demand"] is None
                or "observed_demand" in prediction
            ):
                raise ValueError("prediction is unavailable or already evaluated")
            prediction.update(
                observed_demand=observed,
                observed_at=ts,
                absolute_error=abs(observed - prediction["predicted_demand"]),
            )
            return self.store.put_object(
                tenant,
                "prediction",
                prediction["id"],
                prediction,
                prediction["version"],
            )
        if route == "finops/import":
            result = finops.normalize_records(payload["records"], payload["provider"])
            if result["errors"]:
                return {**result, "status": "rejected", "persisted": False}
            existing = {
                r["provider"] + ":" + r["record_id"]: r["source_sha256"]
                for b in self.store.list_objects(tenant, "cost_import")
                for r in b["records"]
            }
            for row in result["records"]:
                if row["cluster_id"]:
                    self._cluster(tenant, row)
                key = row["provider"] + ":" + row["record_id"]
                if key in existing:
                    raise ValueError(
                        "record already imported; duplicate financial data rejected"
                    )
            # Deterministic batch id prevents identical concurrent imports.
            batch_id = hashlib.sha256(
                json.dumps(
                    sorted(
                        r["provider"] + ":" + r["record_id"] for r in result["records"]
                    )
                ).encode()
            ).hexdigest()
            return self.store.import_financial_batch(
                tenant,
                batch_id,
                {
                    **result,
                    "id": batch_id,
                    "created_at": time.time(),
                    "status": "imported",
                    "persisted": True,
                },
            )
        if route == "finops/reconcile":
            records = [
                r
                for b in self.store.list_objects(tenant, "cost_import")
                for r in b["records"]
            ]
            return self._save(
                tenant, "reconciliation", finops.reconcile(records, payload["invoice"])
            )
        if route == "licenses/activate":
            claims = verify_license(
                payload["license"], tenant, os.environ.get("LICENSE_SIGNING_SECRET", "")
            )
            existing = [r for r in self.store.list_objects(tenant, "license") if r["claims"]["id"] == claims["id"]]
            if any(r.get("status") == "revoked" for r in existing):
                raise AuthorizationError("revoked license cannot be reactivated")
            if existing:
                previous = max(existing, key=lambda r: r["created_at"])
                if previous["claims"] == claims:
                    return previous
                if claims["issued_at"] <= previous["claims"]["issued_at"] or claims["expires_at"] <= previous["claims"]["expires_at"]:
                    raise ValueError("renewal must have a newer issue time and expiration")
                value = {**previous, "claims": claims, "activated_by": principal, "created_at": time.time()}
                value.setdefault("history", []).append({"action": "renewed", "principal": principal, "timestamp": time.time()})
                return self.store.put_object(tenant, "license", previous["id"], value, previous["version"])
            value = {"id": claims["id"], "claims": claims, "status": "active", "activated_by": principal, "created_at": time.time(), "history": []}
            return self.store.put_object(tenant, "license", value["id"], value)
        if route == "licenses/revoke":
            license_record = self._find(tenant, "license", payload["id"])
            reason = payload.get("reason", "")
            if not isinstance(reason, str) or not 1 <= len(reason.strip()) <= 500:
                raise ValueError("revocation reason must contain 1..500 characters")
            license_record["status"] = "revoked"
            license_record.setdefault("history", []).append({"action": "revoked", "principal": principal, "reason": reason, "timestamp": time.time()})
            return self.store.put_object(tenant, "license", license_record["id"], license_record, int(payload["version"]))
        if route == "consolidation/plans":
            from .binpacking import NodeBinPacker, parse_nodes

            cluster = self._cluster(tenant, payload)
            mode = payload.get("mode", "observe-only")
            strategy = payload.get("execution_strategy", "empty-only")
            if strategy not in ("empty-only", "stateless-eviction"):
                raise ValueError("invalid consolidation execution strategy")
            if strategy == "stateless-eviction" and mode != "assisted":
                raise ValueError("stateless eviction requires an assisted plan")
            if mode not in ("observe-only", "assisted", "automated"):
                raise ValueError("invalid consolidation mode")
            if (
                mode == "automated"
                and os.environ.get("ENABLE_AUTOMATED_CONSOLIDATION") != "true"
            ):
                raise ValueError("automated consolidation is disabled")
            result = asdict(
                NodeBinPacker().optimize_cluster(parse_nodes(payload["nodes"]))
            )
            return self._save(
                tenant,
                "consolidation",
                {
                    **result,
                    "cluster_id": cluster,
                    "mode": mode,
                    "execution_strategy": strategy,
                    "status": "observed"
                    if mode == "observe-only"
                    else "pending_approval",
                    "requested_by": principal,
                    "expires_at": time.time() + 300,
                    "savings_kind": "estimated",
                    "execution_status": "not_dispatched",
                    "history": [],
                },
            )
        if route == "consolidation/decision":
            plan = self._find(tenant, "consolidation", payload["id"])
            if (
                plan["mode"] == "observe-only"
                or plan["status"] != "pending_approval"
                or plan["expires_at"] <= time.time()
            ):
                raise ValueError("plan is not eligible for approval")
            decision = payload["decision"]
            if decision not in ("approve", "reject"):
                raise ValueError("decision must be approve or reject")
            if decision == "approve" and principal == plan["requested_by"]:
                raise AuthorizationError("approval requires a different principal")
            plan["status"] = "approved" if decision == "approve" else "rejected"
            plan["history"].append(
                {"decision": decision, "principal": principal, "timestamp": time.time()}
            )
            # A separate executor claims the approved candidate after its live preflight.
            return self.store.put_object(
                tenant, "consolidation", plan["id"], plan, int(payload["version"])
            )
        if route == "consolidation/execution":
            plan = self._find(tenant, "consolidation", payload["id"])
            node = validate_identifier(payload["node"], "node")
            if plan["status"] != "approved" or plan["mode"] != "assisted" or node not in plan["nodes_can_drain"]:
                raise ValueError("execution requires an approved assisted plan and a candidate node")
            action = payload["action"]
            state = plan["execution_status"]
            if action == "claim":
                if state != "not_dispatched" or plan["expires_at"] <= time.time():
                    raise ValueError("plan cannot be claimed")
                plan.update(execution_status="claimed", execution_node=node, executor=principal)
            elif action in ("recovering", "recovered"):
                allowed = ("claimed", "recovering") if action == "recovering" else ("recovering",)
                if (state not in allowed or plan.get("execution_strategy") != "stateless-eviction"
                        or plan.get("executor") != principal or plan.get("execution_node") != node):
                    raise AuthorizationError("only the claiming executor can recover this eviction")
                plan["execution_status"] = action
            elif action in ("cordoned", "failed"):
                if state != "claimed" or plan["executor"] != principal or plan["execution_node"] != node:
                    raise AuthorizationError("only the claiming executor can report this node")
                plan["execution_status"] = action
            else:
                raise ValueError("invalid execution action")
            plan["history"].append({"execution": action, "node": node, "principal": principal, "timestamp": time.time()})
            return self.store.put_object(tenant, "consolidation", plan["id"], plan, int(payload["version"]))
        raise KeyError(route)

    def _find(self, tenant, kind, object_id):
        matches = [
            r for r in self.store.list_objects(tenant, kind) if r["id"] == object_id
        ]
        if not matches:
            raise ValueError("object not found in organization")
        return matches[0]
