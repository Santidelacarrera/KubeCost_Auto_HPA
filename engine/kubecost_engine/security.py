"""Enterprise Security, Multi-Tenant Authorization, Distributed Rate Limiting & Input Validation.

Implements the production security controls for KubeCost Auto-HPA:
1. Unified Authentication Context (`AuthContext`) supporting `ENGINE_TOKEN`, `ENGINE_ADMIN_TOKEN`,
   and SHA-256 tenant-bound API keys (`kc_live_*` / `kch_live_*`) with expiration and revocation.
2. Strict Multi-Tenant BOLA/IDOR Authorization (`_verify_tenant_access`) where `X-Tenant-ID`
   is never trusted as standalone identity.
3. Distributed Redis Rate Limiting (`REDIS_URL` with atomic Lua scripts) + Daily Evaluation Quotas
   (`RATE_LIMIT_DAILY_EVAL_QUOTA`) + explicit fail-closed policy in multi-replica production.
4. Centralized Input Validation (`_parse_query_float`, `_parse_query_int`, `_validate_identifier`)
   rejecting `NaN`, `Infinity`, out-of-range numbers, and malformed identifiers.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime, timezone
import hmac
import ipaddress
import logging
import math
import os
import re
import threading
import time
from typing import Any, Optional

try:
    import redis
except ImportError:
    redis = None  # type: ignore

logger = logging.getLogger("kubecost_engine.security")

# Identifier validation patterns
# Standard IDs: UUIDs, tenant slugs, cluster names, EC2 node DNS names (ip-10-0-12-88.ec2.internal)
_IDENTIFIER_RE = re.compile(r"^[a-zA-Z0-9_.:-]{1,128}$")
# Workload identifiers may include namespace/name (single slash, no path traversal)
_WORKLOAD_RE = re.compile(r"^[a-zA-Z0-9_.:-]+(?:/[a-zA-Z0-9_.:-]+)?$")

_ATOMIC_INCR_LUA = """
local current = redis.call("INCR", KEYS[1])
if current == 1 then
    redis.call("EXPIRE", KEYS[1], tonumber(ARGV[1]))
end
local ttl = redis.call("TTL", KEYS[1])
if ttl < 0 then
    redis.call("EXPIRE", KEYS[1], tonumber(ARGV[1]))
    ttl = tonumber(ARGV[1])
