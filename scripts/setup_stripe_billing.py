#!/usr/bin/env python3
"""
KubeCost Auto-HPA - Automated Stripe Billing Setup
--------------------------------------------------
Provisions Stripe Products, Prices (per-node monthly & annual), and Checkout
links for the KubeCost Auto-HPA commercial tiers.

Supports both direct Stripe REST API execution (via standard library urllib)
and offline simulation / dry-run mode for CI/CD and air-gapped environments.

Usage:
    # Dry-run simulation (no network calls, validates configuration):
    python scripts/setup_stripe_billing.py --dry-run

    # Live or Test API execution using Stripe secret key:
    export STRIPE_API_KEY="sk_test_..."
    python scripts/setup_stripe_billing.py --live

    # Export configuration to JSON for front-end consumption:
    python scripts/setup_stripe_billing.py --dry-run --output web/stripe-config.json
"""

import argparse
import base64
import json
import os
import sys
import urllib.parse
import urllib.request
from typing import Any, Dict, Optional

TIERS_SPECIFICATION = [
    {
        "id": "growth",
        "name": "KubeCost Auto-HPA Growth Tier",
        "description": "Autonomous unit-economics autoscaling for growth teams. Unlimited clusters, spot bidding optimizer, Slack & PagerDuty webhooks.",
        "monthly_cents_per_node": 1800,  # $18.00 / node / month
        "annual_cents_per_node": 17200,  # $172.00 / node / year (~20% discount)
        "features": [
            "Autonomous Downscale Stabilization (Zero Outages)",
            "Spot Instance Cost-Bidding Engine",
            "Multi-Cluster Web Console (Neo Kinpaku UI)",
            "Slack & PagerDuty Webhook Integrations",
            "Standard SLA (Business Hours Email Support)",
        ],
    },
    {
        "id": "enterprise",
        "name": "KubeCost Auto-HPA Mission-Critical Enterprise Tier",
        "description": "Enterprise-grade FinOps autoscaler for Fortune 500 & multi-region clusters. Air-gapped deployment, SAML 2.0 SSO, 99.95% SLA, and signed DPA.",
        "monthly_cents_per_node": 3200,  # $32.00 / node / month
        "annual_cents_per_node": 30800,  # $308.00 / node / year (~20% discount)
        "features": [
            "Everything in Growth Tier",
            "Air-Gapped & Sovereign Private Registry Deployment",
            "SAML 2.0 / Okta / Azure AD Single Sign-On",
            "Custom Downscale Anomaly Guards & Financial Budgets",
            "Dedicated FinOps Solutions Architect & 24/7/365 Phone SLA",
            "SOC 2 Type II Audit Packet & Signed GDPR/CCPA DPA",
        ],
    },
]


