"""HTTP front-end so the Go operator can consume the engine.

stdlib only. Endpoints: GET /healthz, GET /metrics, POST /v1/evaluate.
"""
import concurrent.futures
import hmac
import json
import math
import os
import ssl
import threading
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from .binpacking import ConsolidationResult, NodeBinPacker, NodeState, PodPlacement
from .economics import cost_per_request_from_pod
from .guardrails import BudgetGuardrail
from .oom_shield import OOMProtectionShield, OOMShieldDecision
from .predictive import ForecastResult, PredictiveAutoscaler
from .saas import get_control_plane
from .scaling import recommend_memory, recommend_replicas, recommend_resize
from .spot import SpotCandidate, is_spot_eligible

_counters: dict[str, int] = {}
_lock = threading.Lock()


def _count(action: str) -> None:
    with _lock:
        _counters[action] = _counters.get(action, 0) + 1


def _num(req: dict, key: str, default: float | None = None) -> float:
    """A finite, non-negative number (NaN/Infinity/negatives are rejected)."""
    if key not in req:
        if default is None:
            raise KeyError(key)
        return default
    v = req[key]
    if isinstance(v, bool) or not isinstance(v, (int, float)):
        raise ValueError(f"{key} must be a number")
    if not math.isfinite(v) or v < 0:
        raise ValueError(f"{key} must be finite and non-negative")
    return float(v)


def _reject_constant(name: str):
    raise ValueError(f"invalid JSON constant {name}")