end
return {current, ttl}
"""


class AuthenticationError(PermissionError):
    """Raised when a request lacks valid credentials (maps to HTTP 401)."""


class AuthorizationError(PermissionError):
    """Raised when an authenticated principal is not authorized for a tenant or resource (maps to HTTP 403)."""


class InputValidationError(ValueError):
    """Raised when query parameters, identifiers, or request bodies fail validation (maps to HTTP 400)."""


class PayloadTooLargeError(ValueError):
    """Raised when a request body exceeds the maximum allowed byte size (maps to HTTP 413)."""


class RateLimitBackendUnavailableError(RuntimeError):
    """Raised when production requires distributed Redis rate limiting and Redis is unreachable (maps to HTTP 503)."""


class StripeWebhookConfigError(RuntimeError):
    """Raised when STRIPE_WEBHOOK_SECRET is missing on the server (maps to HTTP 503)."""


@dataclass(frozen=True)
class AuthContext:
    """Immutable authenticated principal and authorization context for a single HTTP request."""

    authenticated: bool
    principal: str
    authenticated_tenant: Optional[str]
    credential_type: str  # "api_key" | "engine_token" | "admin_token" | "anonymous_dev"
    roles: tuple[str, ...] = field(default_factory=tuple)
    scopes: tuple[str, ...] = field(default_factory=tuple)
    expires_at: Optional[int] = None
    revoked: bool = False

    @property
    def is_admin(self) -> bool:
        return "admin" in self.roles or "admin:*" in self.scopes

    def has_scope(self, scope: str) -> bool:
        if self.is_admin or "*" in self.scopes or "admin:*" in self.scopes:
            return True
        return scope in self.scopes


def is_production_mode() -> bool:
    """Return True when the server runs in production or strict auth mode."""
    env = (
        os.environ.get("KUBECOST_ENV")
        or os.environ.get("APP_ENV")
        or os.environ.get("ENVIRONMENT")
        or ""
    ).strip().lower()
    req_auth = os.environ.get("REQUIRE_AUTH", "").strip().lower()
    return env in ("production", "prod") or req_auth in ("1", "true", "yes")


def is_redis_required_in_production() -> bool:
    """Return True when distributed Redis rate limiting is mandatory (fail-closed if unavailable)."""
    explicit_req = os.environ.get("RATE_LIMIT_REQUIRE_REDIS", "").strip().lower()
    if explicit_req in ("1", "true", "yes"):
        return True
    if explicit_req in ("0", "false", "no"):
        return False
    standalone = os.environ.get("STANDALONE_SINGLE_REPLICA", "").strip().lower() in ("1", "true", "yes")
    return is_production_mode() and not standalone


def validate_identifier(
    value: Any,
    field_name: str = "identifier",
    allow_slash: bool = False,
    max_len: int = 128,
) -> str:
    """Validate tenant_id, client_id, cluster_id, workload, or resource IDs."""
    if not isinstance(value, str):
        raise InputValidationError(f"invalid {field_name}: must be a string")
    cleaned = value.strip()
    if not cleaned:
        raise InputValidationError(f"invalid {field_name}: cannot be empty")
    if len(cleaned) > max_len:
        raise InputValidationError(f"invalid {field_name}: exceeds maximum length of {max_len}")
    if ".." in cleaned or "\\" in cleaned:
        raise InputValidationError(f"invalid {field_name}: path traversal sequences are forbidden")
    pattern = _WORKLOAD_RE if allow_slash else _IDENTIFIER_RE
    if not pattern.match(cleaned):
        raise InputValidationError(f"invalid {field_name}: contains disallowed characters")
    return cleaned


def parse_query_float(
    qs: dict[str, list[str]],
    key: str,
    default: float,
    min_val: float = 0.0,
    max_val: float = 1e9,
) -> float:
    """Safely parse and validate a finite float from URL query parameters."""
    raw_list = qs.get(key)
    if not raw_list or raw_list[0] == "":
        return float(default)
    raw = raw_list[0].strip()
    try:
        val = float(raw)
    except (ValueError, TypeError) as exc:
        raise InputValidationError(f"invalid numeric parameter '{key}': must be a valid number") from exc
    if not math.isfinite(val):
        raise InputValidationError(f"invalid numeric parameter '{key}': NaN and Infinity are forbidden")
    if val < min_val or val > max_val:
        raise InputValidationError(
            f"parameter '{key}' out of allowed range [{min_val}, {max_val}]"
        )
    return val


def parse_query_int(
    qs: dict[str, list[str]],
    key: str,
    default: int,
    min_val: int = 0,
    max_val: int = 1_000_000,
) -> int:
    """Safely parse and validate a bounded integer from URL query parameters."""
    raw_list = qs.get(key)
    if not raw_list or raw_list[0] == "":
        return int(default)
    raw = raw_list[0].strip()
    try:
        if "." in raw or "e" in raw.lower():
            raise ValueError("not an integer")
        val = int(raw)
    except (ValueError, TypeError) as exc:
        raise InputValidationError(f"invalid integer parameter '{key}': must be an integer") from exc
    if val < min_val or val > max_val:
        raise InputValidationError(
            f"parameter '{key}' out of allowed range [{min_val}, {max_val}]"
        )
    return val


def parse_body_float(
    payload: dict[str, Any],
    key: str,
    default: float,
    min_val: float = 0.0,
    max_val: float = 1e9,
) -> float:
    """Safely parse and validate a finite float from a JSON request dictionary."""
    if key not in payload or payload[key] is None:
        return float(default)
    raw = payload[key]
    if isinstance(raw, bool) or not isinstance(raw, (int, float)):
        raise InputValidationError(f"invalid field '{key}': must be a finite number")
    val = float(raw)
    if not math.isfinite(val):
        raise InputValidationError(f"invalid field '{key}': NaN and Infinity are forbidden")
    if val < min_val or val > max_val:
        raise InputValidationError(f"field '{key}' out of allowed range [{min_val}, {max_val}]")
    return val


def parse_body_int(
    payload: dict[str, Any],
    key: str,
    default: int,
    min_val: int = 0,
    max_val: int = 1_000_000,
) -> int:
    """Safely parse and validate a bounded integer from a JSON request dictionary."""
    if key not in payload or payload[key] is None:
        return int(default)
    raw = payload[key]
    if isinstance(raw, bool) or not isinstance(raw, int):
        raise InputValidationError(f"invalid field '{key}': must be an integer")
    if raw < min_val or raw > max_val:
        raise InputValidationError(f"field '{key}' out of allowed range [{min_val}, {max_val}]")
    return int(raw)


def _verify_tenant_access(
    requested_tenant: Optional[str],
    authenticated_tenant: Optional[str] = None,
    auth_ctx: Optional[AuthContext] = None,
    provided_tenant_header: Optional[str] = None,
    default_tenant: str = "default-tenant",
) -> str:
    """Centralized multi-tenant authorization check preventing BOLA/IDOR.

    Rules:
    1. `X-Tenant-ID` is NEVER accepted as standalone proof of identity.
    2. When authenticated via a tenant API key (`kc_live_*` / `kch_live_*`),
       `auth_ctx.authenticated_tenant` is authoritative. Any mismatch with `requested_tenant`
       or `provided_tenant_header` raises `AuthorizationError` (HTTP 403).
    3. When `provided_tenant_header` is present, it must match `requested_tenant`.
    4. Returns the canonical validated tenant ID.
    """
    clean_req = requested_tenant.strip() if isinstance(requested_tenant, str) and requested_tenant.strip() else None
    clean_hdr = (
        provided_tenant_header.strip()
        if isinstance(provided_tenant_header, str) and provided_tenant_header.strip()
        else None
    )
    clean_auth = (
        authenticated_tenant.strip()
        if isinstance(authenticated_tenant, str) and authenticated_tenant.strip()
        else (auth_ctx.authenticated_tenant if auth_ctx else None)
    )

    if clean_req:
        clean_req = validate_identifier(clean_req, "tenant_id")
    if clean_hdr:
        clean_hdr = validate_identifier(clean_hdr, "X-Tenant-ID")

    # Rule 1: Consistency between X-Tenant-ID header and requested tenant parameter
    if clean_hdr and clean_req and clean_hdr != clean_req:
        raise AuthorizationError(
            "Tenant isolation violation: X-Tenant-ID header does not match requested tenant resource"
        )

    # Rule 2: If authenticated context is bound to a specific tenant (API key or tenant-scoped ENGINE_TOKEN)
    if clean_auth:
        if auth_ctx is None or not auth_ctx.is_admin:
            if clean_hdr and clean_hdr != clean_auth:
                raise AuthorizationError(
                    "Tenant isolation violation: X-Tenant-ID header does not match authenticated credential tenant"
                )
            if clean_req and clean_req != clean_auth:
                raise AuthorizationError(
                    "Tenant isolation violation: credential is not authorized for the requested tenant"
                )
        return clean_req or clean_auth

    # Rule 3: Service token or dev mode with X-Tenant-ID / requested_tenant
    if auth_ctx and auth_ctx.credential_type == "engine_token" and not auth_ctx.is_admin:
        # Non-admin service token: if accessing a specific non-default tenant when X-Tenant-ID is missing in strict mode
        if is_production_mode() and clean_req and clean_req != default_tenant and not clean_hdr:
            raise AuthorizationError(
                "Tenant isolation violation: service token requires explicit tenant context matching target tenant"
            )

    effective = clean_req or clean_hdr or default_tenant
    return validate_identifier(effective, "tenant_id")


def extract_requested_tenant(
    qs: Optional[dict[str, list[str]]] = None,
    payload: Optional[dict[str, Any]] = None,
) -> Optional[str]:
    """Extract `client_id` and/or `tenant_id` from query string and JSON body, rejecting contradictions."""
    candidates: list[str] = []
    if qs:
        for k in ("client_id", "tenant_id"):
            vals = qs.get(k)
            if vals and vals[0].strip():
                candidates.append(validate_identifier(vals[0].strip(), k))
    if isinstance(payload, dict):
        for k in ("client_id", "tenant_id"):
            v = payload.get(k)
            if v is not None and str(v).strip():
                candidates.append(validate_identifier(str(v).strip(), k))
    if not candidates:
        return None
    first = candidates[0]
    for c in candidates[1:]:
        if c != first:
            raise AuthorizationError("Conflicting client_id and tenant_id parameters in request")
    return first


def resolve_client_ip(direct_ip: str, forwarded_for_header: Optional[str] = None) -> str:
    """Resolve client IP safely, only trusting X-Forwarded-For when direct_ip is a configured trusted proxy."""
    trusted_raw = os.environ.get("TRUSTED_PROXIES", "").strip()
    if not trusted_raw or not forwarded_for_header:
        return direct_ip or "127.0.0.1"

    trusted_list = [p.strip() for p in trusted_raw.split(",") if p.strip()]
    is_trusted = False
    try:
        peer_addr = ipaddress.ip_address(direct_ip)
        for entry in trusted_list:
            if "/" in entry:
                if peer_addr in ipaddress.ip_network(entry, strict=False):
                    is_trusted = True
                    break
            elif peer_addr == ipaddress.ip_address(entry):
                is_trusted = True
                break
    except ValueError:
        is_trusted = direct_ip in trusted_list

    if not is_trusted:
        return direct_ip or "127.0.0.1"

    first_hop = forwarded_for_header.split(",")[0].strip()
    try:
        ipaddress.ip_address(first_hop)
        return first_hop
    except ValueError:
        return direct_ip or "127.0.0.1"


@dataclass(frozen=True)
class RateLimitDecision:
    allowed: bool
    limit: int
    remaining: int
    retry_after_seconds: int
    category: str
    quota_exceeded: bool = False


class DistributedRateLimiter:
    """Production rate limiter supporting atomic Redis counters (`REDIS_URL`) and SQLite/memory fallback."""

    def __init__(self, redis_url: Optional[str] = None, sqlite_store: Any = None) -> None:
        self._lock = threading.Lock()
        self._explicit_redis_url = redis_url
        self._redis_client: Any = None
        self._redis_url_cached: Optional[str] = None
        self._sqlite_store = sqlite_store
        # Local fallback state: key -> (count, expires_at_epoch)
        self._local_counters: dict[str, tuple[int, int]] = {}

    def reset_local(self) -> None:
        with self._lock:
            self._local_counters.clear()

    def _get_redis_client(self) -> Any:
        url = self._explicit_redis_url if self._explicit_redis_url is not None else os.environ.get("REDIS_URL", "").strip()
        if not url:
            if is_redis_required_in_production():
                raise RateLimitBackendUnavailableError(
                    "REDIS_URL is required for distributed rate limiting in production mode"
                )
            return None
        if redis is None:
            if is_redis_required_in_production():
                raise RateLimitBackendUnavailableError(
                    "redis package is required for distributed rate limiting in production mode"
                )
            return None
        if self._redis_client is not None and self._redis_url_cached == url:
            return self._redis_client
        try:
            self._redis_client = redis.Redis.from_url(
                url,
                decode_responses=True,
                socket_timeout=1.5,
                socket_connect_timeout=1.5,
            )
            self._redis_url_cached = url
            return self._redis_client
        except Exception as exc:
            logger.error("Failed to initialize Redis rate limiter client: %s", type(exc).__name__)
            if is_redis_required_in_production():
                raise RateLimitBackendUnavailableError(
                    "Distributed Redis rate limiter backend is unavailable"
                ) from exc
            return None

    def _increment_counter(self, key: str, window_seconds: int, now_ts: Optional[int] = None) -> tuple[int, int]:
        """Atomically increment `key` with TTL `window_seconds`, returning `(count, ttl_remaining)`."""
        now = int(now_ts if now_ts is not None else time.time())
        r_client = self._get_redis_client()
        if r_client is not None:
            try:
                res = r_client.eval(_ATOMIC_INCR_LUA, 1, key, int(window_seconds))
                return int(res[0]), max(1, int(res[1]))
            except Exception as exc:
                logger.error("Redis rate limiter operation failed: %s", type(exc).__name__)
                if is_redis_required_in_production():
                    raise RateLimitBackendUnavailableError(
                        "Distributed Redis rate limiter failed in production mode"
                    ) from exc

        # If SQLite store is attached (for persistent standalone quotas across restarts)
        if self._sqlite_store is not None and hasattr(self._sqlite_store, "increment_rate_counter"):
            try:
                return self._sqlite_store.increment_rate_counter(key, window_seconds, now)
            except Exception:
                pass

        with self._lock:
            existing = self._local_counters.get(key)
            if existing is None or existing[1] <= now:
                expires_at = now + int(window_seconds)
                self._local_counters[key] = (1, expires_at)
                return 1, int(window_seconds)
            count, expires_at = existing
            count += 1
            self._local_counters[key] = (count, expires_at)
            return count, max(1, expires_at - now)

    @staticmethod
    def classify_endpoint(path: str, method: str) -> str:
        """Classify request into rate-limit category."""
        m = method.upper()
        if path.startswith("/v1/auth/keys") and m == "POST":
            return "auth_keys"
        if path == "/v1/billing/stripe-webhook":
            return "stripe_webhook"
        if path in ("/v1/telemetry/ingest", "/v1/agents/heartbeat", "/v1/agents/register"):
            return "telemetry_ingest"
        if path in ("/v1/evaluate", "/v1/cluster/binpack") or (
            path in ("/v1/gpu/optimize", "/v1/gameday/simulate") and m == "POST"
        ):
            return "compute_intensive"
        return "general_read"

    @staticmethod
    def _limits_for_category(category: str) -> tuple[int, int]:
        """Return `(tenant_limit_per_min, ip_limit_per_min)` from environment or safe defaults."""
        if category == "auth_keys":
            lim = int(os.environ.get("RATE_LIMIT_AUTH_KEYS_PER_MIN", "10"))
            return lim, lim
        if category == "stripe_webhook":
            lim = int(os.environ.get("RATE_LIMIT_STRIPE_WEBHOOK_PER_MIN", "120"))
            return lim, lim
        if category == "telemetry_ingest":
            t_lim = int(os.environ.get("RATE_LIMIT_TELEMETRY_PER_MIN", "600"))
            ip_lim = int(os.environ.get("RATE_LIMIT_TELEMETRY_IP_PER_MIN", str(max(t_lim, 1200))))
            return t_lim, ip_lim
        if category == "compute_intensive":
            t_lim = int(os.environ.get("RATE_LIMIT_COMPUTE_PER_MIN", "60"))
            ip_lim = int(os.environ.get("RATE_LIMIT_COMPUTE_IP_PER_MIN", str(max(t_lim * 2, 120))))
            return t_lim, ip_lim
        t_lim = int(os.environ.get("RATE_LIMIT_GENERAL_PER_MIN", "120"))
        ip_lim = int(os.environ.get("RATE_LIMIT_GENERAL_IP_PER_MIN", str(max(t_lim * 2, 240))))
        return t_lim, ip_lim

    def check_request(
        self,
        path: str,
        method: str,
        client_ip: str,
        authenticated_tenant: Optional[str] = None,
        principal: Optional[str] = None,
        now_ts: Optional[int] = None,
    ) -> RateLimitDecision:
        """Evaluate per-IP, per-tenant per-minute limits, and daily evaluation quotas."""
        now = int(now_ts if now_ts is not None else time.time())
        minute_slot = now // 60
        category = self.classify_endpoint(path, method)
        tenant_limit, ip_limit = self._limits_for_category(category)
        tenant_id = authenticated_tenant or "anon"

        # 1. Per-IP check (strictly primary for auth_keys and stripe_webhook)
        ip_key = f"rl:ip:{category}:{client_ip}:{minute_slot}"
        ip_count, ip_ttl = self._increment_counter(ip_key, 60, now_ts=now)
        if ip_count > ip_limit:
            logger.warning(
                "rate_limit_exceeded scope=ip category=%s ip=%s limit=%d",
                category,
                client_ip,
                ip_limit,
            )
            return RateLimitDecision(
                allowed=False,
                limit=ip_limit,
                remaining=0,
                retry_after_seconds=ip_ttl,
                category=category,
                quota_exceeded=False,
            )

        # 2. Per-Tenant per-minute check
        if category not in ("stripe_webhook",):
            tenant_key = f"rl:tenant:{category}:{tenant_id}:{minute_slot}"
            t_count, t_ttl = self._increment_counter(tenant_key, 60, now_ts=now)
            if t_count > tenant_limit:
                logger.warning(
                    "rate_limit_exceeded scope=tenant category=%s tenant=%s limit=%d",
                    category,
                    tenant_id,
                    tenant_limit,
                )
                return RateLimitDecision(
                    allowed=False,
                    limit=tenant_limit,
                    remaining=0,
                    retry_after_seconds=t_ttl,
                    category=category,
                    quota_exceeded=False,
                )
        else:
            t_count = ip_count

        # 3. Daily Evaluation Quota check for compute_intensive endpoints
        if category == "compute_intensive":
            daily_quota = int(os.environ.get("RATE_LIMIT_DAILY_EVAL_QUOTA", "10000"))
            day_str = datetime.fromtimestamp(now, tz=timezone.utc).strftime("%Y-%m-%d")
            seconds_left_in_day = max(60, 86400 - (now % 86400))
            quota_key = f"rl:daily_eval:{tenant_id}:{day_str}"
            q_count, q_ttl = self._increment_counter(quota_key, seconds_left_in_day, now_ts=now)
            if q_count > daily_quota:
                logger.warning(
                    "daily_eval_quota_exceeded tenant=%s daily_quota=%d",
                    tenant_id,
                    daily_quota,
                )
                return RateLimitDecision(
                    allowed=False,
                    limit=daily_quota,
                    remaining=0,
                    retry_after_seconds=q_ttl,
                    category="daily_eval_quota",
                    quota_exceeded=True,
                )

        # 4. Daily Telemetry Quota check (separate from eval quota)
        if category == "telemetry_ingest":
            tel_daily_quota = int(os.environ.get("RATE_LIMIT_DAILY_TELEMETRY_QUOTA", "250000"))
            day_str = datetime.fromtimestamp(now, tz=timezone.utc).strftime("%Y-%m-%d")
            seconds_left_in_day = max(60, 86400 - (now % 86400))
            t_quota_key = f"rl:daily_telemetry:{tenant_id}:{day_str}"
            tq_count, tq_ttl = self._increment_counter(t_quota_key, seconds_left_in_day, now_ts=now)
            if tq_count > tel_daily_quota:
                logger.warning(
                    "daily_telemetry_quota_exceeded tenant=%s daily_quota=%d",
                    tenant_id,
                    tel_daily_quota,
                )
                return RateLimitDecision(
                    allowed=False,
                    limit=tel_daily_quota,
                    remaining=0,
                    retry_after_seconds=tq_ttl,
                    category="daily_telemetry_quota",
                    quota_exceeded=True,
                )

        eff_limit = ip_limit if category in ("auth_keys", "stripe_webhook") else tenant_limit
        eff_remaining = max(0, eff_limit - (ip_count if category in ("auth_keys", "stripe_webhook") else t_count))
        return RateLimitDecision(
            allowed=True,
            limit=eff_limit,
            remaining=eff_remaining,
            retry_after_seconds=0,
            category=category,
            quota_exceeded=False,
        )
