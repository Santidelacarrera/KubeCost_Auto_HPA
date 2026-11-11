"""Tests for Multi-Cloud Instance Pricing, Multi-Tenant API Keys, Stripe Billing & TimescaleDB DDL."""

import hashlib
import hmac
import time
import pytest

from kubecost_engine.auth_billing import (
    MultiTenantAuthManager,
    generate_stripe_billing_invoice,
    verify_stripe_webhook_signature,
)
from kubecost_engine.persistence import generate_timescaledb_migrations
from kubecost_engine.pricing import lookup_instance_pricing


def test_lookup_instance_pricing_aws_gcp_azure_and_regions():
    aws = lookup_instance_pricing("m6i.2xlarge", "us-east-1")
    assert aws["provider"] == "AWS"
    assert aws["vcpus"] == 8
    assert aws["spot_hourly_usd"] < aws["ondemand_hourly_usd"]
    assert aws["spot_discount_pct"] > 50.0

    aws_sa = lookup_instance_pricing("m6i.2xlarge", "sa-east-1")
    assert aws_sa["ondemand_hourly_usd"] > aws["ondemand_hourly_usd"]

    gcp = lookup_instance_pricing("n2-standard-8", "us-central1")
    assert gcp["provider"] == "GCP"

    azure = lookup_instance_pricing("Standard_D8s_v5", "eastus")
    assert azure["provider"] == "AZURE"


def test_multitenant_api_key_lifecycle_and_rbac_scopes():
    mgr = MultiTenantAuthManager()
    issued = mgr.issue_api_key("tenant-alpha", scopes=["telemetry:write", "billing:read"])
    raw_key = issued["api_key"]
    assert raw_key.startswith("kch_live_tenant-alpha_")

    rec = mgr.verify_api_key(raw_key, required_scope="telemetry:write", expected_client_id="tenant-alpha")
    assert rec.client_id == "tenant-alpha"

    # Cross-tenant isolation check
    with pytest.raises(PermissionError, match="cross-tenant"):
        mgr.verify_api_key(raw_key, expected_client_id="tenant-beta")

    # Missing scope check
    with pytest.raises(PermissionError, match="missing required RBAC scope"):
        mgr.verify_api_key(raw_key, required_scope="recommendations:execute")

    # Revocation check
    assert mgr.revoke_api_key("tenant-alpha", issued["key_id"]) is True
    with pytest.raises(PermissionError, match="invalid or revoked"):
        mgr.verify_api_key(raw_key)


def test_stripe_billing_invoice_and_webhook_hmac():
    inv_shared = generate_stripe_billing_invoice(
        client_id="tenant-alpha",
        cluster_id="eks-prod-1",
        verified_monthly_savings_usd=5000.0,
        managed_nodes_count=10,
        billing_model="VALUE_SHARED",
        share_percentage=22.0,
    )
    assert inv_shared["amount_due_usd"] == 1100.0
    assert inv_shared["amount_due_cents"] == 110000
    assert inv_shared["customer_net_savings_usd"] == 3900.0

    inv_fixed = generate_stripe_billing_invoice(
        client_id="tenant-alpha",
        cluster_id="eks-prod-1",
        verified_monthly_savings_usd=5000.0,
        managed_nodes_count=10,
        billing_model="FIXED_NODE",
        price_per_node_usd=25.0,
    )
    assert inv_fixed["amount_due_usd"] == 250.0
    assert inv_fixed["customer_net_savings_usd"] == 4750.0

    # Verify Stripe webhook signature
    secret = "whsec_test_secret_123"
    now = int(time.time())
    body = b'{"id":"evt_123","type":"invoice.paid"}'
    sig = hmac.new(secret.encode("utf-8"), f"{now}.".encode("utf-8") + body, hashlib.sha256).hexdigest()
    header = f"t={now},v1={sig}"
    assert verify_stripe_webhook_signature(body, header, secret, now_unix=now) is True
    assert verify_stripe_webhook_signature(body, header, "wrong_secret", now_unix=now) is False


def test_timescaledb_migrations_ddl():
    ddls = generate_timescaledb_migrations(retention_days=180)
    joined = "\n".join(ddls)
    assert "CREATE EXTENSION IF NOT EXISTS timescaledb" in joined
    assert "create_hypertable('telemetry_events'" in joined
    assert "INTERVAL '180 days'" in joined
    assert "tenant_api_keys" in joined