def evaluate(req: dict) -> dict:
    """Pure evaluation of one workload. Raises ValueError/KeyError on bad input."""
    if not isinstance(req, dict):
        raise ValueError("request must be a JSON object")
    cpu = int(_num(req, "cpu_millicores"))
    if cpu <= 0:
        raise ValueError("cpu_millicores must be positive")
    rps = _num(req, "rps", 0)
    cpu_price = _num(req, "cpu_price_per_core_hour", 0)
    mem_price = _num(req, "mem_price_per_gib_hour", 0)
    mem_gib = _num(req, "memory_gib", 0)
    max_cpr = _num(req, "max_cost_per_request", 0)
    p99 = _num(req, "p99_ms")
    slo = _num(req, "slo_p99_ms")
    min_m, max_m = int(_num(req, "min_millicores", 50)), int(_num(req, "max_millicores", 8000))
    if min_m > max_m:
        raise ValueError("min_millicores must be <= max_millicores")
    usage = req.get("cpu_usage_millicores")
    if usage is not None:
        usage = _num(req, "cpu_usage_millicores")

    cpr = cost_per_request_from_pod(cpu / 1000, mem_gib, rps, cpu_price, mem_price)
    over = max_cpr > 0 and cpr > max_cpr

    d = recommend_resize(
        cpu,
        p99,
        slo,
        min_millicores=min_m,
        max_millicores=max_m,
        cpu_usage_millicores=usage,
        cost_over_target=over,
    )

    b = req.get("budget") or {}
    frozen = False
    action, new_cpu, reason = d.action, d.cpu_millicores, d.reason
    if b.get("monthly_limit"):
        g = BudgetGuardrail(float(b["monthly_limit"]), float(b.get("spent", 0)))
        frozen = g.frozen
        if not g.allows(action):
            action, new_cpu, reason = "hold", cpu, "budget frozen: growth blocked"

    spot = None
    if req.get("spot"):
        s = req["spot"]
        spot = is_spot_eligible(
            SpotCandidate(bool(s.get("fault_tolerant")), int(s.get("replicas", 0)),
                          bool(s.get("has_pdb")), bool(s.get("stateful"))),
            int(s.get("min_replicas", 2)),
        )

    # Optional memory (in MiB) and replica recommendations.
    mem_out = None
    m = req.get("memory")
    if m:
        md = recommend_memory(int(m["request_mib"]), float(m["usage_mib"]),
                              min_mib=int(m.get("min_mib", 64)), max_mib=int(m.get("max_mib", 16384)))
        if md.action == "grow" or (md.action == "shrink" and not frozen):
            mem_out = {"action": md.action, "mib": md.cpu_millicores, "reason": md.reason}
        else:
            mem_out = {"action": "hold", "mib": int(m["request_mib"]), "reason": md.reason}
    rep_out = None
    r = req.get("replicas")
    if r:
        target, why = recommend_replicas(
            int(r["current"]), int(r["min"]), int(r["max"]), d,
            cpu_usage_millicores=req.get("cpu_usage_millicores"), cpu_millicores=cpu,
            configured_min_millicores=int(req.get("min_millicores", 0)))
        if frozen and target > int(r["current"]):
            target, why = int(r["current"]), "budget frozen: scale out blocked"
        rep_out = {"target": target, "reason": why}

    after = cost_per_request_from_pod(new_cpu / 1000, mem_gib, rps, cpu_price, mem_price)
    _count(action)

    # ScaleOps Parity: Predictive Autoscaling forecast
    predictive_out = None
    if req.get("predictive"):
        p = req["predictive"]
        pa = PredictiveAutoscaler(
            min_replicas=int(r.get("min", 2)) if r else 2,
            max_replicas=int(r.get("max", 50)) if r else 50,
            warmup_lead_time_seconds=int(p.get("warmup_lead_time_seconds", 120)),
        )
        p_res = pa.forecast_demand(
            current_replicas=int(r["current"]) if r else 1,
            current_rps=rps,
            rps_history=p.get("rps_history"),
            target_rps_per_pod=float(p.get("target_rps_per_pod", 50.0)),
            hour_of_day_utc=p.get("hour_of_day_utc"),
        )
        predictive_out = {
            "predicted_replicas": p_res.predicted_replicas,
            "reactive_replicas": p_res.reactive_replicas,
            "recommended_replicas": p_res.recommended_replicas,
            "is_prewarming": p_res.is_prewarming,
            "confidence_score": p_res.confidence_score,
            "reason": p_res.reason,
        }
        if p_res.is_prewarming and not frozen and rep_out:
            rep_out["target"] = max(rep_out["target"], p_res.recommended_replicas)
            rep_out["reason"] = f"{rep_out['reason']} (Prewarmed by {p_res.reason})"

    # ScaleOps Parity: OOM Protection Shield
    oom_out = None
    if req.get("oom_protection") or (m and m.get("limit_mib")):
        oom_cfg = req.get("oom_protection") or {}
        shield = OOMProtectionShield(
            critical_threshold=float(oom_cfg.get("critical_threshold", 0.88)),
            warning_threshold=float(oom_cfg.get("warning_threshold", 0.78)),
        )
        cur_limit = int(m.get("limit_mib") or m.get("max_mib", 4096)) if m else 4096
        cur_req = int(m.get("request_mib", 1024)) if m else 1024
        cur_usage = float(m.get("usage_mib", 512.0)) if m else 512.0
        oom_dec = shield.evaluate_memory_risk(
            current_usage_mib=cur_usage,
            current_request_mib=cur_req,
            current_limit_mib=cur_limit,
            usage_history_mib=oom_cfg.get("usage_history_mib"),
            max_allowed_limit_mib=int(oom_cfg.get("max_allowed_limit_mib", 16384)),
        )
        oom_out = {
            "is_at_risk": oom_dec.is_at_risk,
            "risk_level": oom_dec.risk_level,
            "recommended_limit_mib": oom_dec.recommended_limit_mib,
            "recommended_request_mib": oom_dec.recommended_request_mib,
            "headroom_percentage": oom_dec.headroom_percentage,
            "estimated_seconds_to_oom": oom_dec.estimated_seconds_to_oom,
            "action": oom_dec.action,
            "reason": oom_dec.reason,
        }
        if oom_dec.is_at_risk and mem_out:
            mem_out["action"] = "grow"
            mem_out["mib"] = oom_dec.recommended_request_mib
            mem_out["reason"] = oom_dec.reason

    decision_res = {
        "action": action,
        "cpu_millicores": new_cpu,
        "reason": reason,
        "cost_per_request": None if cpr == float("inf") else cpr,
        "cost_per_request_after": None if after == float("inf") else after,
        "budget_frozen": frozen,
        "spot_eligible": spot,
        "memory": mem_out,
        "replicas": rep_out,
        "predictive": predictive_out,
        "oom_protection": oom_out,
    }

    if os.environ.get("NEXUS_TELEMETRY_ENABLED", "").lower() in ("1", "true", "yes"):
        try:
            from .telemetry import get_telemetry_client

            get_telemetry_client().publish_scaling_decision(decision_res, request_context=req)
        except Exception:
            pass

    return decision_res


