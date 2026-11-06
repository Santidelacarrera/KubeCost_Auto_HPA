"""Enterprise Multi-Tenant Authentication (API Keys + RBAC Scopes) & Stripe Commercial Billing."""

from dataclasses import asdict, dataclass
import hashlib
import hmac
import secrets
import time

from .economics import calculate_commercial_billing


@dataclass(frozen=True)
class TenantAPIKeyRecord:
    key_id: str
    client_id: str
    key_prefix: str
    key_sha256: str
    scopes: list[str]
    created_at: int
    revoked: bool = False


class MultiTenantAuthManager:
    """Manages per-tenant API keys with SHA-256 hashing and fine-grained RBAC scopes."""

    def __init__(self) -> None:
        self._keys_by_hash: dict[str, TenantAPIKeyRecord] = {}
        self._keys_by_client: dict[str, list[TenantAPIKeyRecord]] = {}

    @staticmethod
    def _hash_key(raw_key: str) -> str:
        return hashlib.sha256(raw_key.encode("utf-8")).hexdigest()

    def issue_api_key(
        self,
        client_id: str,
        scopes: list[str] | None = None,
    ) -> dict:
        if not client_id or not client_id.strip():
            raise ValueError("client_id is required")
        resolved_scopes = scopes or ["telemetry:write", "recommendations:execute", "billing:read"]
        raw_secret = f"kch_live_{client_id}_{secrets.token_urlsafe(18)}"
        key_hash = self._hash_key(raw_secret)
        rec = TenantAPIKeyRecord(
            key_id=f"key-{secrets.token_hex(4)}",
            client_id=client_id,
            key_prefix=raw_secret[:16],
            key_sha256=key_hash,
            scopes=list(resolved_scopes),
            created_at=int(time.time()),
            revoked=False,
        )
        self._keys_by_hash[key_hash] = rec
        self._keys_by_client.setdefault(client_id, []).append(rec)
        return {
            "key_id": rec.key_id,
            "client_id": rec.client_id,
            "api_key": raw_secret,
            "key_prefix": rec.key_prefix,
            "scopes": rec.scopes,
            "created_at": rec.created_at,
        }

    def verify_api_key(
        self,
        raw_key: str,
        required_scope: str | None = None,
        expected_client_id: str | None = None,
    ) -> TenantAPIKeyRecord:
        if not raw_key:
            raise PermissionError("missing API key")
        key_hash = self._hash_key(raw_key)
        rec = self._keys_by_hash.get(key_hash)
        if rec is None or rec.revoked:
            raise PermissionError("invalid or revoked API key")
        if expected_client_id and rec.client_id != expected_client_id:
            raise PermissionError(f"cross-tenant key mismatch: key belongs to {rec.client_id}, not {expected_client_id}")
        if required_scope and required_scope not in rec.scopes and "admin:*" not in rec.scopes:
            raise PermissionError(f"missing required RBAC scope: {required_scope}")
        return rec

    def revoke_api_key(self, client_id: str, key_id: str) -> bool:
        records = self._keys_by_client.get(client_id, [])
        for idx, rec in enumerate(records):
            if rec.key_id == key_id:
                updated = TenantAPIKeyRecord(
                    key_id=rec.key_id,
                    client_id=rec.client_id,
                    key_prefix=rec.key_prefix,
                    key_sha256=rec.key_sha256,
                    scopes=rec.scopes,
                    created_at=rec.created_at,
                    revoked=True,
                )
                records[idx] = updated
                self._keys_by_hash[rec.key_sha256] = updated
                return True
        return False

    def list_client_keys(self, client_id: str) -> list[dict]:
        return [
            {
                "key_id": r.key_id,
                "client_id": r.client_id,
                "key_prefix": r.key_prefix,
                "scopes": r.scopes,
                "created_at": r.created_at,
                "revoked": r.revoked,
            }
            for r in self._keys_by_client.get(client_id, [])
        ]


def generate_stripe_billing_invoice(
    client_id: str,
    cluster_id: str,
    verified_monthly_savings_usd: float,
    managed_nodes_count: int,
    billing_model: str = "VALUE_SHARED",
    share_percentage: float = 22.0,
    price_per_node_usd: float = 25.0,
) -> dict:
    """Build a Stripe-ready commercial invoice payload from verified counterfactual savings."""
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
        "client_id": client_id,
        "cluster_id": cluster_id,
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
    sig_header: str,
    webhook_secret: str,
    now_unix: int | None = None,
    tolerance_seconds: int = 300,
) -> bool:
    """Verify a Stripe-Signature header (t=...,v1=...) using HMAC-SHA256."""
    if not sig_header or not webhook_secret:
        return False
    parts = dict(item.split("=", 1) for item in sig_header.split(",") if "=" in item)
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