class StripeBillingClient:
    """Zero-dependency Stripe REST API client using standard library urllib."""

    BASE_URL = "https://api.stripe.com/v1"

    def __init__(self, api_key: Optional[str] = None, dry_run: bool = False):
        self.api_key = api_key or os.environ.get("STRIPE_API_KEY", "")
        self.dry_run = dry_run or not bool(self.api_key)

    def _request(self, endpoint: str, data: Dict[str, Any]) -> Dict[str, Any]:
        """Performs form-urlencoded POST request to Stripe REST API."""
        if self.dry_run:
            mock_id = f"mock_{endpoint.replace('/', '_')}_{abs(hash(str(data))) % 1000000}"
            return {"id": mock_id, **data, "_mode": "mock"}

        url = f"{self.BASE_URL}{endpoint}"
        encoded_data = urllib.parse.urlencode(data).encode("utf-8")
        
        req = urllib.request.Request(url, data=encoded_data, method="POST")
        auth_bytes = f"{self.api_key}:".encode("utf-8")
        req.add_header("Authorization", f"Basic {base64.b64encode(auth_bytes).decode('utf-8')}")
        req.add_header("Content-Type", "application/x-www-form-urlencoded")
        req.add_header("User-Agent", "KubeCost-Auto-HPA-Provisioner/1.0")

        try:
            with urllib.request.urlopen(req, timeout=15) as resp:
                return json.loads(resp.read().decode("utf-8"))
        except urllib.error.HTTPError as e:
            err_body = e.read().decode("utf-8")
            raise RuntimeError(f"Stripe API error ({e.code}): {err_body}")

    def create_product(self, name: str, description: str, metadata: Dict[str, str]) -> Dict[str, Any]:
        data = {
            "name": name,
            "description": description,
        }
        for k, v in metadata.items():
            data[f"metadata[{k}]"] = str(v)
        return self._request("/products", data)

    def create_price(
        self,
        product_id: str,
        unit_amount: int,
        currency: str,
        interval: str,
        nickname: str,
    ) -> Dict[str, Any]:
        data = {
            "product": product_id,
            "unit_amount": unit_amount,
            "currency": currency,
            "recurring[interval]": interval,
            "recurring[usage_type]": "licensed",
            "nickname": nickname,
        }
        return self._request("/prices", data)

    def provision_all(self) -> Dict[str, Any]:
        """Provisions all products, monthly and annual prices."""
        results = {
            "environment": "simulation" if self.dry_run else "stripe_live_or_test",
            "currency": "usd",
            "tiers": {},
        }

        for spec in TIERS_SPECIFICATION:
            tier_id = spec["id"]
            product = self.create_product(
                name=spec["name"],
                description=spec["description"],
                metadata={
                    "tier": tier_id,
                    "engine_version": "0.1.0",
                    "vendor": "AutoFinOps Technologies, Inc.",
                },
            )
            prod_id = product["id"]

            monthly_price = self.create_price(
                product_id=prod_id,
                unit_amount=spec["monthly_cents_per_node"],
                currency="usd",
                interval="month",
                nickname=f"{spec['name']} - Monthly per Node",
            )

            annual_price = self.create_price(
                product_id=prod_id,
                unit_amount=spec["annual_cents_per_node"],
                currency="usd",
                interval="year",
                nickname=f"{spec['name']} - Annual per Node",
            )

            results["tiers"][tier_id] = {
                "name": spec["name"],
                "product_id": prod_id,
                "prices": {
                    "monthly": {
                        "price_id": monthly_price["id"],
                        "unit_amount_usd": spec["monthly_cents_per_node"] / 100.0,
                        "interval": "month",
                    },
                    "annual": {
                        "price_id": annual_price["id"],
                        "unit_amount_usd": spec["annual_cents_per_node"] / 100.0,
                        "interval": "year",
                        "savings_percentage": 20,
                    },
                },
                "features": spec["features"],
            }

        return results


def main() -> int:
    parser = argparse.ArgumentParser(description="Provision Stripe billing catalogue for KubeCost Auto-HPA")
    parser.add_argument("--dry-run", action="store_true", help="Simulate creation without Stripe API key")
    parser.add_argument("--live", action="store_true", help="Execute against Stripe API using STRIPE_API_KEY")
    parser.add_argument("--output", type=str, default="", help="Path to write JSON configuration output")
    args = parser.parse_args()

    api_key = os.environ.get("STRIPE_API_KEY", "")
    if args.live and not api_key:
        print("[ERROR] --live specified but STRIPE_API_KEY environment variable is missing.", file=sys.stderr)
        return 1

    dry_run = args.dry_run or not bool(api_key)
    print(f"[*] Initializing Stripe billing provisioning (mode={'dry-run' if dry_run else 'live'})...")

    client = StripeBillingClient(api_key=api_key, dry_run=dry_run)
    catalog = client.provision_all()

    print("[SUCCESS] Billing catalog successfully provisioned:")
    for tier_id, data in catalog["tiers"].items():
        print(f"  [-] {data['name']}:")
        print(f"      Product ID: {data['product_id']}")
        print(f"      Monthly:    ${data['prices']['monthly']['unit_amount_usd']:.2f}/node/mo ({data['prices']['monthly']['price_id']})")
        print(f"      Annual:     ${data['prices']['annual']['unit_amount_usd']:.2f}/node/yr ({data['prices']['annual']['price_id']})")

    if args.output:
        os.makedirs(os.path.dirname(os.path.abspath(args.output)), exist_ok=True)
        with open(args.output, "w", encoding="utf-8") as f:
            json.dump(catalog, f, indent=2)
        print(f"[*] Configuration saved to {args.output}")

    return 0


if __name__ == "__main__":
    sys.exit(main())
