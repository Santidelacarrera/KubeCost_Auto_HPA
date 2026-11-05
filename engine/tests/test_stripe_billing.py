import json
import os
import sys
import unittest

# Ensure scripts directory is on Python path
REPO_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
sys.path.insert(0, os.path.join(REPO_ROOT, "scripts"))

from setup_stripe_billing import StripeBillingClient, TIERS_SPECIFICATION


class TestStripeBilling(unittest.TestCase):
    def setUp(self):
        self.client = StripeBillingClient(dry_run=True)

    def test_tiers_specification_integrity(self):
        self.assertGreaterEqual(len(TIERS_SPECIFICATION), 2)
        tier_ids = [t["id"] for t in TIERS_SPECIFICATION]
        self.assertIn("growth", tier_ids)
        self.assertIn("enterprise", tier_ids)

        for tier in TIERS_SPECIFICATION:
            self.assertGreater(tier["monthly_cents_per_node"], 0)
            self.assertGreater(tier["annual_cents_per_node"], 0)
            # Annual price should provide discount compared to 12 x monthly
            self.assertLess(tier["annual_cents_per_node"], tier["monthly_cents_per_node"] * 12)
            self.assertGreater(len(tier["features"]), 3)

    def test_provision_all_dry_run(self):
        catalog = self.client.provision_all()

        self.assertEqual(catalog["environment"], "simulation")
        self.assertEqual(catalog["currency"], "usd")
        self.assertIn("growth", catalog["tiers"])
        self.assertIn("enterprise", catalog["tiers"])

        growth = catalog["tiers"]["growth"]
        self.assertEqual(growth["prices"]["monthly"]["unit_amount_usd"], 18.0)
        self.assertEqual(growth["prices"]["annual"]["unit_amount_usd"], 172.0)
        self.assertTrue(growth["product_id"].startswith("mock_"))

        enterprise = catalog["tiers"]["enterprise"]
        self.assertEqual(enterprise["prices"]["monthly"]["unit_amount_usd"], 32.0)
        self.assertEqual(enterprise["prices"]["annual"]["unit_amount_usd"], 308.0)
        self.assertTrue(enterprise["product_id"].startswith("mock_"))


if __name__ == "__main__":
    unittest.main()
