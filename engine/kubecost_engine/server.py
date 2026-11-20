"""HTTP front-end and Multi-Tenant SaaS Control Plane API for KubeCost Auto-HPA.

Implements:
- Unified authentication across all `/v1/*` routes (`ENGINE_TOKEN`, `ENGINE_ADMIN_TOKEN`,
  and tenant-bound `kc_live_*` / `kch_live_*` API keys with expiration & revocation).
- Strict multi-tenant BOLA/IDOR authorization (`_verify_tenant_access`) where `X-Tenant-ID`
  is never trusted as standalone proof of identity.
- Distributed Redis Rate Limiting (`REDIS_URL`) & Daily Evaluation Quotas (`RATE_LIMIT_DAILY_EVAL_QUOTA`)
  returning `HTTP 429` with `Retry-After`, `X-RateLimit-Limit`, and `X-RateLimit-Remaining`.
- Centralized 1 MiB body limit (`_MAX_BODY_BYTES = 1 << 20`, `HTTP 413`), strict numeric/identifier
  validation (`math.isfinite`, range bounds), sanitized error responses, and HTTP security headers.
"""

from __future__ import annotations

import concurrent.futures
import hmac
import json
import logging
import math
import os
import signal
import ssl
import threading
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any, Optional

from .auth_billing import MultiTenantAuthManager, generate_stripe_billing_invoice
from .binpacking import NodeBinPacker, parse_nodes
from .enterprise import EnterpriseService
from .bootstrap import get_production_readiness_report, process_stripe_webhook_event
from .cast_suite import (
    evaluate_cluster_hibernation_and_rebalancing,
    evaluate_commitments_arbitrage,
    evaluate_k8s_security_posture,
    evaluate_network_and_storage_optimization,
    generate_cast_onboarding_bundle,
)
from .economics import cost_per_request_from_pod
from .gameday import run_autonomous_gameday
from .gpu_optimizer import evaluate_gpu_workload
from .greenops_guardrail import evaluate_finops_budget_guardrail, evaluate_greenops_carbon
from .guardrails import BudgetGuardrail
from .oom_shield import OOMProtectionShield
from .predictive import PredictiveAutoscaler
from .pricing import lookup_instance_pricing
from .saas import get_control_plane
from .scaling import recommend_memory, recommend_replicas, recommend_resize
from .security import (
    AuthContext,
    AuthenticationError,
    AuthorizationError,
    DistributedRateLimiter,
    InputValidationError,
    PayloadTooLargeError,
    RateLimitBackendUnavailableError,
    RateLimitDecision,
    StripeWebhookConfigError,
    _verify_tenant_access,
    extract_requested_tenant,
    is_production_mode,
    parse_body_float,
    parse_body_int,
    parse_query_float,
    parse_query_int,
    resolve_client_ip,
    validate_identifier,
)
from .spot import SpotCandidate, is_spot_eligible
from .stripe_checkout import KIND as CHECKOUT_KIND, StripeUnavailable, create_checkout, public_status

logger = logging.getLogger("kubecost_engine.server")

_MAX_BODY_BYTES = 1 << 20  # 1 MiB strict limit

_auth_manager = MultiTenantAuthManager(store=get_control_plane()._store)
_rate_limiter = DistributedRateLimiter(sqlite_store=get_control_plane()._store)
_draining = threading.Event()


def readiness():
    if _draining.is_set():
        return 503, {"status": "draining"}
    checks = (
        ("database", get_control_plane()._store.check_connection),
        ("redis", _rate_limiter.check_redis),
    )
    for name, check in checks:
        try:
            if check():
                continue
        except Exception as exc:
            logger.warning("Readiness check failed for %s: %s", name, type(exc).__name__)
        return 503, {"status": "unavailable", "dependency": name}
    return 200, {"status": "ok"}


class LocalReadinessHandler(BaseHTTPRequestHandler):
    def do_GET(self):  # noqa: N802
        if self.path != "/readyz":
            self.send_error(404)
            return
        code, payload = readiness()
        body = json.dumps(payload).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *_args):
        pass

_counters: dict[str, int] = {}
_lock = threading.Lock()

# Global numeric query parameters validated whenever present on any GET request
_GLOBAL_NUMERIC_QUERY_BOUNDS: dict[str, tuple[float, float]] = {
    "hourly_cost": (0.0, 1_000_000.0),
    "monthly_budget_usd": (0.01, 1_000_000_000.0),
    "current_spend_usd": (0.0, 1_000_000_000.0),
    "projected_spend_usd": (0.0, 1_000_000_000.0),
    "monthly_on_demand_usd": (0.0, 1_000_000_000.0),
    "mtd_spend_usd": (0.0, 1_000_000_000.0),
    "savings_usd": (0.0, 1_000_000_000.0),
    "sm_utilization_pct": (0.0, 100.0),
    "vram_used_gib": (0.0, 10_000.0),
    "tokens_per_second": (0.0, 10_000_000.0),
}


def get_auth_manager() -> MultiTenantAuthManager:
    return _auth_manager


def get_rate_limiter() -> DistributedRateLimiter:
    return _rate_limiter


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
    raise InputValidationError(f"invalid JSON constant {name}")


def _boolean(payload, key, default=False):
    value = payload.get(key, default)
    if type(value) is not bool:
        raise ValueError(f"{key} must be a boolean")
    return value


