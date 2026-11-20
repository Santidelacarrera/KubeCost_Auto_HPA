import hashlib
import hmac
import json
import time

import pytest

from kubecost_engine import stripe_checkout
from kubecost_engine.bootstrap import process_stripe_webhook_event
from kubecost_engine.persistence import SQLiteSaaSStore
from kubecost_engine.security import InputValidationError, StripeWebhookConfigError


def test_hosted_checkout_persists_and_reconciles_only_matching_signed_payment(tmp_path, monkeypatch):
    for key, value in {
        "STRIPE_SECRET_KEY": "sk_test_example", "STRIPE_WEBHOOK_SECRET": "whsec_example",
        "STRIPE_CHECKOUT_MODE": "test", "STRIPE_CHECKOUT_PRICES": '{"pilot":"price_demo"}',
        "STRIPE_CHECKOUT_SUCCESS_URL": "https://example.test/success",
        "STRIPE_CHECKOUT_CANCEL_URL": "https://example.test/cancel",
    }.items():
        monkeypatch.setenv(key, value)
    db = str(tmp_path / "checkout.db")
    store = SQLiteSaaSStore(db)
    calls = []

    def stripe(key, idem, params):
        calls.append((idem, params))
        assert params["line_items[0][price]"] == "price_demo"
        assert params["client_reference_id"] == "org"
        return {"livemode": False, "mode": "subscription", "client_reference_id": "org",
                "id": "cs_test_a", "url": "https://checkout.stripe.com/c/pay/example"}

    monkeypatch.setattr(stripe_checkout, "create_at_stripe", stripe)
    payload = {"plan": "pilot", "request_id": "payment-1", "amount": 1}
    try:
        opened = stripe_checkout.create_checkout(store, "org", payload)
        assert opened["status"] == "open"
        assert not store.get_object("elsewhere", stripe_checkout.KIND, "payment-1")
        store.close()
        store = SQLiteSaaSStore(db)
        assert stripe_checkout.create_checkout(store, "org", payload) == opened
        assert len(calls) == 1

        def webhook(event_id, session_id, payment, event_type="checkout.session.completed"):
            evt = {"id": event_id, "type": event_type, "livemode": False, "data": {"object": {
                "id": session_id, "livemode": False, "mode": "subscription", "client_reference_id": "org",
                "status": "complete", "payment_status": payment, "amount_total": 4500, "currency": "usd",
                "metadata": {"kca_checkout": "payment-1", "kca_tenant": "org"},
            }}}
            raw = json.dumps(evt)
            now = int(time.time())
            digest = hmac.new(b"whsec_example", f"{now}.{raw}".encode(), hashlib.sha256).hexdigest()
            return process_stripe_webhook_event(raw, f"t={now},v1={digest}", store=store)

        with pytest.raises(InputValidationError):
            webhook("evt_bad_session", "cs_test_unknown", "paid")
        webhook("evt_unpaid_checkout", "cs_test_a", "unpaid")
        assert store.get_object("org", stripe_checkout.KIND, "payment-1")["status"] == "processing"
        webhook("evt_paid_checkout", "cs_test_a", "paid", "checkout.session.async_payment_succeeded")
        webhook("evt_checkout_expired", "cs_test_a", "unpaid", "checkout.session.expired")
        paid = stripe_checkout.create_checkout(store, "org", payload)
        assert paid["status"] == "paid" and paid["amount_total"] == 4500 and "checkout_url" not in paid

        monkeypatch.setenv("STRIPE_CHECKOUT_MODE", "live")
        with pytest.raises(StripeWebhookConfigError):
            stripe_checkout.create_checkout(store, "org", payload)
    finally:
        store.close()