def render_metrics() -> str:
    lines = ["# TYPE kubecost_engine_decisions_total counter"]
    with _lock:
        for k, v in sorted(_counters.items()):
            lines.append(f'kubecost_engine_decisions_total{{action="{k}"}} {v}')
    return "\n".join(lines) + "\n"


def _token() -> str:
    return os.environ.get("ENGINE_TOKEN", "")


def _get_web_dir() -> str | None:
    """Locate web static assets directory if available."""
    custom = os.environ.get("WEB_DIR")
    if custom and os.path.isdir(custom):
        return os.path.abspath(custom)
    candidates = [
        os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", "web")),
        os.path.abspath(os.path.join(os.path.dirname(__file__), "web")),
        "/app/web",
    ]
    for c in candidates:
        if os.path.isdir(c):
            return c
    return None


_MIME_TYPES = {
    ".html": "text/html; charset=utf-8",
    ".css": "text/css; charset=utf-8",
    ".js": "application/javascript; charset=utf-8",
    ".json": "application/json; charset=utf-8",
    ".svg": "image/svg+xml",
    ".png": "image/png",
    ".ico": "image/x-icon",
    ".txt": "text/plain; charset=utf-8",
}


class Handler(BaseHTTPRequestHandler):
    def _authorized(self) -> bool:
        """Bearer-token check for /v1/*; disabled when ENGINE_TOKEN is unset."""
        token = _token()
        if not token:
            return True
        got = self.headers.get("Authorization", "")
        return hmac.compare_digest(got.encode(), f"Bearer {token}".encode())

    def _send(self, code: int, body: str | bytes, ctype: str = "application/json") -> None:
        data = body.encode("utf-8") if isinstance(body, str) else body
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(data)))
        self.send_header("Access-Control-Allow-Origin", "*")
        self.send_header("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
        self.send_header("Access-Control-Allow-Headers", "Content-Type, Authorization")
        self.end_headers()
        self.wfile.write(data)

    def do_OPTIONS(self):  # noqa: N802
        """CORS preflight handling."""
        self.send_response(204)
        self.send_header("Access-Control-Allow-Origin", "*")
        self.send_header("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
        self.send_header("Access-Control-Allow-Headers", "Content-Type, Authorization")
        self.send_header("Access-Control-Max-Age", "86400")
        self.end_headers()

    def _serve_file(self, file_path: str, ctype: str) -> bool:
        try:
            with open(file_path, "rb") as f:
                content = f.read()
            self._send(200, content, ctype)
            return True
        except OSError:
            return False

    def do_GET(self):  # noqa: N802
        parsed = urllib.parse.urlparse(self.path)
        path_only = parsed.path
        qs = urllib.parse.parse_qs(parsed.query)
        tenant_hdr = self.headers.get("X-Tenant-ID")

        if path_only in ("/healthz", "/health"):
            self._send(200, '{"status":"ok"}')
        elif path_only == "/metrics":
            self._send(200, render_metrics(), "text/plain; version=0.0.4")
        elif path_only == "/v1/policies":
            if not self._authorized():
                return self._send(401, '{"error":"unauthorized"}')
            try:
                client_id = qs.get("client_id", [""])[0]
                cluster_id = qs.get("cluster_id", ["eks-prod-1"])[0]
                res = get_control_plane().get_policy(client_id, cluster_id, tenant_header=tenant_hdr)
                return self._send(200, json.dumps(res))
            except PermissionError as e:
                return self._send(403, json.dumps({"error": str(e)}))
            except Exception as e:
                return self._send(400, json.dumps({"error": str(e)}))
        elif path_only == "/v1/commands/pending":
            if not self._authorized():
                return self._send(401, '{"error":"unauthorized"}')
            try:
                client_id = qs.get("client_id", [""])[0]
                cluster_id = qs.get("cluster_id", ["eks-prod-1"])[0]
                res = get_control_plane().get_pending_commands(client_id, cluster_id, tenant_header=tenant_hdr)
                return self._send(200, json.dumps({"commands": res}))
            except PermissionError as e:
                return self._send(403, json.dumps({"error": str(e)}))
            except Exception as e:
                return self._send(400, json.dumps({"error": str(e)}))
        elif path_only == "/v1/audit/decisions":
            if not self._authorized():
                return self._send(401, '{"error":"unauthorized"}')
            try:
                client_id = qs.get("client_id", [""])[0]
                cluster_id = qs.get("cluster_id", [None])[0]
                res = get_control_plane().get_audit_decisions(client_id, cluster_id, tenant_header=tenant_hdr)
                return self._send(200, json.dumps({"decisions": res}))
            except PermissionError as e:
                return self._send(403, json.dumps({"error": str(e)}))
            except Exception as e:
                return self._send(400, json.dumps({"error": str(e)}))
        elif path_only == "/v1/ui/cast-dashboard":
            client_id = qs.get("client_id", ["default-tenant"])[0]
            cluster_id = qs.get("cluster_id", ["eks-prod-us-east-1"])[0]
            res = get_control_plane().get_cast_dashboard(client_id=client_id, cluster_id=cluster_id)
            return self._send(200, json.dumps(res))
        elif path_only == "/v1/cluster/topology":
            # ScaleOps Parity: Sample Cluster Topology & Bin-Packing State
            sample_topology = {
                "cluster_name": "eks-prod-us-east-1",
                "total_nodes": 6,
                "cluster_efficiency_pct": 74.2,
                "projected_efficiency_after_binpack_pct": 91.8,
                "monthly_waste_cost_usd": 1280.00,
                "nodes": [
                    {
                        "name": "ip-10-0-12-42.ec2.internal",
                        "instance_type": "m6i.2xlarge",
                        "zone": "us-east-1a",
                        "cpu_capacity_cores": 8,
                        "mem_capacity_gib": 32,
                        "cpu_utilization_pct": 82.5,
                        "mem_utilization_pct": 78.0,
                        "pods_count": 18,
                        "drain_candidate": False,
                    },
                    {
                        "name": "ip-10-0-12-88.ec2.internal",
                        "instance_type": "m6i.2xlarge",
                        "zone": "us-east-1a",
                        "cpu_capacity_cores": 8,
                        "mem_capacity_gib": 32,
                        "cpu_utilization_pct": 21.0,
                        "mem_utilization_pct": 19.5,
                        "pods_count": 3,
                        "drain_candidate": True,
                        "can_consolidate_to": "ip-10-0-12-42.ec2.internal",
                        "potential_monthly_savings_usd": 280.00,
                    },
                    {
                        "name": "ip-10-0-14-102.ec2.internal",
                        "instance_type": "c6i.4xlarge",
                        "zone": "us-east-1b",
                        "cpu_capacity_cores": 16,
                        "mem_capacity_gib": 32,
                        "cpu_utilization_pct": 88.0,
                        "mem_utilization_pct": 64.0,
                        "pods_count": 26,
                        "drain_candidate": False,
                    },
                    {
                        "name": "ip-10-0-14-109.ec2.internal",
                        "instance_type": "c6i.4xlarge",
                        "zone": "us-east-1b",
                        "cpu_capacity_cores": 16,
                        "mem_capacity_gib": 32,
                        "cpu_utilization_pct": 18.0,
                        "mem_utilization_pct": 14.0,
                        "pods_count": 2,
                        "drain_candidate": True,
                        "can_consolidate_to": "ip-10-0-14-102.ec2.internal",
                        "potential_monthly_savings_usd": 395.00,
                    },
                ],
            }
            self._send(200, json.dumps(sample_topology))
        else:
            # Check for static file in web directory
            web_dir = _get_web_dir()
            if web_dir:
                if path_only in ("/", ""):
                    target = os.path.join(web_dir, "index.html")
                elif path_only in ("/app", "/app.html", "/console"):
                    target = os.path.join(web_dir, "app.html")
                else:
                    clean_rel = os.path.normpath(path_only.lstrip("/\\"))
                    target = os.path.join(web_dir, clean_rel)

                target = os.path.abspath(target)
                # Prevent path traversal outside web_dir
                if target.startswith(web_dir) and os.path.isfile(target):
                    ext = os.path.splitext(target)[1].lower()
                    ctype = _MIME_TYPES.get(ext, "application/octet-stream")
                    if self._serve_file(target, ctype):
                        return

            self._send(404, '{"error":"not found"}')

    def do_POST(self):  # noqa: N802
        if self.path == "/v1/cluster/binpack":
            if not self._authorized():
                return self._send(401, '{"error":"unauthorized"}')
            try:
                n = int(self.headers.get("Content-Length", 0))
                payload = json.loads(self.rfile.read(n), parse_constant=_reject_constant)
                raw_nodes = payload.get("nodes", [])
                nodes = []
                for rn in raw_nodes:
                    pods = [
                        PodPlacement(
                            name=p["name"],
                            namespace=p.get("namespace", "default"),
                            cpu_request_millicores=int(p["cpu_request_millicores"]),
                            memory_request_mib=int(p["memory_request_mib"]),
                            can_evict=bool(p.get("can_evict", True)),
                        )
                        for p in rn.get("pods", [])
                    ]
                    nodes.append(
                        NodeState(
                            name=rn["name"],
                            instance_type=rn.get("instance_type", "m6i.xlarge"),
                            zone=rn.get("zone", "us-east-1a"),
                            monthly_cost_usd=float(rn.get("monthly_cost_usd", 140.0)),
                            total_cpu_millicores=int(rn["total_cpu_millicores"]),
                            total_memory_mib=int(rn["total_memory_mib"]),
                            allocated_cpu_millicores=int(rn.get("allocated_cpu_millicores", 0)),
                            allocated_memory_mib=int(rn.get("allocated_memory_mib", 0)),
                            pods=pods,
                        )
                    )
                bp = NodeBinPacker()
                res = bp.optimize_cluster(nodes)
                out = {
                    "cluster_efficiency_before_pct": res.cluster_efficiency_before_pct,
                    "cluster_efficiency_after_pct": res.cluster_efficiency_after_pct,
                    "total_nodes": res.total_nodes,
                    "nodes_can_drain": res.nodes_can_drain,
                    "monthly_savings_usd": res.monthly_savings_usd,
                    "relocation_plans": [
                        {
                            "pod_name": rp.pod_name,
                            "namespace": rp.namespace,
                            "from_node": rp.from_node,
                            "to_node": rp.to_node,
                            "cpu_millicores": rp.cpu_millicores,
                            "memory_mib": rp.memory_mib,
                        }
                        for rp in res.relocation_plans
                    ],
                    "reason": res.reason,
                }
                return self._send(200, json.dumps(out))
            except Exception as e:
                return self._send(400, json.dumps({"error": str(e)}))

        saas_post_routes = {
            "/v1/agents/register": get_control_plane().register_agent,
            "/v1/agents/heartbeat": get_control_plane().heartbeat,
            "/v1/telemetry/ingest": get_control_plane().ingest_telemetry,
            "/v1/commands/prewarm": get_control_plane().issue_prewarm_command,
            "/v1/commands/ack": get_control_plane().acknowledge_command,
            "/v1/audit/decisions": get_control_plane().record_audit_decision,
            "/v1/ui/recommendations/execute": get_control_plane().execute_ui_recommendation,
        }
        if self.path in saas_post_routes:
            if not self._authorized():
                return self._send(401, '{"error":"unauthorized"}')
            try:
                n = int(self.headers.get("Content-Length", 0))
                if n > 1 << 20:
                    return self._send(413, '{"error":"payload too large"}')
                payload = json.loads(self.rfile.read(n), parse_constant=_reject_constant)
                tenant_hdr = self.headers.get("X-Tenant-ID")
                res = saas_post_routes[self.path](payload, tenant_header=tenant_hdr)
                return self._send(200, json.dumps(res))
            except PermissionError as e:
                return self._send(403, json.dumps({"error": str(e)}))
            except (ValueError, KeyError, TypeError) as e:
                return self._send(400, json.dumps({"error": str(e)}))

        if self.path != "/v1/evaluate":
            return self._send(404, '{"error":"not found"}')
        if not self._authorized():
            return self._send(401, '{"error":"unauthorized"}')
        try:
            n = int(self.headers.get("Content-Length", 0))
            if n > 1 << 20:
                return self._send(413, '{"error":"payload too large"}')
            self._send(200, json.dumps(evaluate(json.loads(self.rfile.read(n), parse_constant=_reject_constant))))
        except (ValueError, KeyError, TypeError) as e:
            self._send(400, json.dumps({"error": str(e)}))

    def log_message(self, *args):  # keep stdout quiet
        pass


class HighConcurrencyHTTPServer(ThreadingHTTPServer):
    """High-performance HTTP server utilizing a warm thread pool and high-backlog listen queue."""
    request_queue_size = 2048
    daemon_threads = True
    allow_reuse_address = True

    def __init__(self, server_address, RequestHandlerClass, bind_and_activate=True, max_workers=None):
        if max_workers is None:
            max_workers = int(os.environ.get("MAX_WORKERS", "256"))
        self.executor = concurrent.futures.ThreadPoolExecutor(
            max_workers=max_workers, thread_name_prefix="engine-worker"
        )
        super().__init__(server_address, RequestHandlerClass, bind_and_activate=bind_and_activate)

    def process_request(self, request, client_address):
        """Submit request handling to the pre-warmed thread pool instead of spawning new OS threads."""
        self.executor.submit(self.process_request_thread, request, client_address)

    def server_bind(self):
        super().server_bind()
        try:
            import socket
            self.socket.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
        except (AttributeError, OSError):
            pass

    def server_close(self):
        super().server_close()
        self.executor.shutdown(wait=False)


def make_server(host: str = "0.0.0.0", port: int = 8000, max_workers: int | None = None) -> HighConcurrencyHTTPServer:
    """TLS is enabled with TLS_CERT/TLS_KEY; adding TLS_CLIENT_CA requires client certs (mTLS)."""
    srv = HighConcurrencyHTTPServer((host, port), Handler, max_workers=max_workers)
    cert, key = os.environ.get("TLS_CERT"), os.environ.get("TLS_KEY")
    if cert and key:
        ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        ctx.minimum_version = ssl.TLSVersion.TLSv1_2
        ctx.load_cert_chain(cert, key)
        ca = os.environ.get("TLS_CLIENT_CA")
        if ca:
            ctx.load_verify_locations(ca)
            ctx.verify_mode = ssl.CERT_REQUIRED
        srv.socket = ctx.wrap_socket(srv.socket, server_side=True)
    return srv


def main() -> None:
    srv = make_server(port=int(os.environ.get("PORT", "8000")))
    srv.serve_forever()


if __name__ == "__main__":
    main()