def evaluate(req: dict, *, tenant_id: str | None = None) -> dict:
    """Pure evaluation of one workload. Raises ValueError/KeyError on bad input."""
    if not isinstance(req, dict):
        raise ValueError("request must be a JSON object")
    for section in ("budget", "spot", "memory", "replicas", "predictive", "oom_protection"):
        if req.get(section) is not None and not isinstance(req[section], dict):
            raise ValueError(f"{section} must be an object")
    cpu = int(_num(req, "cpu_millicores"))
    if cpu <= 0 or cpu > 1_000_000:
        raise ValueError("cpu_millicores must be positive and within bounds")
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
        limit_val = _num(b, "monthly_limit")
        spent_val = _num(b, "spent", 0.0)
        g = BudgetGuardrail(limit_val, spent_val)
        frozen = g.frozen
        if not g.allows(action):
            action, new_cpu, reason = "hold", cpu, "budget frozen: growth blocked"

    spot = None
    if req.get("spot"):
        s = req["spot"]
        spot = is_spot_eligible(
            SpotCandidate(
                _boolean(s, "fault_tolerant"),
                int(_num(s, "replicas", 0)),
                _boolean(s, "has_pdb"),
                _boolean(s, "stateful"),
            ),
            int(_num(s, "min_replicas", 2)),
        )

    # Optional memory (in MiB) and replica recommendations.
    mem_out = None
    m = req.get("memory")
    if m:
        req_mib = int(_num(m, "request_mib"))
        use_mib = _num(m, "usage_mib")
        min_mib = int(_num(m, "min_mib", 64))
        max_mib = int(_num(m, "max_mib", 16384))
        md = recommend_memory(req_mib, use_mib, min_mib=min_mib, max_mib=max_mib)
        if md.action == "grow" or (md.action == "shrink" and not frozen):
            mem_out = {"action": md.action, "mib": md.cpu_millicores, "reason": md.reason}
        else:
            mem_out = {"action": "hold", "mib": req_mib, "reason": md.reason}
    rep_out = None
    r = req.get("replicas")
    if r:
        cur_rep = int(_num(r, "current"))
        min_rep = int(_num(r, "min"))
        max_rep = int(_num(r, "max"))
        target, why = recommend_replicas(
            cur_rep,
            min_rep,
            max_rep,
            d,
            cpu_usage_millicores=req.get("cpu_usage_millicores"),
            cpu_millicores=cpu,
            configured_min_millicores=int(req.get("min_millicores", 0)),
        )
        if frozen and target > cur_rep:
            target, why = cur_rep, "budget frozen: scale out blocked"
        rep_out = {"target": target, "reason": why}

    after = cost_per_request_from_pod(new_cpu / 1000, mem_gib, rps, cpu_price, mem_price)

    # ScaleOps Parity: Predictive Autoscaling forecast
    predictive_out = None
    if req.get("predictive"):
        p = req["predictive"]
        pa = PredictiveAutoscaler(
            min_replicas=int(r.get("min", 2)) if r else 2,
            max_replicas=int(r.get("max", 50)) if r else 50,
            warmup_lead_time_seconds=int(_num(p, "warmup_lead_time_seconds", 120)),
        )
        p_res = pa.forecast_demand(
            current_replicas=int(r["current"]) if r else 1,
            current_rps=rps,
            rps_history=p.get("rps_history"),
            target_rps_per_pod=float(_num(p, "target_rps_per_pod", 50.0)),
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
            critical_threshold=float(_num(oom_cfg, "critical_threshold", 0.88)),
            warning_threshold=float(_num(oom_cfg, "warning_threshold", 0.78)),
        )
        cur_limit = int(m.get("limit_mib") or m.get("max_mib", 4096)) if m else 4096
        cur_req = int(m.get("request_mib", 1024)) if m else 1024
        cur_usage = float(m.get("usage_mib", 512.0)) if m else 512.0
        oom_dec = shield.evaluate_memory_risk(
            current_usage_mib=cur_usage,
            current_request_mib=cur_req,
            current_limit_mib=cur_limit,
            usage_history_mib=oom_cfg.get("usage_history_mib"),
            max_allowed_limit_mib=int(_num(oom_cfg, "max_allowed_limit_mib", 16384)),
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

    _count(action)
    if os.environ.get("NEXUS_TELEMETRY_ENABLED", "").lower() in ("1", "true", "yes"):
        try:
            from .telemetry import get_telemetry_client

            get_telemetry_client().publish_scaling_decision(decision_res, request_context=req, tenant_id=tenant_id)
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
    return os.environ.get("ENGINE_TOKEN", "").strip()


def _admin_token() -> str:
    return os.environ.get("ENGINE_ADMIN_TOKEN", "").strip()


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


# GET endpoints whose answer comes from built-in example inputs, static reference tables or figures supplied in the query
# string: never from a connected cluster and never from a provider bill.
_SAMPLE_DATA_PATHS = frozenset(
    {
        "/v1/ui/cast-dashboard",
        "/v1/pricing/lookup",
        "/v1/billing/invoice",
        "/v1/gpu/optimize",
        "/v1/greenops/report",
        "/v1/gameday/simulate",
        "/v1/cluster/topology",
    }
)
SAMPLE_DATA_NOTICE = (
    "sample: computed from built-in example inputs, static reference tables or the figures in the request; "
    "not read from a connected cluster and not a provider bill"
)


def _is_sample_data_path(path_only: str) -> bool:
    return path_only in _SAMPLE_DATA_PATHS or path_only.startswith("/v1/cast/")


class Handler(BaseHTTPRequestHandler):
    def _extract_raw_credential(self) -> Optional[str]:
        """Extract Bearer token or X-API-Key header if provided."""
        for name in ("Authorization", "X-API-Key", "X-Tenant-ID"):
            if len(self.headers.get_all(name, [])) > 1:
                raise AuthenticationError("repeated identity headers are forbidden")
        if self.headers.get("Authorization") and self.headers.get("X-API-Key"):
            raise AuthenticationError("use a single credential header")
        auth_hdr = self.headers.get("Authorization", "").strip()
        if auth_hdr:
            if auth_hdr.lower().startswith("bearer "):
                return auth_hdr[7:].strip()
            return auth_hdr
        api_key_hdr = self.headers.get("X-API-Key", "").strip()
        if api_key_hdr:
            return api_key_hdr
        return None

    def _authenticate_request(self, required_scope: Optional[str] = None) -> AuthContext:
        """Unified authentication gate for all `/v1/*` endpoints.

        Supports:
        1. Tenant API Keys (`kc_live_*` / `kch_live_*`) validated via `MultiTenantAuthManager`
           with constant-time SHA-256 comparison, expiration, revocation, and tenant binding.
        2. `ENGINE_ADMIN_TOKEN` (global administrative credential).
        3. `ENGINE_TOKEN` (service/operator credential, optionally bound to `ENGINE_TOKEN_TENANT_ID`).
        4. Never falls back to unauthenticated demo mode when `is_production_mode()` is True or
           when `ENGINE_TOKEN` / `ENGINE_ADMIN_TOKEN` is configured.
        """
        raw_cred = self._extract_raw_credential()
        eng_tok = _token()
        adm_tok = _admin_token()
        prod_mode = is_production_mode()

        if raw_cred is not None:
            if not raw_cred:
                raise AuthenticationError("empty credential in Authorization header")

            # 1. Tenant API key (kc_live_* or kch_live_*)
            if raw_cred.startswith(("kc_live_", "kch_live_")):
                rec = _auth_manager.verify_api_key(raw_cred, required_scope=required_scope)
                is_adm = "admin:*" in rec.scopes
                return AuthContext(
                    authenticated=True,
                    principal=f"user:{rec.client_id}:{rec.user_id}" if rec.user_id else f"apikey:{rec.key_id}",
                    credential_id=rec.key_id,
                    authenticated_tenant=rec.client_id,
                    credential_type="api_key",
                    roles=("admin", "tenant") if is_adm else ("tenant",),
                    scopes=tuple(rec.scopes),
                    expires_at=rec.expires_at,
                    revoked=rec.revoked,
                )

            # 2. Global Admin Token
            if adm_tok and hmac.compare_digest(raw_cred.encode("utf-8"), adm_tok.encode("utf-8")):
                return AuthContext(
                    authenticated=True,
                    principal="system:engine-admin",
                    authenticated_tenant=None,
                    credential_type="admin_token",
                    roles=("admin",),
                    scopes=("admin:*",),
                )

            # 3. Engine Service Token
            if eng_tok and hmac.compare_digest(raw_cred.encode("utf-8"), eng_tok.encode("utf-8")):
                bound_tenant = os.environ.get("ENGINE_TOKEN_TENANT_ID", "").strip() or None
                role_cfg = os.environ.get("ENGINE_TOKEN_ROLE", "service").strip().lower()
                is_adm = role_cfg == "admin"
                return AuthContext(
                    authenticated=True,
                    principal="system:engine-service",
                    authenticated_tenant=bound_tenant,
                    credential_type="engine_token",
                    roles=("admin", "service") if is_adm else ("service",),
                    scopes=("admin:*",) if is_adm else ("service:*",),
                )

            # Any unrecognized or invalid credential is always rejected with 401
            raise AuthenticationError("invalid credentials")

        # No credential supplied
        if eng_tok or adm_tok or prod_mode:
            raise AuthenticationError("authentication required")

        # Local development / test mode (only when ENGINE_TOKEN is unset and not in production)
        return AuthContext(
            authenticated=False,
            principal="dev:local",
            authenticated_tenant=None,
            credential_type="anonymous_dev",
            roles=("dev",),
            scopes=("*",),
        )

    def _authorized(self) -> bool:
        """Backward-compatible boolean wrapper around `_authenticate_request()`."""
        try:
            self._authenticate_request()
            return True
        except AuthenticationError:
            return False

    def _client_ip(self) -> str:
        direct_ip = self.client_address[0] if self.client_address else "127.0.0.1"
        xff = self.headers.get("X-Forwarded-For")
        return resolve_client_ip(direct_ip, xff)

    def _apply_security_headers(self, ctype: str, is_api: bool = True) -> None:
        """Apply centralized HTTP security headers compatible with Nexus Client Portal."""
        self.send_header("X-Content-Type-Options", "nosniff")
        self.send_header("Referrer-Policy", "strict-origin-when-cross-origin")
        allowed_ancestors = os.environ.get("ALLOWED_FRAME_ANCESTORS", "").strip()
        if allowed_ancestors:
            self.send_header(
                "Content-Security-Policy",
                f"frame-ancestors 'self' {allowed_ancestors}",
            )
        elif is_api:
            self.send_header("X-Frame-Options", "DENY")
            self.send_header("Content-Security-Policy", "frame-ancestors 'none'")
        else:
            self.send_header("X-Frame-Options", "SAMEORIGIN")
            self.send_header("Content-Security-Policy", "frame-ancestors 'self'")
        if is_api:
            self.send_header("Cache-Control", "no-store")

    def _send(
        self,
        code: int,
        body: str | bytes,
        ctype: str = "application/json",
        extra_headers: Optional[dict[str, str]] = None,
    ) -> None:
        data = body.encode("utf-8") if isinstance(body, str) else body
        is_api = ctype.startswith("application/json")
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(data)))
        self.send_header("Access-Control-Allow-Origin", "*")
        self.send_header("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
        self.send_header("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Tenant-ID, X-API-Key")
        self._apply_security_headers(ctype, is_api=is_api)
        if extra_headers:
            for hk, hv in extra_headers.items():
                self.send_header(hk, hv)
        self.end_headers()
        self.wfile.write(data)

    def _write_json(
        self,
        code: int,
        payload: dict[str, Any],
        rl_decision: Optional[RateLimitDecision] = None,
        extra_headers: Optional[dict[str, str]] = None,
    ) -> None:
        headers: dict[str, str] = {}
        if rl_decision is not None:
            headers["X-RateLimit-Limit"] = str(rl_decision.limit)
            headers["X-RateLimit-Remaining"] = str(rl_decision.remaining)
        if extra_headers:
            headers.update(extra_headers)
        route = urllib.parse.urlparse(self.path).path
        if code == 200 and (route.startswith("/v1/cast/") or route in ("/v1/gameday/simulate", "/v1/gpu/optimize", "/v1/greenops/report", "/v1/billing/invoice", "/v1/pricing/lookup")):
            payload = {**payload, "evidence_kind": "model_or_simulation", "externally_validated": False}
        # Endpoints that answer from built-in example inputs or static tables say so, in a header and in the body, so a
        # console or a report can never present them as measurements of a connected cluster or as a provider bill.
        path_only = getattr(self, "path", "").split("?", 1)[0]
        if code == 200 and self.command == "GET" and _is_sample_data_path(path_only) and isinstance(payload, dict):
            headers["X-KubeCost-Data-Source"] = "sample"
            payload = {**payload, "data_source": SAMPLE_DATA_NOTICE}
        self._send(code, json.dumps(payload), "application/json", extra_headers=headers)

    def _write_rate_limited(self, rl_decision: RateLimitDecision) -> None:
        headers = {
            "Retry-After": str(max(1, rl_decision.retry_after_seconds)),
            "X-RateLimit-Limit": str(rl_decision.limit),
            "X-RateLimit-Remaining": "0",
        }
        self._write_json(
            429,
            {
                "error": "rate limit exceeded",
                "category": rl_decision.category,
                "quota_exceeded": rl_decision.quota_exceeded,
                "retry_after_seconds": max(1, rl_decision.retry_after_seconds),
            },
            extra_headers=headers,
        )

    def _read_raw_body(self, max_bytes: int = _MAX_BODY_BYTES) -> bytes:
        """Centralized bounded body reader enforcing `_MAX_BODY_BYTES = 1 << 20` (1 MiB)."""
        if self.headers.get("Transfer-Encoding") is not None:
            self.close_connection = True
            raise InputValidationError("Transfer-Encoding is not supported")
        if len(self.headers.get_all("Content-Length", [])) > 1:
            self.close_connection = True
            raise InputValidationError("repeated Content-Length headers are forbidden")
        raw_cl = self.headers.get("Content-Length")
        if raw_cl is None:
            raise InputValidationError("Content-Length header is required")
        raw_cl = raw_cl.strip()
        if not raw_cl.isascii() or not raw_cl.isdecimal():
            raise InputValidationError("invalid Content-Length header")
        if len(raw_cl) > 20:
            raise InputValidationError("invalid Content-Length header")
        n = int(raw_cl)
        if n > max_bytes:
            # Reject from headers without waiting for an attacker-controlled body.
            # Close the connection so unread bytes cannot become another request.
            self.close_connection = True
            raise PayloadTooLargeError("payload too large")
        if n == 0:
            raise InputValidationError("request body cannot be empty")
        data = self.rfile.read(n)
        if len(data) != n:
            self.close_connection = True
            raise InputValidationError("incomplete request body")
        return data

    def _read_json_body(self, max_bytes: int = _MAX_BODY_BYTES) -> dict[str, Any]:
        """Read and validate a JSON object body within the 1 MiB limit."""
        raw_bytes = self._read_raw_body(max_bytes=max_bytes)
        try:
            text = raw_bytes.decode("utf-8")
        except UnicodeDecodeError as exc:
            raise InputValidationError("request body must be valid UTF-8") from exc
        try:
            def unique_object(pairs):
                value = {}
                for key, item in pairs:
                    if key in value:
                        raise InputValidationError("duplicate JSON fields are forbidden")
                    value[key] = item
                return value

            parsed = json.loads(text, parse_constant=_reject_constant, object_pairs_hook=unique_object)
        except InputValidationError:
            raise
        except Exception as exc:
            raise InputValidationError("invalid JSON payload") from exc
        if not isinstance(parsed, dict):
            raise InputValidationError("request body must be a JSON object")
        return parsed

    def _validate_global_numeric_query_params(self, qs: dict[str, list[str]]) -> None:
        """Reject NaN, Infinity, or out-of-range numbers on any known numeric query parameter."""
        for key, (min_v, max_v) in _GLOBAL_NUMERIC_QUERY_BOUNDS.items():
            if key in qs:
                parse_query_float(qs, key, min_v, min_val=min_v, max_val=max_v)

    def do_OPTIONS(self):  # noqa: N802
        """CORS preflight handling."""
        self.send_response(204)
        self.send_header("Access-Control-Allow-Origin", "*")
        self.send_header("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
        self.send_header("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Tenant-ID, X-API-Key")
        self.send_header("Access-Control-Max-Age", "86400")
        self._apply_security_headers("text/plain", is_api=True)
        self.end_headers()

    def _handle_unsupported_method(self) -> None:
        parsed = urllib.parse.urlparse(self.path)
        if parsed.path.startswith("/v1/"):
            try:
                self._authenticate_request()
            except AuthenticationError:
                return self._write_json(401, {"error": "unauthorized"})
        self._write_json(405, {"error": "method not allowed"})

    def do_PUT(self):  # noqa: N802
        self._handle_unsupported_method()

    def do_DELETE(self):  # noqa: N802
        self._handle_unsupported_method()

    def do_PATCH(self):  # noqa: N802
        self._handle_unsupported_method()

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
        qs = urllib.parse.parse_qs(parsed.query, keep_blank_values=True)
        tenant_hdr = self.headers.get("X-Tenant-ID")

        # 1. Explicit Public Health & Readiness Probes
        if path_only == "/readyz":
            return self._write_json(*readiness())
        if path_only in ("/healthz", "/health"):
            return self._write_json(200, {"status": "ok"})

        # 2. Metrics endpoint: protected when ENGINE_TOKEN / production mode is active
        if path_only == "/metrics":
            if _token() or _admin_token() or is_production_mode():
                try:
                    self._authenticate_request()
                except AuthenticationError:
                    return self._write_json(401, {"error": "unauthorized"})
            return self._send(200, render_metrics(), "text/plain; version=0.0.4")

        # 3. Unified Security Gate for ALL /v1/* GET endpoints
        if path_only.startswith("/v1/"):
            try:
                auth_ctx = self._authenticate_request()
            except AuthenticationError:
                return self._write_json(401, {"error": "unauthorized"})
            except AuthorizationError:
                return self._write_json(403, {"error": "forbidden: access denied"})

            try:
                self._validate_global_numeric_query_params(qs)
                req_tenant = extract_requested_tenant(qs=qs)
                default_t = (
                    "org-prod-01"
                    if path_only == "/v1/finops/guardrail"
                    else (auth_ctx.authenticated_tenant or "default-tenant")
                )
                effective_tenant = _verify_tenant_access(
                    requested_tenant=req_tenant,
                    authenticated_tenant=auth_ctx.authenticated_tenant,
                    auth_ctx=auth_ctx,
                    provided_tenant_header=tenant_hdr,
                    default_tenant=default_t,
                )
            except AuthorizationError:
                return self._write_json(403, {"error": "forbidden: tenant isolation violation"})
            except InputValidationError as exc:
                return self._write_json(400, {"error": str(exc)})

            # Distributed Rate Limiting
            try:
                rl_dec = _rate_limiter.check_request(
                    path=path_only,
                    method="GET",
                    client_ip=self._client_ip(),
                    authenticated_tenant=effective_tenant,
                    principal=auth_ctx.principal,
                )
            except RateLimitBackendUnavailableError:
                return self._write_json(503, {"error": "service unavailable: rate limit backend unreachable"})

            if not rl_dec.allowed:
                return self._write_rate_limited(rl_dec)

            try:
                read_scopes = {
                    "/v1/audit/decisions": "audit:read",
                    "/v1/commands/pending": "commands:read",
                    "/v1/policies": "policies:read",
                    "/v1/telemetry/events": "telemetry:read",
                    "/v1/ui/cast-dashboard": "telemetry:read",
                    "/v1/billing/invoice": "billing:read",
                }
                scope = read_scopes.get(path_only)
                if scope and auth_ctx.credential_type == "api_key" and not auth_ctx.has_scope(scope):
                    raise AuthorizationError("missing required read scope")
                if path_only == "/v1/billing/checkout":
                    if not auth_ctx.authenticated or not auth_ctx.has_scope("billing:write"):
                        raise AuthorizationError("missing billing:write scope")
                    request_id = validate_identifier(qs.get("request_id", [""])[0], "request_id")
                    row = get_control_plane()._store.get_object(effective_tenant, CHECKOUT_KIND, request_id)
                    if row is None:
                        return self._write_json(404, {"error": "checkout not found"}, rl_decision=rl_dec)
                    return self._write_json(200, public_status(row), rl_decision=rl_dec)
                enterprise_reads = {"/v1/alerts": "alerts", "/v1/predictions/backtests": "predictions/backtests", "/v1/usage": "usage", "/v1/subscriptions": "subscriptions", "/v1/clusters": "clusters", "/v1/plans": "plans", "/v1/rightsizing": "rightsizing", "/v1/predictions": "predictions", "/v1/finops/costs": "costs", "/v1/finops/reconciliations": "reconciliations", "/v1/consolidation/plans": "consolidation/plans", "/v1/licenses": "licenses"}
                if path_only in enterprise_reads:
                    if auth_ctx.credential_type == "api_key" and not auth_ctx.has_scope("enterprise:read"):
                        raise AuthorizationError("missing enterprise:read scope")
                    result = EnterpriseService(get_control_plane()).get(enterprise_reads[path_only], effective_tenant, qs)
                    return self._write_json(200, result, rl_decision=rl_dec)
                if path_only == "/v1/users/me":
                    from .users import own_profile
                    result = own_profile(get_control_plane()._store, effective_tenant, auth_ctx)
                    return self._write_json(200, result, rl_decision=rl_dec)
                if path_only == "/v1/users":
                    if auth_ctx.credential_type == "api_key" and not auth_ctx.has_scope("users:manage"):
                        raise AuthorizationError("missing users:manage scope")
                    result = EnterpriseService._page(get_control_plane()._store.list_objects(effective_tenant, "user"), qs)
                    return self._write_json(200, result, rl_decision=rl_dec)
                if path_only == "/v1/openapi.json":
                    from .openapi import specification
                    return self._write_json(200, specification(), rl_decision=rl_dec)
                if path_only == "/v1/policies":
                    if req_tenant is None and not auth_ctx.authenticated_tenant:
                        raise InputValidationError("client_id is required")
                    cluster_id = validate_identifier(qs.get("cluster_id", ["eks-prod-1"])[0], "cluster_id")
                    res = get_control_plane().get_policy(
                        effective_tenant, cluster_id, tenant_header=effective_tenant
                    )
                    return self._write_json(200, res, rl_decision=rl_dec)

                if path_only == "/v1/commands/pending":
                    if req_tenant is None and not auth_ctx.authenticated_tenant:
                        raise InputValidationError("client_id is required")
                    cluster_id = validate_identifier(qs.get("cluster_id", ["eks-prod-1"])[0], "cluster_id")
                    res = get_control_plane().get_pending_commands(
                        effective_tenant, cluster_id, tenant_header=effective_tenant
                    )
                    return self._write_json(200, {"commands": res}, rl_decision=rl_dec)

                if path_only == "/v1/audit/decisions":
                    if req_tenant is None and not auth_ctx.authenticated_tenant:
                        raise InputValidationError("client_id is required")
                    raw_cluster = qs.get("cluster_id", [None])[0]
                    cluster_id = validate_identifier(raw_cluster, "cluster_id") if raw_cluster else None
                    raw_dec_id = qs.get("decision_id", [None])[0]
                    dec_id = validate_identifier(raw_dec_id, "decision_id") if raw_dec_id else None
                    res = get_control_plane().get_audit_decisions(
                        effective_tenant,
                        cluster_id,
                        tenant_header=effective_tenant,
                        decision_id=dec_id,
                    )
                    return self._write_json(200, {"decisions": res}, rl_decision=rl_dec)

                if path_only == "/v1/telemetry/events":
                    raw_cluster = qs.get("cluster_id", [None])[0]
                    cluster_id = validate_identifier(raw_cluster, "cluster_id") if raw_cluster else None
                    raw_evt = qs.get("event_id", [None])[0]
                    evt_id = validate_identifier(raw_evt, "event_id") if raw_evt else None
                    events = get_control_plane().get_telemetry_events(
                        effective_tenant,
                        cluster_id=cluster_id,
                        event_id=evt_id,
                        tenant_header=effective_tenant,
                    )
                    return self._write_json(200, {"events": events}, rl_decision=rl_dec)

                if path_only == "/v1/ui/cast-dashboard":
                    cluster_id = validate_identifier(
                        qs.get("cluster_id", ["eks-prod-us-east-1"])[0], "cluster_id"
                    )
                    res = get_control_plane().get_cast_dashboard(
                        client_id=effective_tenant,
                        cluster_id=cluster_id,
                        tenant_header=effective_tenant,
                    )
                    return self._write_json(200, res, rl_decision=rl_dec)

                if path_only == "/v1/pricing/lookup":
                    inst = validate_identifier(qs.get("instance_type", ["m6i.2xlarge"])[0], "instance_type")
                    reg = validate_identifier(qs.get("region", ["us-east-1"])[0], "region")
                    return self._write_json(200, lookup_instance_pricing(inst, reg), rl_decision=rl_dec)

                if path_only == "/v1/billing/invoice":
                    cluster_id = validate_identifier(
                        qs.get("cluster_id", ["eks-prod-us-east-1"])[0], "cluster_id"
                    )
                    get_control_plane().verify_cluster_access(
                        effective_tenant, cluster_id, tenant_header=effective_tenant
                    )
                    savings = parse_query_float(qs, "savings_usd", 3420.0, min_val=0.0, max_val=1e9)
                    nodes = parse_query_int(qs, "nodes", 6, min_val=1, max_val=100_000)
                    model = validate_identifier(qs.get("model", ["VALUE_SHARED"])[0], "model")
                    inv = generate_stripe_billing_invoice(
                        client_id=effective_tenant,
                        cluster_id=cluster_id,
                        verified_monthly_savings_usd=savings,
                        managed_nodes_count=nodes,
                        billing_model=model,
                    )
                    return self._write_json(200, inv, rl_decision=rl_dec)

                if path_only == "/v1/auth/keys":
                    keys = _auth_manager.list_client_keys(effective_tenant)
                    return self._write_json(200, {"keys": keys}, rl_decision=rl_dec)

                if path_only == "/v1/gpu/optimize":
                    workload = validate_identifier(
                        qs.get("workload", ["llm-gateway-vllm"])[0], "workload", allow_slash=True
                    )
                    gpu_model = validate_identifier(qs.get("gpu_model", ["A100-80GB"])[0], "gpu_model")
                    sm_pct = parse_query_float(qs, "sm_utilization_pct", 22.0, min_val=0.0, max_val=100.0)
                    vram_gib = parse_query_float(qs, "vram_used_gib", 13.5, min_val=0.0, max_val=10_000.0)
                    tokens_s = parse_query_float(qs, "tokens_per_second", 540.0, min_val=0.0, max_val=1e7)
                    plan = evaluate_gpu_workload(
                        workload=workload,
                        current_gpu_model=gpu_model,
                        sm_utilization_pct=sm_pct,
                        vram_used_gib=vram_gib,
                        tokens_per_second=tokens_s,
                    )
                    return self._write_json(200, plan.to_dict(), rl_decision=rl_dec)

                if path_only == "/v1/greenops/report":
                    reg = validate_identifier(qs.get("region", ["us-east-1"])[0], "region")
                    rep = evaluate_greenops_carbon(current_region=reg)
                    return self._write_json(200, rep.to_dict(), rl_decision=rl_dec)

                if path_only == "/v1/finops/guardrail":
                    budget = parse_query_float(qs, "monthly_budget_usd", 15000.0, min_val=0.01, max_val=1e9)
                    mtd = parse_query_float(qs, "mtd_spend_usd", 9200.0, min_val=0.0, max_val=1e9)
                    day = parse_query_int(qs, "day_of_month", 20, min_val=1, max_val=31)
                    dec = evaluate_finops_budget_guardrail(
                        tenant_id=effective_tenant,
                        monthly_budget_usd=budget,
                        mtd_spend_usd=mtd,
                        day_of_month=day,
                    )
                    return self._write_json(200, dec.to_dict(), rl_decision=rl_dec)

                if path_only == "/v1/gameday/simulate":
                    cluster_id = validate_identifier(qs.get("cluster_id", ["prod-us-east-1"])[0], "cluster_id")
                    get_control_plane().verify_cluster_access(
                        effective_tenant, cluster_id, tenant_header=effective_tenant
                    )
                    scorecard = run_autonomous_gameday(cluster_id=cluster_id)
                    return self._write_json(200, scorecard.to_dict(), rl_decision=rl_dec)

                if path_only == "/v1/system/readiness":
                    rep = get_production_readiness_report(auth_manager=_auth_manager)
                    return self._write_json(200, rep, rl_decision=rl_dec)

                if path_only == "/v1/cast/commitments":
                    provider = validate_identifier(qs.get("provider", ["aws"])[0], "provider")
                    req_cores = parse_query_int(qs, "requested_cores", 16, min_val=1, max_val=100_000)
                    return self._write_json(
                        200,
                        evaluate_commitments_arbitrage(
                            provider=provider, workload_requested_cores=req_cores
                        ),
                        rl_decision=rl_dec,
                    )

                if path_only == "/v1/cast/hibernation":
                    cluster_id = validate_identifier(
                        qs.get("cluster_id", ["eks-staging-us-east-1"])[0], "cluster_id"
                    )
                    get_control_plane().verify_cluster_access(
                        effective_tenant, cluster_id, tenant_header=effective_tenant
                    )
                    env = validate_identifier(qs.get("environment", ["staging"])[0], "environment")
                    hour = parse_query_int(qs, "hour_utc", 22, min_val=0, max_val=23)
                    return self._write_json(
                        200,
                        evaluate_cluster_hibernation_and_rebalancing(
                            cluster_id=cluster_id, environment=env, current_hour_utc=hour
                        ),
                        rl_decision=rl_dec,
                    )

                if path_only == "/v1/cast/network-storage":
                    return self._write_json(
                        200, evaluate_network_and_storage_optimization(), rl_decision=rl_dec
                    )

                if path_only == "/v1/cast/security":
                    cluster_id = validate_identifier(
                        qs.get("cluster_id", ["eks-prod-us-east-1"])[0], "cluster_id"
                    )
                    get_control_plane().verify_cluster_access(
                        effective_tenant, cluster_id, tenant_header=effective_tenant
                    )
                    return self._write_json(
                        200, evaluate_k8s_security_posture(cluster_id=cluster_id), rl_decision=rl_dec
                    )

                if path_only == "/v1/cast/onboarding":
                    provider = validate_identifier(qs.get("provider", ["eks"])[0], "provider")
                    cluster_name = validate_identifier(
                        qs.get("cluster_name", ["eks-prod-us-east-1"])[0], "cluster_name"
                    )
                    get_control_plane().verify_cluster_access(
                        effective_tenant, cluster_name, tenant_header=effective_tenant
                    )
                    return self._write_json(
                        200,
                        generate_cast_onboarding_bundle(provider=provider, cluster_name=cluster_name),
                        rl_decision=rl_dec,
                    )

                if path_only == "/v1/cast/full-suite":
                    return self._write_json(
                        200,
                        {
                            "commitments": evaluate_commitments_arbitrage(),
                            "hibernation": evaluate_cluster_hibernation_and_rebalancing(),
                            "network_and_storage": evaluate_network_and_storage_optimization(),
                            "security_posture": evaluate_k8s_security_posture(),
                            "onboarding": generate_cast_onboarding_bundle(),
                        },
                        rl_decision=rl_dec,
                    )

                if path_only == "/v1/cluster/topology":
                    cluster_id = validate_identifier(
                        qs.get("cluster_id", ["eks-prod-us-east-1"])[0], "cluster_id"
                    )
                    get_control_plane().verify_cluster_access(
                        effective_tenant, cluster_id, tenant_header=effective_tenant
                    )
                    sample_topology = {"cluster_id": cluster_id, "data_source": "not_collected", "nodes": [], "message": "No complete node scheduling inventory collected. Submit an explicit inventory to the planner."}
                    return self._write_json(200, sample_topology, rl_decision=rl_dec)

                return self._write_json(404, {"error": "not found"}, rl_decision=rl_dec)
            except AuthorizationError:
                return self._write_json(403, {"error": "forbidden: access denied"}, rl_decision=rl_dec)
            except PermissionError:
                return self._write_json(403, {"error": "forbidden: access denied"}, rl_decision=rl_dec)
            except InputValidationError as exc:
                return self._write_json(400, {"error": str(exc)}, rl_decision=rl_dec)
            except (ValueError, KeyError, TypeError):
                return self._write_json(400, {"error": "invalid request parameters"}, rl_decision=rl_dec)
            except Exception:
                logger.exception("Unhandled error in GET %s", path_only)
                return self._write_json(500, {"error": "internal server error"}, rl_decision=rl_dec)

        # 4. Static web console files (explicit public allowlist)
        web_dir = _get_web_dir()
        if web_dir:
            if path_only in ("/", ""):
                target = os.path.join(web_dir, "index.html")
            elif path_only in ("/app", "/app.html", "/console"):
                target = os.path.join(web_dir, "app.html")
            else:
                clean_rel = os.path.normpath(path_only.lstrip("/\\"))
                target = os.path.join(web_dir, clean_rel)

            # Prefix matching permits siblings such as web-private. Resolve
            # symlinks too, then compare complete path components.
            target = os.path.realpath(target)
            web_root = os.path.realpath(web_dir)
            try:
                contained = os.path.commonpath((web_root, target)) == web_root
            except ValueError:
                contained = False
            if contained and os.path.isfile(target):
                ext = os.path.splitext(target)[1].lower()
                relative_parts = os.path.relpath(target, web_root).replace("\\", "/").split("/")
                ctype = _MIME_TYPES.get(ext)
                if ctype and not any(part.startswith(".") for part in relative_parts) and self._serve_file(target, ctype):
                    return

        self._write_json(404, {"error": "not found"})

    def do_POST(self):  # noqa: N802
        parsed = urllib.parse.urlparse(self.path)
        path_only = parsed.path
        qs = urllib.parse.parse_qs(parsed.query, keep_blank_values=True)
        tenant_hdr = self.headers.get("X-Tenant-ID")

        # 1. Special Exception: Stripe Webhook (authenticated via Stripe-Signature HMAC-SHA256)
        if path_only == "/v1/billing/stripe-webhook":
            try:
                rl_dec = _rate_limiter.check_request(
                    path=path_only,
                    method="POST",
                    client_ip=self._client_ip(),
                    authenticated_tenant="stripe-webhook",
                )
            except RateLimitBackendUnavailableError:
                return self._write_json(503, {"error": "service unavailable: rate limit backend unreachable"})

            if not rl_dec.allowed:
                return self._write_rate_limited(rl_dec)

            try:
                raw_bytes = self._read_raw_body(max_bytes=_MAX_BODY_BYTES)
                raw_body = raw_bytes.decode("utf-8")
                sig_hdr = self.headers.get("Stripe-Signature", "")
                res = process_stripe_webhook_event(
                    raw_body=raw_body,
                    signature_header=sig_hdr,
                    store=get_control_plane()._store,
                )
                return self._write_json(200, res, rl_decision=rl_dec)
            except StripeWebhookConfigError:
                return self._write_json(
                    503,
                    {"error": "webhook processing unavailable: server configuration incomplete"},
                    rl_decision=rl_dec,
                )
            except PayloadTooLargeError:
                return self._write_json(413, {"error": "payload too large"}, rl_decision=rl_dec)
            except TimeoutError:
                self.close_connection = True
                return self._write_json(408, {"error": "request body timed out"}, rl_decision=rl_dec)
            except AuthenticationError:
                return self._write_json(401, {"error": "invalid Stripe-Signature"}, rl_decision=rl_dec)
            except PermissionError:
                return self._write_json(401, {"error": "invalid Stripe-Signature"}, rl_decision=rl_dec)
            except InputValidationError as exc:
                return self._write_json(400, {"error": str(exc)}, rl_decision=rl_dec)
            except Exception:
                logger.exception("Unhandled error in Stripe webhook")
                return self._write_json(400, {"error": "invalid webhook request"}, rl_decision=rl_dec)

        # 2. All other POST routes must be under /v1/* and pass unified authentication
        if not path_only.startswith("/v1/"):
            return self._write_json(404, {"error": "not found"})

        try:
            auth_ctx = self._authenticate_request()
        except AuthenticationError:
            return self._write_json(401, {"error": "unauthorized"})
        except AuthorizationError:
            return self._write_json(403, {"error": "forbidden: access denied"})

        # 3. Read & validate JSON body (strictly enforcing 1 MiB limit across ALL POST routes)
        try:
            payload = self._read_json_body(max_bytes=_MAX_BODY_BYTES)
        except PayloadTooLargeError:
            return self._write_json(413, {"error": "payload too large"})
        except TimeoutError:
            self.close_connection = True
            return self._write_json(408, {"error": "request body timed out"})
        except InputValidationError as exc:
            return self._write_json(400, {"error": str(exc)})

        # 4. Multi-Tenant BOLA/IDOR Authorization
        try:
            req_tenant = extract_requested_tenant(qs=qs, payload=payload)
            default_t = (
                "org-prod-01"
                if path_only == "/v1/finops/guardrail"
                else (auth_ctx.authenticated_tenant or "default-tenant")
            )
            effective_tenant = _verify_tenant_access(
                requested_tenant=req_tenant,
                authenticated_tenant=auth_ctx.authenticated_tenant,
                auth_ctx=auth_ctx,
                provided_tenant_header=tenant_hdr,
                default_tenant=default_t,
            )
        except AuthorizationError:
            return self._write_json(403, {"error": "forbidden: tenant isolation violation"})
        except InputValidationError as exc:
            return self._write_json(400, {"error": str(exc)})

        # 5. Distributed Rate Limiting & Daily Quotas
        try:
            rl_dec = _rate_limiter.check_request(
                path=path_only,
                method="POST",
                client_ip=self._client_ip(),
                authenticated_tenant=effective_tenant,
                principal=auth_ctx.principal,
            )
        except RateLimitBackendUnavailableError:
            return self._write_json(503, {"error": "service unavailable: rate limit backend unreachable"})

        if not rl_dec.allowed:
            return self._write_rate_limited(rl_dec)

        # 6. Route Dispatch
        try:
            if path_only == "/v1/billing/checkout":
                if not auth_ctx.authenticated or not auth_ctx.has_scope("billing:write"):
                    raise AuthorizationError("missing billing:write scope")
                try:
                    checkout = create_checkout(get_control_plane()._store, effective_tenant, payload)
                except (StripeWebhookConfigError, StripeUnavailable):
                    return self._write_json(503, {"error": "Stripe Checkout unavailable"}, rl_decision=rl_dec)
                return self._write_json(200, checkout, rl_decision=rl_dec)
            write_scopes = {"/v1/policies": "policies:write", "/v1/agents/register": "agents:write", "/v1/agents/heartbeat": "telemetry:write", "/v1/telemetry/ingest": "telemetry:write", "/v1/commands/prewarm": "commands:write", "/v1/commands/ack": "commands:ack", "/v1/audit/decisions": "audit:write", "/v1/cluster/binpack": "compute:evaluate", "/v1/evaluate": "compute:evaluate"}
            if auth_ctx.credential_type == "api_key" and path_only in write_scopes and not auth_ctx.has_scope(write_scopes[path_only]):
                raise AuthorizationError("missing required write scope")
            if path_only == "/v1/users/me/rotate":
                if (auth_ctx.credential_type != "api_key" or not auth_ctx.credential_id
                        or not auth_ctx.principal.startswith("user:") or auth_ctx.authenticated_tenant != effective_tenant):
                    raise AuthorizationError("a user-bound credential is required")
                if set(payload) - {"client_id", "tenant_id"}:
                    raise ValueError("rotation accepts only tenant parameters")
                result = _auth_manager.rotate_api_key(effective_tenant, auth_ctx.credential_id, preserve_expiry=True)
                return self._write_json(200, result, rl_decision=rl_dec)
            if path_only == "/v1/users/me":
                from .users import own_profile
                result = own_profile(get_control_plane()._store, effective_tenant, auth_ctx, payload)
                return self._write_json(200, result, rl_decision=rl_dec)
            if path_only in ("/v1/users", "/v1/users/update", "/v1/users/credentials"):
                from .users import mutate
                result = mutate(get_control_plane(), _auth_manager, path_only.removeprefix("/v1/"), effective_tenant, payload, auth_ctx)
                return self._write_json(200, result, rl_decision=rl_dec)
            enterprise_writes = {"/v1/maintenance/retention": "maintenance/retention", "/v1/alerts/ack": "alerts/ack", "/v1/licenses/revoke": "licenses/revoke", "/v1/predictions/backtest": "predictions/backtest", "/v1/subscriptions/sandbox": "subscriptions/sandbox", "/v1/rightsizing": "rightsizing", "/v1/predictions": "predictions", "/v1/predictions/observe": "predictions/observe", "/v1/finops/import": "finops/import", "/v1/finops/reconcile": "finops/reconcile", "/v1/licenses/activate": "licenses/activate", "/v1/consolidation/plans": "consolidation/plans", "/v1/consolidation/decision": "consolidation/decision", "/v1/consolidation/execution": "consolidation/execution"}
            if path_only in enterprise_writes:
                scope = "maintenance:write" if path_only.startswith("/v1/maintenance/") else "alerts:write" if path_only == "/v1/alerts/ack" else "licenses:manage" if path_only.startswith("/v1/licenses/") else "consolidation:execute" if path_only.endswith("/execution") else "consolidation:approve" if path_only.endswith("/decision") else "enterprise:write"
                requires_scope = scope.startswith("consolidation:") or auth_ctx.credential_type == "api_key"
                if requires_scope and not auth_ctx.has_scope(scope):
                    raise AuthorizationError("missing required enterprise scope")
                result = EnterpriseService(get_control_plane()).post(enterprise_writes[path_only], effective_tenant, payload, auth_ctx.principal)
                return self._write_json(200, result, rl_decision=rl_dec)
            if path_only == "/v1/cluster/binpack":
                if "cluster_id" in payload and payload["cluster_id"]:
                    cid = validate_identifier(str(payload["cluster_id"]), "cluster_id")
                    get_control_plane().verify_cluster_access(
                        effective_tenant, cid, tenant_header=effective_tenant
                    )
                nodes = parse_nodes(payload.get("nodes", []))
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
                return self._write_json(200, out, rl_decision=rl_dec)

            if path_only == "/v1/policies":
                res = get_control_plane().update_policy(payload, tenant_header=effective_tenant)
                return self._write_json(200, res, rl_decision=rl_dec)

            if path_only == "/v1/auth/keys/revoke":
                if auth_ctx.credential_type == "api_key" and not auth_ctx.has_scope("keys:manage"):
                    raise AuthorizationError("missing required RBAC scope: keys:manage")
                key_id = validate_identifier(str(payload.get("key_id", "")), "key_id")
                revoked = _auth_manager.revoke_api_key(effective_tenant, key_id)
                return self._write_json(
                    200,
                    {"client_id": effective_tenant, "key_id": key_id, "revoked": revoked},
                    rl_decision=rl_dec,
                )

            if path_only == "/v1/auth/keys":
                if auth_ctx.principal.startswith("user:"):
                    raise AuthorizationError("issue user-bound credentials through /v1/users/credentials")
                if auth_ctx.credential_type == "api_key" and not auth_ctx.has_scope("keys:manage"):
                    raise AuthorizationError("missing required RBAC scope: keys:manage")
                exp_days = parse_body_int(payload, "expires_in_days", 90, min_val=1, max_val=365)
                prefix_tag = str(payload.get("prefix_tag", "kch_live"))
                requested_scopes = payload.get("scopes")
                if requested_scopes is not None and (not isinstance(requested_scopes, list) or any(not isinstance(s, str) for s in requested_scopes)):
                    raise InputValidationError("scopes must be a list of strings")
                if auth_ctx.credential_type == "api_key":
                    if requested_scopes is None or any(not auth_ctx.has_scope(s) for s in requested_scopes):
                        raise AuthorizationError("cannot delegate scopes absent from the issuing key")
                res = _auth_manager.issue_api_key(
                    client_id=effective_tenant,
                    scopes=payload.get("scopes"),
                    expires_in_days=exp_days,
                    prefix_tag=prefix_tag,
                )
                return self._write_json(200, res, rl_decision=rl_dec)

            if path_only == "/v1/ui/recommendations/execute":
                if auth_ctx.credential_type == "api_key" and not auth_ctx.has_scope("recommendations:execute"):
                    raise AuthorizationError("missing required RBAC scope: recommendations:execute")
                res = get_control_plane().execute_ui_recommendation(
                    payload, tenant_header=effective_tenant
                )
                return self._write_json(200, res, rl_decision=rl_dec)

            saas_post_routes = {
                "/v1/agents/register": get_control_plane().register_agent,
                "/v1/agents/heartbeat": get_control_plane().heartbeat,
                "/v1/telemetry/ingest": get_control_plane().ingest_telemetry,
                "/v1/commands/prewarm": get_control_plane().issue_prewarm_command,
                "/v1/commands/ack": get_control_plane().acknowledge_command,
                "/v1/audit/decisions": get_control_plane().record_audit_decision,
                "/v1/gpu/optimize": lambda p, tenant_header=None: evaluate_gpu_workload(
                    workload=validate_identifier(
                        str(p.get("workload", "llm-gateway-vllm")), "workload", allow_slash=True
                    ),
                    current_gpu_model=validate_identifier(
                        str(p.get("current_gpu_model", "A100-80GB")), "gpu_model"
                    ),
                    requested_gpus=parse_body_int(p, "requested_gpus", 1, min_val=1, max_val=1024),
                    sm_utilization_pct=parse_body_float(
                        p, "sm_utilization_pct", 22.0, min_val=0.0, max_val=100.0
                    ),
                    vram_used_gib=parse_body_float(
                        p, "vram_used_gib", 13.5, min_val=0.0, max_val=10_000.0
                    ),
                    tokens_per_second=parse_body_float(
                        p, "tokens_per_second", 540.0, min_val=0.0, max_val=1e7
                    ),
                    inference_rps=parse_body_float(p, "inference_rps", 18.0, min_val=0.0, max_val=1e7),
                    spot_tolerant=_boolean(p, "spot_tolerant", True),
                    latency_p99_ms=parse_body_float(p, "latency_p99_ms", 185.0, min_val=0.0, max_val=1e6),
                    latency_slo_ms=parse_body_float(p, "latency_slo_ms", 350.0, min_val=0.1, max_val=1e6),
                ).to_dict(),
                "/v1/finops/guardrail": lambda p, tenant_header=None: evaluate_finops_budget_guardrail(
                    tenant_id=effective_tenant,
                    monthly_budget_usd=parse_body_float(
                        p, "monthly_budget_usd", 15000.0, min_val=0.01, max_val=1e9
                    ),
                    mtd_spend_usd=parse_body_float(p, "mtd_spend_usd", 9200.0, min_val=0.0, max_val=1e9),
                    day_of_month=parse_body_int(p, "day_of_month", 20, min_val=1, max_val=31),
                    days_in_month=parse_body_int(p, "days_in_month", 30, min_val=28, max_val=31),
                    slo_critical_breach=_boolean(p, "slo_critical_breach"),
                ).to_dict(),
                "/v1/gameday/simulate": lambda p, tenant_header=None: (
                    get_control_plane().verify_cluster_access(
                        effective_tenant,
                        validate_identifier(str(p.get("cluster_id", "prod-us-east-1")), "cluster_id"),
                        tenant_header=effective_tenant,
                    )
                    or run_autonomous_gameday(
                        cluster_id=validate_identifier(
                            str(p.get("cluster_id", "prod-us-east-1")), "cluster_id"
                        ),
                        baseline_rps=parse_body_float(
                            p, "baseline_rps", 840.0, min_val=0.0, max_val=1e7
                        ),
                        node_count=parse_body_int(p, "node_count", 12, min_val=1, max_val=100_000),
                    ).to_dict()
                ),
                "/v1/cast/commitments": lambda p, tenant_header=None: evaluate_commitments_arbitrage(
                    provider=validate_identifier(str(p.get("provider", "aws")), "provider"),
                    region=validate_identifier(str(p.get("region", "us-east-1")), "region"),
                    committed_cores=parse_body_int(p, "committed_cores", 64, min_val=0, max_val=1_000_000),
                    currently_used_cores=parse_body_int(
                        p, "currently_used_cores", 44, min_val=0, max_val=1_000_000
                    ),
                    hourly_commitment_usd=parse_body_float(
                        p, "hourly_commitment_usd", 1.92, min_val=0.0, max_val=1e7
                    ),
                    workload_requested_cores=parse_body_int(
                        p, "workload_requested_cores", 16, min_val=1, max_val=1_000_000
                    ),
                ),
                "/v1/cast/hibernation": lambda p, tenant_header=None: (
                    get_control_plane().verify_cluster_access(
                        effective_tenant,
                        validate_identifier(
                            str(p.get("cluster_id", "eks-staging-us-east-1")), "cluster_id"
                        ),
                        tenant_header=effective_tenant,
                    )
                    or evaluate_cluster_hibernation_and_rebalancing(
                        cluster_id=validate_identifier(
                            str(p.get("cluster_id", "eks-staging-us-east-1")), "cluster_id"
                        ),
                        environment=validate_identifier(
                            str(p.get("environment", "staging")), "environment"
                        ),
                        active_nodes=parse_body_int(p, "active_nodes", 10, min_val=1, max_val=100_000),
                        skeleton_nodes=parse_body_int(
                            p, "skeleton_nodes", 1, min_val=0, max_val=100_000
                        ),
                        current_hour_utc=parse_body_int(p, "current_hour_utc", 22, min_val=0, max_val=23),
                    )
                ),
                "/v1/cast/network-storage": lambda p, tenant_header=None: evaluate_network_and_storage_optimization(
                    cross_az_gb_per_day=parse_body_float(
                        p, "cross_az_gb_per_day", 420.0, min_val=0.0, max_val=1e9
                    ),
                    gp2_volumes_total_gib=parse_body_int(
                        p, "gp2_volumes_total_gib", 4000, min_val=0, max_val=100_000_000
                    ),
                    overprovisioned_pvc_gib=parse_body_int(
                        p, "overprovisioned_pvc_gib", 1800, min_val=0, max_val=100_000_000
                    ),
                    unattached_zombie_volumes_gib=parse_body_int(
                        p, "unattached_zombie_volumes_gib", 650, min_val=0, max_val=100_000_000
                    ),
                ),
            }

            if path_only in saas_post_routes:
                res = saas_post_routes[path_only](payload, tenant_header=effective_tenant)
                return self._write_json(200, res, rl_decision=rl_dec)

            if path_only == "/v1/evaluate":
                return self._write_json(200, evaluate(payload, tenant_id=effective_tenant), rl_decision=rl_dec)

            return self._write_json(404, {"error": "not found"}, rl_decision=rl_dec)
        except AuthorizationError:
            return self._write_json(403, {"error": "forbidden: access denied"}, rl_decision=rl_dec)
        except PermissionError:
            return self._write_json(403, {"error": "forbidden: access denied"}, rl_decision=rl_dec)
        except InputValidationError as exc:
            return self._write_json(400, {"error": str(exc)}, rl_decision=rl_dec)
        except (ValueError, KeyError, TypeError):
            return self._write_json(400, {"error": "invalid request payload"}, rl_decision=rl_dec)
        except Exception:
            logger.exception("Unhandled error in POST %s", path_only)
            return self._write_json(500, {"error": "internal server error"}, rl_decision=rl_dec)

    def log_message(self, *args):  # keep stdout quiet
        pass


class HighConcurrencyHTTPServer(ThreadingHTTPServer):
    """Bounded HTTP workers with idle I/O deadlines, including TLS handshakes."""

    request_queue_size = 2048
    daemon_threads = True
    allow_reuse_address = True

    def __init__(self, server_address, RequestHandlerClass, bind_and_activate=True, max_workers=None,
                 socket_timeout=None):
        if max_workers is None:
            max_workers = int(os.environ.get("MAX_WORKERS", "256"))
        if isinstance(max_workers, bool) or not isinstance(max_workers, int) or not 1 <= max_workers <= 1024:
            raise ValueError("MAX_WORKERS must be an integer in 1..1024")
        if socket_timeout is None:
            socket_timeout = float(os.environ.get("HTTP_SOCKET_TIMEOUT_SECONDS", "15"))
        if not math.isfinite(socket_timeout) or not 0.1 <= socket_timeout <= 300:
            raise ValueError("HTTP_SOCKET_TIMEOUT_SECONDS must be finite and in 0.1..300")
        self.socket_timeout = socket_timeout
        self.tls_context = None
        self.request_slots = threading.BoundedSemaphore(max_workers)
        self.executor = concurrent.futures.ThreadPoolExecutor(
            max_workers=max_workers, thread_name_prefix="engine-worker"
        )
        super().__init__(server_address, RequestHandlerClass, bind_and_activate=bind_and_activate)

    def process_request(self, request, client_address):
        # ThreadPoolExecutor has an unbounded internal queue. Reserve a slot
        # before submission so slow clients cannot accumulate accepted sockets.
        if not self.request_slots.acquire(blocking=False):
            if self.tls_context is None:
                try:
                    request.settimeout(0.1)
                    request.sendall(b"HTTP/1.0 503 Service Unavailable\r\nContent-Length: 0\r\n"
                                    b"Connection: close\r\nRetry-After: 1\r\n\r\n")
                except OSError:
                    pass
            self.shutdown_request(request)
            return
        try:
            self.executor.submit(self._process_bounded_request, request, client_address)
        except RuntimeError:
            self.request_slots.release()
            self.shutdown_request(request)

    def _process_bounded_request(self, request, client_address):
        try:
            if self.tls_context is not None:
                try:
                    request = self.tls_context.wrap_socket(request, server_side=True)
                except OSError:
                    self.shutdown_request(request)
                    return
            self.process_request_thread(request, client_address)
        finally:
            self.request_slots.release()

    def get_request(self):
        request, address = super().get_request()
        request.settimeout(self.socket_timeout)
        return request, address

    def server_bind(self):
        super().server_bind()
        try:
            import socket

            self.socket.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
        except (AttributeError, OSError):
            pass

    def server_close(self, *, wait_for_requests=False):
        worker = getattr(self, "retention_worker", None)
        if worker is not None:
            worker.stop()
        super().server_close()
        self.executor.shutdown(wait=wait_for_requests)


def make_server(
    host: str = "0.0.0.0", port: int = 8000, max_workers: int | None = None,
    socket_timeout: float | None = None,
) -> HighConcurrencyHTTPServer:
    """TLS is enabled with TLS_CERT/TLS_KEY; adding TLS_CLIENT_CA requires client certs (mTLS)."""
    from .maintenance import configured_worker
    cert, key = os.environ.get("TLS_CERT"), os.environ.get("TLS_KEY")
    ca = os.environ.get("TLS_CLIENT_CA")
    if bool(cert) != bool(key) or (ca and not cert):
        raise ValueError("TLS_CERT and TLS_KEY must be configured together; TLS_CLIENT_CA requires TLS")
    ctx = None
    if cert and key:
        ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        ctx.minimum_version = ssl.TLSVersion.TLSv1_2
        ctx.load_cert_chain(cert, key)
        if ca:
            ctx.load_verify_locations(ca)
            ctx.verify_mode = ssl.CERT_REQUIRED
    worker = configured_worker(get_control_plane())
    srv = HighConcurrencyHTTPServer((host, port), Handler, max_workers=max_workers,
                                    socket_timeout=socket_timeout)
    # Perform TLS in a bounded worker, not inside accept() on the listener.
    srv.tls_context = ctx
    srv.retention_worker = worker
    if worker is not None:
        worker.start()
    return srv


def main() -> None:
    srv = make_server(port=int(os.environ.get("PORT", "8000")))
    local_probe = None
    previous_handler = signal.getsignal(signal.SIGTERM)

    def terminate(_signum, _frame):
        if not _draining.is_set():
            _draining.set()
            threading.Thread(target=srv.shutdown, daemon=True).start()

    signal.signal(signal.SIGTERM, terminate)
    try:
        if port := os.environ.get("LOCAL_READINESS_PORT"):
            local_probe = ThreadingHTTPServer(("127.0.0.1", int(port)), LocalReadinessHandler)
            threading.Thread(target=local_probe.serve_forever, daemon=True).start()
        if os.environ.get("POSTGRES_STANDBY") == "true":
            threading.Thread(target=_exit_on_database_session_loss, daemon=True).start()
        srv.serve_forever()
    finally:
        _draining.set()
        if local_probe is not None:
            local_probe.shutdown()
            local_probe.server_close()
        srv.server_close(wait_for_requests=True)
        get_control_plane()._store.close()
        signal.signal(signal.SIGTERM, previous_handler)


def _exit_on_database_session_loss():
    store = get_control_plane()._store
    while not _draining.wait(2):
        try:
            store.check_connection()
        except Exception as exc:
            # A replacement process must reacquire the lock and reload durable state.
            logger.error("Database session failed: %s. Restarting engine.", type(exc).__name__)
            os._exit(1)


if __name__ == "__main__":
    main()
