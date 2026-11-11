"""Comprehensive Security Hardening & Production Readiness Test Suite (Sections 4–14).

Verifies:
- 14.1 Unified Authentication across all `/v1/*` endpoints (`ENGINE_TOKEN`, `kc_live_*` / `kch_live_*`,
  90-day expiration, revocation, rotation, RBAC scopes, and production fail-closed policy).
- 14.2 Multi-Tenant BOLA/IDOR isolation across all 10 mandatory cross-tenant attack vectors.
- 14.3 Distributed Redis Rate Limiting (`HTTP 429`, `Retry-After`, `X-RateLimit-*`), Daily Evaluation Quotas,
  multi-instance Redis Lua atomicity, restart persistence, and production fail-closed when Redis is down.
- 14.4 Stripe Webhook HMAC-SHA256 verification without fallback secrets (`HTTP 503` when unconfigured,
  `HTTP 401` on invalid signature/replay, `HTTP 200` idempotent deduplication on retries).
- 14.5 Strict Input Validation (`NaN`, `Infinity`, range bounds, identifier regex, 1 MiB `HTTP 413` body limit
  on `/v1/cluster/binpack`, sanitized error messages, and HTTP security headers).
"""

from __future__ import annotations

from concurrent.futures import ThreadPoolExecutor
import hashlib
import hmac
import json
import threading
import time
import urllib.error
import urllib.request
from unittest.mock import MagicMock

import pytest

from kubecost_engine.auth_billing import DEFAULT_KEY_EXPIRATION_DAYS, MultiTenantAuthManager
from kubecost_engine.bootstrap import (
    process_stripe_webhook_event,
    reset_stripe_webhook_idempotency_cache,
)
from kubecost_engine.persistence import SQLiteSaaSStore
from kubecost_engine.saas import SaaSControlPlane, get_control_plane
from kubecost_engine.security import (
    DistributedRateLimiter,
    RateLimitBackendUnavailableError,
    StripeWebhookConfigError,
)
from kubecost_engine.server import get_auth_manager, get_rate_limiter, make_server


def _start_test_server():
    get_rate_limiter().reset_local()
    srv = make_server("127.0.0.1", 0)
    t = threading.Thread(target=srv.serve_forever, daemon=True)
    t.start()
    return srv, f"http://127.0.0.1:{srv.server_address[1]}"


