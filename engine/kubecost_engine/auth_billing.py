"""Enterprise Multi-Tenant Authentication (API Keys + RBAC Scopes + Expiration/Rotation) & Stripe Commercial Billing."""

from __future__ import annotations

from dataclasses import dataclass
import hashlib
import hmac
import secrets
import threading
import time
from typing import Any, Optional

from .economics import calculate_commercial_billing
from .security import AuthenticationError, AuthorizationError, InputValidationError, validate_identifier

DEFAULT_KEY_EXPIRATION_DAYS = 90
MAX_KEY_EXPIRATION_DAYS = 365


@dataclass(frozen=True)
class TenantAPIKeyRecord:
    key_id: str
    client_id: str
    key_prefix: str
    key_sha256: str
    scopes: list[str]
    created_at: int
    expires_at: int = 0
    revoked: bool = False

    def is_expired(self, now_unix: Optional[int] = None) -> bool:
        now = int(now_unix if now_unix is not None else time.time())
        eff_exp = self.expires_at if self.expires_at > 0 else (self.created_at + DEFAULT_KEY_EXPIRATION_DAYS * 86400)
        return now >= eff_exp


class MultiTenantAuthManager:
    """Manages per-tenant API keys (`kc_live_*` / `kch_live_*`) with SHA-256 hashing,
    default 90-day expiration, revocation, rotation, RBAC scopes, and audit trail.
    """

    def __init__(self, store: Any = None) -> None:
        self._lock = threading.Lock()
        self._store = store
        self._keys_by_hash: dict[str, TenantAPIKeyRecord] = {}
        self._keys_by_client: dict[str, list[TenantAPIKeyRecord]] = {}
        self._audit_log: list[dict[str, Any]] = []
        if self._store is not None and hasattr(self._store, "load_all_api_keys"):
            self._load_from_store()

    def _load_from_store(self) -> None:
        for row in self._store.load_all_api_keys():
            created = int(row["created_at"])
            exp = int(row.get("expires_at") or 0)
            if exp <= 0:
                exp = created + DEFAULT_KEY_EXPIRATION_DAYS * 86400
            rec = TenantAPIKeyRecord(
                key_id=row["key_id"],
                client_id=row["client_id"],
                key_prefix=row["key_prefix"],
                key_sha256=row["key_sha256"],
                scopes=list(row["scopes"]),
                created_at=created,
                expires_at=exp,
                revoked=bool(row["revoked"]),
            )
            self._keys_by_hash[rec.key_sha256] = rec
            self._keys_by_client.setdefault(rec.client_id, []).append(rec)

    @staticmethod
    def _hash_key(raw_key: str) -> str:
        return hashlib.sha256(raw_key.encode("utf-8")).hexdigest()

    def _record_audit(self, event_type: str, client_id: str, key_id: str, detail: str = "") -> None:
        self._audit_log.append(
            {
                "timestamp": int(time.time()),
                "event_type": event_type,
                "client_id": client_id,
                "key_id": key_id,
                "detail": detail,
            }
        )
        if len(self._audit_log) > 5000:
            del self._audit_log[: len(self._audit_log) - 5000]

    def issue_api_key(
        self,
        client_id: str,
        scopes: list[str] | None = None,
        expires_in_days: int = DEFAULT_KEY_EXPIRATION_DAYS,
        created_at: Optional[int] = None,
        expires_at: Optional[int] = None,
        prefix_tag: str = "kch_live",
    ) -> dict[str, Any]:
        """Issue a new tenant-bound API key with default 90-day expiration.

        The plaintext `api_key` is returned ONLY once at creation time and is never stored.
        """
        if not client_id or not isinstance(client_id, str) or not client_id.strip():
            raise InputValidationError("client_id is required")
        clean_client = validate_identifier(client_id.strip(), "client_id")

        if expires_at is None:
            if isinstance(expires_in_days, bool) or not isinstance(expires_in_days, (int, float)):
                raise InputValidationError("expires_in_days must be a positive integer")
            days_int = int(expires_in_days)
            if days_int <= 0 or days_int > MAX_KEY_EXPIRATION_DAYS:
                raise InputValidationError(
                    f"expires_in_days must be between 1 and {MAX_KEY_EXPIRATION_DAYS}"
                )
        now = int(created_at if created_at is not None else time.time())
        resolved_expires_at = (
            int(expires_at)
            if expires_at is not None
            else now + int(expires_in_days) * 86400
        )

        resolved_scopes = list(
            scopes
            if scopes is not None
            else [
                "telemetry:write",
                "telemetry:read",
                "recommendations:execute",
                "billing:read",
                "compute:evaluate",
                "commands:read",
                "commands:write",
                "commands:ack",
                "audit:read",
                "audit:write",
                "policies:read",
            ]
        )
        tag = "kc_live" if prefix_tag == "kc_live" else "kch_live"
        raw_secret = f"{tag}_{clean_client}_{secrets.token_urlsafe(24)}"
        key_hash = self._hash_key(raw_secret)
        key_id = f"key-{secrets.token_hex(6)}"
        rec = TenantAPIKeyRecord(
            key_id=key_id,
            client_id=clean_client,
            key_prefix=raw_secret[:16],
            key_sha256=key_hash,
            scopes=resolved_scopes,
            created_at=now,
            expires_at=resolved_expires_at,
            revoked=False,
        )
        with self._lock:
            self._keys_by_hash[key_hash] = rec
            self._keys_by_client.setdefault(clean_client, []).append(rec)
            if self._store is not None and hasattr(self._store, "save_api_key"):
                self._store.save_api_key(
                    key_id=rec.key_id,
                    client_id=rec.client_id,
                    key_prefix=rec.key_prefix,
                    key_sha256=rec.key_sha256,
                    scopes=rec.scopes,
                    created_at=rec.created_at,
                    expires_at=rec.expires_at,
                    revoked=rec.revoked,
                )
            self._record_audit("KEY_ISSUED", clean_client, key_id, f"expires_at={resolved_expires_at}")

        return {
            "key_id": rec.key_id,
            "client_id": rec.client_id,
            "api_key": raw_secret,
            "key_prefix": rec.key_prefix,
            "scopes": list(rec.scopes),
            "created_at": rec.created_at,
            "expires_at": rec.expires_at,
            "expires_in_days": max(0, (rec.expires_at - rec.created_at) // 86400),
        }

    def verify_api_key(
        self,
        raw_key: str,
        required_scope: str | None = None,
        expected_client_id: str | None = None,
        now_unix: Optional[int] = None,
    ) -> TenantAPIKeyRecord:
        """Validate an API key (`kc_live_*` or `kch_live_*`) in constant time, checking revocation, expiry, tenant, and scope."""
        if not raw_key or not isinstance(raw_key, str) or not raw_key.strip():
            raise AuthenticationError("missing API key")
        cleaned = raw_key.strip()
        candidate_hash = self._hash_key(cleaned)

        with self._lock:
            matched_rec: Optional[TenantAPIKeyRecord] = None
            for stored_hash, rec in self._keys_by_hash.items():
                if hmac.compare_digest(stored_hash, candidate_hash):
                    matched_rec = rec
                    break

            if matched_rec is None:
                self._record_audit("KEY_VERIFY_FAILED", expected_client_id or "unknown", "unknown", "invalid_hash")
                raise AuthenticationError("invalid or revoked API key")

            if matched_rec.revoked:
                self._record_audit("KEY_VERIFY_FAILED", matched_rec.client_id, matched_rec.key_id, "revoked")
                raise AuthenticationError("invalid or revoked API key")

            if matched_rec.is_expired(now_unix=now_unix):
                self._record_audit("KEY_VERIFY_FAILED", matched_rec.client_id, matched_rec.key_id, "expired")
                raise AuthenticationError("expired API key")

            if expected_client_id and matched_rec.client_id != expected_client_id.strip():
                self._record_audit(
                    "KEY_CROSS_TENANT_BLOCKED",
                    matched_rec.client_id,
                    matched_rec.key_id,
                    f"attempted_target={expected_client_id}",
                )
                raise AuthorizationError(
                    f"cross-tenant key mismatch: key belongs to {matched_rec.client_id}, not {expected_client_id}"
                )

            if required_scope and required_scope not in matched_rec.scopes and "admin:*" not in matched_rec.scopes:
                self._record_audit(
                    "KEY_SCOPE_DENIED",
                    matched_rec.client_id,
                    matched_rec.key_id,
                    f"missing_scope={required_scope}",
                )
                raise AuthorizationError(f"missing required RBAC scope: {required_scope}")

            return matched_rec

    def revoke_api_key(self, client_id: str, key_id: str) -> bool:
        clean_client = validate_identifier(client_id.strip(), "client_id")
        clean_key_id = validate_identifier(key_id.strip(), "key_id")
        with self._lock:
            records = self._keys_by_client.get(clean_client, [])
            for idx, rec in enumerate(records):
                if rec.key_id == clean_key_id:
                    updated = TenantAPIKeyRecord(
                        key_id=rec.key_id,
                        client_id=rec.client_id,
                        key_prefix=rec.key_prefix,
                        key_sha256=rec.key_sha256,
                        scopes=rec.scopes,
                        created_at=rec.created_at,
                        expires_at=rec.expires_at,
                        revoked=True,
                    )
                    records[idx] = updated
                    self._keys_by_hash[rec.key_sha256] = updated
                    if self._store is not None and hasattr(self._store, "save_api_key"):
                        self._store.save_api_key(
                            key_id=updated.key_id,
                            client_id=updated.client_id,
                            key_prefix=updated.key_prefix,
                            key_sha256=updated.key_sha256,
                            scopes=updated.scopes,
                            created_at=updated.created_at,
                            expires_at=updated.expires_at,
                            revoked=True,
                        )
                    self._record_audit("KEY_REVOKED", clean_client, clean_key_id, "revoked_by_tenant")
                    return True
        return False

    def rotate_api_key(
        self,
        client_id: str,
        key_id: str,
        expires_in_days: int = DEFAULT_KEY_EXPIRATION_DAYS,
    ) -> dict[str, Any]:
        """Atomically revoke an existing key and issue a new replacement key with the same scopes."""
        clean_client = validate_identifier(client_id.strip(), "client_id")
        clean_key_id = validate_identifier(key_id.strip(), "key_id")
        with self._lock:
            records = self._keys_by_client.get(clean_client, [])
            target_rec: Optional[TenantAPIKeyRecord] = None
            for rec in records:
                if rec.key_id == clean_key_id:
                    target_rec = rec
                    break
            if target_rec is None:
                raise KeyError(f"key_id '{clean_key_id}' not found for tenant '{clean_client}'")
            scopes_copy = list(target_rec.scopes)

        self.revoke_api_key(clean_client, clean_key_id)
        new_issued = self.issue_api_key(
            client_id=clean_client,
            scopes=scopes_copy,
            expires_in_days=expires_in_days,
        )
        new_issued["rotated_from_key_id"] = clean_key_id
        return new_issued

    def list_client_keys(self, client_id: str, now_unix: Optional[int] = None) -> list[dict[str, Any]]:
        """Return metadata for a tenant's API keys without ever exposing raw key secrets or SHA-256 hashes."""
        clean_client = validate_identifier(client_id.strip(), "client_id")
        with self._lock:
            records = list(self._keys_by_client.get(clean_client, []))
        return [
            {
                "key_id": r.key_id,
                "client_id": r.client_id,
                "key_prefix": r.key_prefix,
                "scopes": list(r.scopes),
                "created_at": r.created_at,
                "expires_at": r.expires_at,
                "revoked": r.revoked,
                "expired": r.is_expired(now_unix=now_unix),
            }
            for r in records
        ]

    def get_audit_log(self, client_id: Optional[str] = None) -> list[dict[str, Any]]:
        with self._lock:
            if not client_id:
                return list(self._audit_log)
            return [e for e in self._audit_log if e.get("client_id") == client_id]


def generate_stripe_billing_invoice(
    client_id: str,
    cluster_id: str,
    verified_monthly_savings_usd: float,
    managed_nodes_count: int,
    billing_model: str = "VALUE_SHARED",
    share_percentage: float = 22.0,
    price_per_node_usd: float = 25.0,
) -> dict[str, Any]:
    """Build a Stripe-ready commercial invoice payload from verified counterfactual savings."""
    clean_client = validate_identifier(client_id, "client_id")
    clean_cluster = validate_identifier(cluster_id, "cluster_id")
    bill = calculate_commercial_billing(
        verified_monthly_net_savings_usd=verified_monthly_savings_usd,
        managed_nodes_count=managed_nodes_count,
        value_shared_percentage=share_percentage / 100.0 if share_percentage > 1.0 else share_percentage,
        fixed_price_per_node_month_usd=price_per_node_usd,
    )
    use_shared = billing_model.upper() == "VALUE_SHARED"
    mod_a = bill["model_a_value_shared"]
    mod_b = bill["model_b_fixed_per_node"]
    amount_usd = mod_a["platform_fee_usd"] if use_shared else mod_b["platform_fee_usd"]
    net_sav_usd = mod_a["customer_net_benefit_usd"] if use_shared else mod_b["customer_net_benefit_usd"]
    amount_cents = int(round(amount_usd * 100))

    return {
        "stripe_object": "invoice",
        "client_id": clean_client,
        "cluster_id": clean_cluster,
        "billing_model": "VALUE_SHARED_22PCT" if use_shared else "FIXED_NODE_SAAS",
        "currency": "usd",
        "amount_due_cents": amount_cents,
        "amount_due_usd": amount_usd,
        "customer_net_savings_usd": net_sav_usd,
        "customer_roi_multiple": round(verified_monthly_savings_usd / amount_usd, 2) if amount_usd > 0 else 0.0,
        "comparison": bill,
        "line_items": [
            {
                "description": (
                    f"Autonomous K8s Optimization — {share_percentage:.0f}% of ${verified_monthly_savings_usd:,.2f} verified savings"
                    if use_shared
                    else f"Autonomous K8s Optimization — {managed_nodes_count} nodes x ${price_per_node_usd:.2f}/node"
                ),
                "amount_cents": amount_cents,
            }
        ],
    }


def verify_stripe_webhook_signature(
    payload_bytes: bytes,
    sig_header: str = "",
    webhook_secret: str = "",
    now_unix: int | None = None,
    tolerance_seconds: int = 300,
    signature_header: str | None = None,
) -> bool:
    """Verify a Stripe-Signature header (t=...,v1=...) using HMAC-SHA256 with replay protection."""
    effective_sig = signature_header if signature_header is not None else sig_header
    if not effective_sig or not webhook_secret:
        return False
    parts = dict(item.strip().split("=", 1) for item in effective_sig.split(",") if "=" in item)
    ts_str = parts.get("t")
    v1_sig = parts.get("v1")
    if not ts_str or not v1_sig:
        return False
    try:
        ts = int(ts_str)
    except ValueError:
        return False
    now = now_unix if now_unix is not None else int(time.time())
    if abs(now - ts) > tolerance_seconds:
        return False
    signed_payload = f"{ts}.".encode("utf-8") + payload_bytes
    expected = hmac.new(webhook_secret.encode("utf-8"), signed_payload, hashlib.sha256).hexdigest()
    return hmac.compare_digest(expected, v1_sig)