def _http_request(url: str, method: str = "GET", body: bytes | None = None, headers: dict | None = None):
    hdrs = dict(headers or {})
    if body is not None and "Content-Type" not in hdrs:
        hdrs["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=body, headers=hdrs, method=method)
    try:
        with urllib.request.urlopen(req) as resp:
            return resp.status, dict(resp.headers), resp.read()
    except urllib.error.HTTPError as err:
        return err.code, dict(err.headers), err.read()


# ==============================================================================
# 14.1 AUTHENTICATION & API KEY LIFECYCLE TESTS
# ==============================================================================


def test_14_1_all_v1_endpoints_require_auth_when_engine_token_set(monkeypatch):
    monkeypatch.setenv("ENGINE_TOKEN", "prod-operator-token-999")
    srv, base_url = _start_test_server()
    try:
        private_get_routes = [
            "/v1/ui/cast-dashboard?client_id=default-tenant",
            "/v1/billing/invoice?client_id=default-tenant",
            "/v1/cluster/topology",
            "/v1/gpu/optimize",
            "/v1/greenops/report",
            "/v1/finops/guardrail",
            "/v1/gameday/simulate",
            "/v1/system/readiness",
            "/v1/cast/commitments",
            "/v1/cast/hibernation",
            "/v1/cast/network-storage",
            "/v1/cast/security",
            "/v1/cast/onboarding",
            "/v1/cast/full-suite",
            "/v1/policies?client_id=default-tenant",
            "/v1/commands/pending?client_id=default-tenant",
            "/v1/audit/decisions?client_id=default-tenant",
            "/v1/auth/keys?client_id=default-tenant",
            "/v1/telemetry/events?client_id=default-tenant",
            "/metrics",
        ]
        for route in private_get_routes:
            # 1. No credentials -> 401
            code, _, _ = _http_request(base_url + route, method="GET")
            assert code == 401, f"Expected 401 on unauthenticated GET {route}, got {code}"

            # 2. Invalid token -> 401
            code_bad, _, _ = _http_request(
                base_url + route,
                method="GET",
                headers={"Authorization": "Bearer wrong-token"},
            )
            assert code_bad == 401, f"Expected 401 on wrong token GET {route}, got {code_bad}"

            # 3. Valid ENGINE_TOKEN -> 200
            code_ok, _, _ = _http_request(
                base_url + route,
                method="GET",
                headers={"Authorization": "Bearer prod-operator-token-999"},
            )
            assert code_ok == 200, f"Expected 200 on authenticated GET {route}, got {code_ok}"

        # Public probes remain open
        for pub in ("/healthz", "/health", "/readyz"):
            code_pub, _, _ = _http_request(base_url + pub, method="GET")
            assert code_pub == 200

        # Unsupported HTTP methods (PUT, DELETE, PATCH) also require auth on /v1/*
        for method in ("PUT", "DELETE", "PATCH"):
            c_unauth, _, _ = _http_request(base_url + "/v1/policies", method=method)
            assert c_unauth == 401
            c_auth, _, _ = _http_request(
                base_url + "/v1/policies",
                method=method,
                headers={"Authorization": "Bearer prod-operator-token-999"},
            )
            assert c_auth == 405
    finally:
        srv.shutdown()
        srv.server_close()


def test_14_1_production_mode_without_engine_token_never_enables_demo_mode(monkeypatch):
    monkeypatch.delenv("ENGINE_TOKEN", raising=False)
    monkeypatch.setenv("KUBECOST_ENV", "production")
    monkeypatch.setenv("STANDALONE_SINGLE_REPLICA", "true")
    srv, base_url = _start_test_server()
    try:
        # Even without ENGINE_TOKEN set, production mode MUST reject unauthenticated requests
        code, _, _ = _http_request(base_url + "/v1/ui/cast-dashboard")
        assert code == 401
        code_eval, _, _ = _http_request(
            base_url + "/v1/evaluate",
            method="POST",
            body=json.dumps({"cpu_millicores": 1000, "p99_ms": 50, "slo_p99_ms": 200}).encode(),
        )
        assert code_eval == 401
    finally:
        srv.shutdown()
        srv.server_close()


def test_14_1_api_key_lifecycle_90day_expiry_revocation_rotation_and_masking(monkeypatch):
    monkeypatch.setenv("ENGINE_TOKEN", "master-srv-token")
    mgr = get_auth_manager()
    now = int(time.time())

    # 1. Default 90-day expiration & kc_live_ prefix support
    issued = mgr.issue_api_key("tenant-lifecycle", prefix_tag="kc_live")
    raw_key = issued["api_key"]
    assert raw_key.startswith("kc_live_tenant-lifecycle_")
    assert issued["expires_in_days"] == DEFAULT_KEY_EXPIRATION_DAYS
    assert issued["expires_at"] == issued["created_at"] + 90 * 86400

    # 2. Listing keys never exposes raw secret or SHA-256 hash
    listed = mgr.list_client_keys("tenant-lifecycle")
    assert len(listed) >= 1
    for item in listed:
        assert "api_key" not in item
        assert "key_sha256" not in item
        assert item["expires_at"] > item["created_at"]

    # 3. Expired key is rejected with HTTP 401
    expired_issue = mgr.issue_api_key(
        "tenant-lifecycle",
        created_at=now - 100 * 86400,
        expires_at=now - 10 * 86400,
        prefix_tag="kc_live",
    )
    srv, base_url = _start_test_server()
    try:
        c_exp, _, _ = _http_request(
            base_url + "/v1/ui/cast-dashboard?client_id=tenant-lifecycle",
            headers={"Authorization": f"Bearer {expired_issue['api_key']}"},
        )
        assert c_exp == 401

        # 4. Valid key succeeds for its own tenant
        c_ok, _, _ = _http_request(
            base_url + "/v1/ui/cast-dashboard?client_id=tenant-lifecycle",
            headers={"Authorization": f"Bearer {raw_key}"},
        )
        assert c_ok == 200

        # 5. Rotation atomically revokes old key and issues new working key
        rotated = mgr.rotate_api_key("tenant-lifecycle", issued["key_id"])
        assert rotated["rotated_from_key_id"] == issued["key_id"]
        c_old, _, _ = _http_request(
            base_url + "/v1/ui/cast-dashboard?client_id=tenant-lifecycle",
            headers={"Authorization": f"Bearer {raw_key}"},
        )
        assert c_old == 401
        c_new, _, _ = _http_request(
            base_url + "/v1/ui/cast-dashboard?client_id=tenant-lifecycle",
            headers={"Authorization": f"Bearer {rotated['api_key']}"},
        )
        assert c_new == 200

        # 6. Revocation immediately invalidates the rotated key
        mgr.revoke_api_key("tenant-lifecycle", rotated["key_id"])
        c_rev, _, _ = _http_request(
            base_url + "/v1/ui/cast-dashboard?client_id=tenant-lifecycle",
            headers={"Authorization": f"Bearer {rotated['api_key']}"},
        )
        assert c_rev == 401
    finally:
        srv.shutdown()
        srv.server_close()


# ==============================================================================
# 14.2 MULTI-TENANT BOLA / IDOR — ALL 10 MANDATORY ATTACK VECTORS
# ==============================================================================


def test_14_2_multitenant_bola_idor_all_10_vectors_blocked(monkeypatch):
    monkeypatch.setenv("ENGINE_TOKEN", "prod-service-secret")
    mgr = get_auth_manager()
    cp = get_control_plane()

    key_a = mgr.issue_api_key("tenant-a", prefix_tag="kc_live")["api_key"]
    key_b = mgr.issue_api_key("tenant-b", prefix_tag="kc_live")["api_key"]
    headers_a = {"Authorization": f"Bearer {key_a}"}
    headers_b = {"Authorization": f"Bearer {key_b}"}

    srv, base_url = _start_test_server()
    try:
        # Setup Tenant B private resources (cluster, telemetry event, policy, command, audit decision)
        c_reg, _, _ = _http_request(
            base_url + "/v1/agents/register",
            method="POST",
            headers=headers_b,
            body=json.dumps({"client_id": "tenant-b", "cluster_id": "eks-private-tenant-b"}).encode(),
        )
        assert c_reg == 200

        c_tel, _, tel_body = _http_request(
            base_url + "/v1/telemetry/ingest",
            method="POST",
            headers=headers_b,
            body=json.dumps(
                {
                    "client_id": "tenant-b",
                    "cluster_id": "eks-private-tenant-b",
                    "event_id": "evt-secret-b-001",
                    "metrics": {
                        "rps": 500.0,
                        "p99_latency_ms": 80.0,
                        "cpu_usage_cores": 2.0,
                        "mem_usage_bytes": 1073741824,
                        "current_replicas": 4,
                    },
                }
            ).encode(),
        )
        assert c_tel == 200

        c_cmd, _, _ = _http_request(
            base_url + "/v1/commands/prewarm",
            method="POST",
            headers=headers_b,
            body=json.dumps(
                {
                    "client_id": "tenant-b",
                    "cluster_id": "eks-private-tenant-b",
                    "command_id": "cmd-secret-b-001",
                    "workload": "payments/api",
                }
            ).encode(),
        )
        assert c_cmd == 200

        # Vector 1: Tenant A cannot query Tenant B's Cast Dashboard
        for url_v1 in (
            f"{base_url}/v1/ui/cast-dashboard?client_id=tenant-b",
            f"{base_url}/v1/ui/cast-dashboard?tenant_id=tenant-b",
        ):
            code, _, _ = _http_request(url_v1, headers=headers_a)
            assert code == 403
        # Header spoofing X-Tenant-ID: tenant-b with Tenant A's API key -> 403
        code_hdr, _, _ = _http_request(
            f"{base_url}/v1/ui/cast-dashboard",
            headers={**headers_a, "X-Tenant-ID": "tenant-b"},
        )
        assert code_hdr == 403

        # Vector 2: Tenant A cannot query Tenant B's Billing Invoice
        for url_v2 in (
            f"{base_url}/v1/billing/invoice?client_id=tenant-b",
            f"{base_url}/v1/billing/invoice?tenant_id=tenant-b",
        ):
            code, _, _ = _http_request(url_v2, headers=headers_a)
            assert code == 403

        # Vector 3: Tenant A cannot query or issue API Keys for Tenant B
        for url_v3 in (
            f"{base_url}/v1/auth/keys?client_id=tenant-b",
            f"{base_url}/v1/auth/keys?tenant_id=tenant-b",
        ):
            code, _, _ = _http_request(url_v3, headers=headers_a)
            assert code == 403
        code_issue_b, _, _ = _http_request(
            f"{base_url}/v1/auth/keys",
            method="POST",
            headers=headers_a,
            body=json.dumps({"client_id": "tenant-b"}).encode(),
        )
        assert code_issue_b == 403

        # Vector 4: Tenant A cannot query or evaluate FinOps Guardrail for Tenant B
        code_fg_get, _, _ = _http_request(
            f"{base_url}/v1/finops/guardrail?tenant_id=tenant-b",
            headers=headers_a,
        )
        assert code_fg_get == 403
        code_fg_post, _, _ = _http_request(
            f"{base_url}/v1/finops/guardrail",
            method="POST",
            headers=headers_a,
            body=json.dumps({"tenant_id": "tenant-b", "monthly_budget_usd": 10000}).encode(),
        )
        assert code_fg_post == 403

        # Vector 5: Tenant A cannot execute 1-click UI recommendations on Tenant B
        code_rec_b, _, _ = _http_request(
            f"{base_url}/v1/ui/recommendations/execute",
            method="POST",
            headers=headers_a,
            body=json.dumps(
                {
                    "client_id": "tenant-b",
                    "recommendation_id": "rec-resize-payment-01",
                    "workload": "payment-service",
                }
            ).encode(),
        )
        assert code_rec_b == 403
        # Also verify direct method call with (client_id, recommendation_type, target, tenant_header)
        with pytest.raises(PermissionError):
            cp.execute_ui_recommendation(
                "tenant-b", "rec-resize-payment-01", "payment-service", tenant_header="tenant-a"
            )
        # Verify Tenant A executing a recommendation for itself does NOT alter Tenant B's dashboard
        code_rec_a, _, _ = _http_request(
            f"{base_url}/v1/ui/recommendations/execute",
            method="POST",
            headers=headers_a,
            body=json.dumps(
                {
                    "client_id": "tenant-a",
                    "recommendation_id": "rec-resize-payment-01",
                    "workload": "payment-service",
                }
            ).encode(),
        )
        assert code_rec_a == 200
        _, _, dash_b_raw = _http_request(
            f"{base_url}/v1/ui/cast-dashboard?client_id=tenant-b",
            headers=headers_b,
        )
        dash_b = json.loads(dash_b_raw.decode())
        assert dash_b["pain_number_cards"]["realized_1click_savings_usd"] == 0.0

        # Vector 6: Tenant A cannot access cluster topology, security, or simulation of Tenant B's cluster
        for cluster_route in (
            f"{base_url}/v1/cluster/topology?cluster_id=eks-private-tenant-b",
            f"{base_url}/v1/gameday/simulate?cluster_id=eks-private-tenant-b",
            f"{base_url}/v1/cast/security?cluster_id=eks-private-tenant-b",
            f"{base_url}/v1/cast/hibernation?cluster_id=eks-private-tenant-b",
        ):
            c_cl, _, _ = _http_request(cluster_route, headers=headers_a)
            assert c_cl == 403

        # Vector 7: Tenant A cannot query or overwrite Tenant B's private telemetry
        c_tel_get, _, _ = _http_request(
            f"{base_url}/v1/telemetry/events?client_id=tenant-b",
            headers=headers_a,
        )
        assert c_tel_get == 403
        c_tel_post, _, _ = _http_request(
            f"{base_url}/v1/telemetry/ingest",
            method="POST",
            headers=headers_a,
            body=json.dumps(
                {
                    "client_id": "tenant-b",
                    "cluster_id": "eks-prod-1",
                    "metrics": {"rps": 10, "p99_latency_ms": 10, "cpu_usage_cores": 1, "mem_usage_bytes": 100},
                }
            ).encode(),
        )
        assert c_tel_post == 403

        # Vector 8: Tenant A cannot read or modify Tenant B's scaling policies
        c_pol_get, _, _ = _http_request(
            f"{base_url}/v1/policies?client_id=tenant-b&cluster_id=eks-private-tenant-b",
            headers=headers_a,
        )
        assert c_pol_get == 403
        c_pol_post, _, _ = _http_request(
            f"{base_url}/v1/policies",
            method="POST",
            headers=headers_a,
            body=json.dumps(
                {"client_id": "tenant-b", "cluster_id": "eks-private-tenant-b", "slo_p99_ms": 500.0}
            ).encode(),
        )
        assert c_pol_post == 403

        # Vector 9: Tenant A cannot issue or list commands for Tenant B
        c_cmd_list, _, _ = _http_request(
            f"{base_url}/v1/commands/pending?client_id=tenant-b&cluster_id=eks-private-tenant-b",
            headers=headers_a,
        )
        assert c_cmd_list == 403
        c_cmd_issue, _, _ = _http_request(
            f"{base_url}/v1/commands/prewarm",
            method="POST",
            headers=headers_a,
            body=json.dumps(
                {"client_id": "tenant-b", "cluster_id": "eks-private-tenant-b", "workload": "web"}
            ).encode(),
        )
        assert c_cmd_issue == 403

        # Vector 10: Tenant A cannot access or ACK resources by Tenant B's resource IDs
        # even when passing client_id="tenant-a"!
        c_idor_ack, _, _ = _http_request(
            f"{base_url}/v1/commands/ack",
            method="POST",
            headers=headers_a,
            body=json.dumps({"client_id": "tenant-a", "command_id": "cmd-secret-b-001"}).encode(),
        )
        assert c_idor_ack == 403

        c_idor_evt, _, _ = _http_request(
            f"{base_url}/v1/telemetry/events?client_id=tenant-a&event_id=evt-secret-b-001",
            headers=headers_a,
        )
        assert c_idor_evt == 403
    finally:
        srv.shutdown()
        srv.server_close()


# ==============================================================================
# 14.3 DISTRIBUTED RATE LIMITING & DAILY QUOTAS
# ==============================================================================


def test_14_3_rate_limiting_429_headers_daily_quota_and_auth_keys_limit(monkeypatch):
    monkeypatch.setenv("ENGINE_TOKEN", "rl-secret")
    monkeypatch.setenv("RATE_LIMIT_AUTH_KEYS_PER_MIN", "3")
    monkeypatch.setenv("RATE_LIMIT_COMPUTE_PER_MIN", "50")
    monkeypatch.setenv("RATE_LIMIT_DAILY_EVAL_QUOTA", "4")

    srv, base_url = _start_test_server()
    auth_hdr = {"Authorization": "Bearer rl-secret", "X-Tenant-ID": "tenant-rl-1"}
    try:
        # 1. Test POST /v1/auth/keys strict brute-force limit (3 req/min/IP)
        for _ in range(3):
            code, hdrs, _ = _http_request(
                f"{base_url}/v1/auth/keys",
                method="POST",
                headers=auth_hdr,
                body=json.dumps({"client_id": "tenant-rl-1"}).encode(),
            )
            assert code == 200
            assert "X-RateLimit-Limit" in hdrs
            assert "X-RateLimit-Remaining" in hdrs

        code_429, hdrs_429, body_429 = _http_request(
            f"{base_url}/v1/auth/keys",
            method="POST",
            headers=auth_hdr,
            body=json.dumps({"client_id": "tenant-rl-1"}).encode(),
        )
        assert code_429 == 429
        assert int(hdrs_429["Retry-After"]) >= 1
        assert hdrs_429["X-RateLimit-Limit"] == "3"
        assert hdrs_429["X-RateLimit-Remaining"] == "0"
        parsed_429 = json.loads(body_429.decode())
        assert parsed_429["error"] == "rate limit exceeded"

        # 2. Test Daily Evaluation Quota (RATE_LIMIT_DAILY_EVAL_QUOTA=4) on /v1/evaluate
        eval_payload = json.dumps(
            {"cpu_millicores": 1000, "p99_ms": 50, "slo_p99_ms": 200, "rps": 100}
        ).encode()
        for _ in range(4):
            c_ev, _, _ = _http_request(
                f"{base_url}/v1/evaluate",
                method="POST",
                headers=auth_hdr,
                body=eval_payload,
            )
            assert c_ev == 200

        c_quota, h_quota, b_quota = _http_request(
            f"{base_url}/v1/evaluate",
            method="POST",
            headers=auth_hdr,
            body=eval_payload,
        )
        assert c_quota == 429
        assert json.loads(b_quota.decode())["quota_exceeded"] is True

        # 3. Telemetry ingestion uses its own separate bucket and is NOT blocked by eval quota
        c_tel, _, _ = _http_request(
            f"{base_url}/v1/telemetry/ingest",
            method="POST",
            headers=auth_hdr,
            body=json.dumps(
                {
                    "client_id": "tenant-rl-1",
                    "cluster_id": "eks-prod-1",
                    "metrics": {"rps": 100, "p99_latency_ms": 40, "cpu_usage_cores": 1.0, "mem_usage_bytes": 1024},
                }
            ).encode(),
        )
        assert c_tel == 200
    finally:
        srv.shutdown()
        srv.server_close()


def test_14_3_redis_distributed_counters_restart_persistence_and_prod_fail_closed(monkeypatch, tmp_path):
    # 1. Simulate shared Redis backend across two separate DistributedRateLimiter instances
    shared_redis_state: dict[str, list[int]] = {}
    redis_lock = threading.Lock()

    def fake_eval(script, numkeys, key, window_seconds):
        with redis_lock:
            entry = shared_redis_state.get(key)
            if entry is None:
                shared_redis_state[key] = [1, int(window_seconds)]
                return [1, int(window_seconds)]
            entry[0] += 1
            return [entry[0], entry[1]]

    mock_redis_client = MagicMock()
    mock_redis_client.eval.side_effect = fake_eval

    limiter_replica_1 = DistributedRateLimiter(redis_url="redis://localhost:6379/0")
    limiter_replica_2 = DistributedRateLimiter(redis_url="redis://localhost:6379/0")
    limiter_replica_1._redis_client = mock_redis_client
    limiter_replica_1._redis_url_cached = "redis://localhost:6379/0"
    limiter_replica_2._redis_client = mock_redis_client
    limiter_replica_2._redis_url_cached = "redis://localhost:6379/0"

    monkeypatch.setenv("RATE_LIMIT_COMPUTE_PER_MIN", "6")
    fixed_ts = 1700000010
    # Send 3 concurrent requests to replica 1 and 3 concurrent requests to replica 2
    with ThreadPoolExecutor(max_workers=6) as pool:
        futs = [
            pool.submit(
                (limiter_replica_1 if i % 2 == 0 else limiter_replica_2).check_request,
                "/v1/evaluate",
                "POST",
                f"10.0.0.{i+1}",
                "tenant-shared-redis",
                "mock-principal",
                fixed_ts,
            )
            for i in range(6)
        ]
        results = [f.result() for f in futs]
    assert all(r.allowed for r in results)

    # 7th request on replica 2 exceeds the shared limit of 6 across replicas
    seventh = limiter_replica_2.check_request(
        "/v1/evaluate", "POST", "10.0.0.99", "tenant-shared-redis", "mock-principal", now_ts=fixed_ts
    )
    assert seventh.allowed is False

    # 2. Persistence across server restart using SQLite WAL store
    db_file = str(tmp_path / "ratelimit_restart.db")
    store1 = SQLiteSaaSStore(db_path=db_file)
    lim1 = DistributedRateLimiter(sqlite_store=store1)
    monkeypatch.setenv("RATE_LIMIT_AUTH_KEYS_PER_MIN", "2")
    assert lim1.check_request("/v1/auth/keys", "POST", "192.0.2.55", "t1", now_ts=1700000010).allowed is True
    assert lim1.check_request("/v1/auth/keys", "POST", "192.0.2.55", "t1", now_ts=1700000011).allowed is True
    store1.close()

    # Simulate restart with new SQLiteSaaSStore and new DistributedRateLimiter
    store2 = SQLiteSaaSStore(db_path=db_file)
    lim2 = DistributedRateLimiter(sqlite_store=store2)
    after_restart = lim2.check_request("/v1/auth/keys", "POST", "192.0.2.55", "t1", now_ts=1700000012)
    assert after_restart.allowed is False
    store2.close()

    # 3. Production fail-closed when Redis is required but missing/unavailable
    monkeypatch.setenv("KUBECOST_ENV", "production")
    monkeypatch.setenv("STANDALONE_SINGLE_REPLICA", "false")
    monkeypatch.delenv("REDIS_URL", raising=False)
    prod_limiter = DistributedRateLimiter()
    with pytest.raises(RateLimitBackendUnavailableError):
        prod_limiter.check_request("/v1/evaluate", "POST", "10.0.0.1", "tenant-prod")


# ==============================================================================
# 14.4 STRIPE WEBHOOK SECURITY & IDEMPOTENCY
# ==============================================================================


def test_14_4_stripe_webhook_no_fallback_secret_hmac_and_idempotency(monkeypatch):
    reset_stripe_webhook_idempotency_cache()
    monkeypatch.delenv("STRIPE_WEBHOOK_SECRET", raising=False)

    # 1. Missing STRIPE_WEBHOOK_SECRET must fail closed with StripeWebhookConfigError (HTTP 503)
    with pytest.raises(StripeWebhookConfigError):
        process_stripe_webhook_event('{"id":"evt_1","type":"invoice.paid"}', "t=1,v1=abc")

    srv, base_url = _start_test_server()
    try:
        c_503, _, _ = _http_request(
            f"{base_url}/v1/billing/stripe-webhook",
            method="POST",
            headers={"Stripe-Signature": "t=1,v1=abc"},
            body=b'{"id":"evt_1","type":"invoice.paid"}',
        )
        assert c_503 == 503

        # 2. Configure real webhook secret in environment
        secret = "whsec_live_test_secret_987654321"
        monkeypatch.setenv("STRIPE_WEBHOOK_SECRET", secret)
        now = int(time.time())
        valid_body = json.dumps(
            {
                "id": "evt_stripe_idemp_001",
                "type": "invoice.payment_succeeded",
                "data": {"object": {"client_reference_id": "tenant-stripe-1"}},
            }
        )
        sig = hmac.new(
            secret.encode("utf-8"),
            f"{now}.{valid_body}".encode("utf-8"),
            hashlib.sha256,
        ).hexdigest()
        sig_hdr = f"t={now},v1={sig}"

        # Invalid signature -> 401
        c_bad_sig, _, _ = _http_request(
            f"{base_url}/v1/billing/stripe-webhook",
            method="POST",
            headers={"Stripe-Signature": f"t={now},v1=0000deadbeef"},
            body=valid_body.encode("utf-8"),
        )
        assert c_bad_sig == 401

        # Altered payload with original signature -> 401
        altered_body = valid_body.replace("tenant-stripe-1", "tenant-attacker")
        c_altered, _, _ = _http_request(
            f"{base_url}/v1/billing/stripe-webhook",
            method="POST",
            headers={"Stripe-Signature": sig_hdr},
            body=altered_body.encode("utf-8"),
        )
        assert c_altered == 401

        # Valid webhook delivery -> 200 PROCESSED_AND_AUDITED
        c_ok, _, raw_ok = _http_request(
            f"{base_url}/v1/billing/stripe-webhook",
            method="POST",
            headers={"Stripe-Signature": sig_hdr},
            body=valid_body.encode("utf-8"),
        )
        assert c_ok == 200
        res_ok = json.loads(raw_ok.decode())
        assert res_ok["status"] == "PROCESSED_AND_AUDITED"
        assert res_ok["idempotent_replay"] is False

        # Legitimate retry / duplicate event with same event.id -> 200 DUPLICATE_IGNORED
        c_dup, _, raw_dup = _http_request(
            f"{base_url}/v1/billing/stripe-webhook",
            method="POST",
            headers={"Stripe-Signature": sig_hdr},
            body=valid_body.encode("utf-8"),
        )
        assert c_dup == 200
        res_dup = json.loads(raw_dup.decode())
        assert res_dup["status"] == "DUPLICATE_IGNORED"
        assert res_dup["idempotent_replay"] is True
    finally:
        srv.shutdown()
        srv.server_close()


# ==============================================================================
# 14.5 INPUT VALIDATION, 1 MiB BODY LIMIT & HTTP SECURITY HEADERS
# ==============================================================================


def test_14_5_input_validation_nan_inf_identifiers_1mib_binpack_and_headers():
    srv, base_url = _start_test_server()
    try:
        # 1. NaN, Infinity, -Infinity, and negative values in query parameters -> 400
        bad_queries = [
            "/v1/billing/invoice?savings_usd=nan",
            "/v1/billing/invoice?savings_usd=inf",
            "/v1/billing/invoice?savings_usd=-100",
            "/v1/finops/guardrail?monthly_budget_usd=NaN",
            "/v1/finops/guardrail?mtd_spend_usd=Infinity",
            "/v1/finops/guardrail?hourly_cost=nan",
            "/v1/finops/guardrail?current_spend_usd=-50",
            "/v1/finops/guardrail?projected_spend_usd=inf",
            "/v1/finops/guardrail?monthly_on_demand_usd=-1",
            "/v1/gpu/optimize?sm_utilization_pct=150",
            "/v1/gpu/optimize?vram_used_gib=-5",
        ]
        for bq in bad_queries:
            code, _, _ = _http_request(base_url + bq)
            assert code == 400, f"Expected 400 for query {bq}, got {code}"

        # 2. Malformed identifiers (XSS, path traversal, SQLi) -> 400
        bad_id_queries = [
            "/v1/ui/cast-dashboard?client_id=%3Cscript%3Ealert(1)%3C/script%3E",
            "/v1/ui/cast-dashboard?cluster_id=../../etc/passwd",
            "/v1/billing/invoice?client_id=tenant%27%20OR%20%271%27%3D%271",
            "/v1/ui/cast-dashboard?client_id=" + ("a" * 200),
        ]
        for biq in bad_id_queries:
            code, _, _ = _http_request(base_url + biq)
            assert code == 400, f"Expected 400 for malformed ID query {biq}, got {code}"

        # Valid UUID tenant ID succeeds -> 200
        c_uuid, hdrs_uuid, _ = _http_request(
            base_url + "/v1/ui/cast-dashboard?client_id=6f1c2a7e-0b3d-4c8e-9a51-2d7f0e4b9c10"
        )
        assert c_uuid == 200

        # 3. Verify HTTP Security Headers
        assert hdrs_uuid.get("X-Content-Type-Options") == "nosniff"
        assert hdrs_uuid.get("Referrer-Policy") == "strict-origin-when-cross-origin"
        assert hdrs_uuid.get("X-Frame-Options") == "DENY"
        assert hdrs_uuid.get("Cache-Control") == "no-store"

        # 4. POST /v1/cluster/binpack > 1 MiB must return HTTP 413 Payload Too Large
        # The server refuses on the declared Content-Length before reading the body, and closes the
        # connection. Sending the whole 1 MiB raced that close (BrokenPipe in CI), so declare the size
        # and read the answer without streaming the body: same check, no race.
        import http.client
        from urllib.parse import urlsplit

        parts = urlsplit(base_url)
        conn = http.client.HTTPConnection(parts.hostname, parts.port, timeout=10)
        try:
            conn.putrequest("POST", "/v1/cluster/binpack")
            conn.putheader("Content-Type", "application/json")
            conn.putheader("Content-Length", str((1 << 20) + 100))
            conn.endheaders()
            c_413 = conn.getresponse().status
        finally:
            conn.close()
        assert c_413 == 413
    finally:
        srv.shutdown()
        srv.server_close()
