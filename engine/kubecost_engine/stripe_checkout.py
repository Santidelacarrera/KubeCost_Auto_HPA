"""Authenticated Stripe-hosted checkout and reconciliation of signed payments."""

import hashlib
import json
import os
import re
import time
import urllib.error
import urllib.parse
import urllib.request

from .security import InputValidationError, StripeWebhookConfigError, validate_identifier


KIND = "stripe_checkout"
CHECKOUT_EVENTS = {
    "checkout.session.completed", "checkout.session.async_payment_succeeded",
    "checkout.session.async_payment_failed", "checkout.session.expired",
}


class StripeUnavailable(RuntimeError):
    pass


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


def settings():
    mode = os.getenv("STRIPE_CHECKOUT_MODE", "")
    key = os.getenv("STRIPE_SECRET_KEY", "")
    try:
        prices = json.loads(os.getenv("STRIPE_CHECKOUT_PRICES", "{}"))
    except ValueError as exc:
        raise StripeWebhookConfigError("invalid Stripe Checkout catalog") from exc
    if mode not in ("test", "live") or not key.startswith((f"sk_{mode}_", f"rk_{mode}_")) or not os.getenv("STRIPE_WEBHOOK_SECRET"):
        raise StripeWebhookConfigError("Stripe Checkout is not configured")
    if not isinstance(prices, dict) or not prices or any(
        not isinstance(price, str) or not re.fullmatch(r"price_[A-Za-z0-9]+", price)
        for price in prices.values()
    ):
        raise StripeWebhookConfigError("invalid Stripe Checkout catalog")
    urls = {name: os.getenv(f"STRIPE_CHECKOUT_{name.upper()}", "") for name in ("success_url", "cancel_url")}
    for url in urls.values():
        parts = urllib.parse.urlsplit(url)
        if parts.scheme != "https" or not parts.hostname or parts.username or parts.password:
            raise StripeWebhookConfigError("Stripe Checkout return URLs must be HTTPS")
    return mode, key, prices, urls


def create_at_stripe(key, idem, params):
    req = urllib.request.Request("https://api.stripe.com/v1/checkout/sessions",
                                 data=urllib.parse.urlencode(params).encode(),
                                 headers={"Authorization": "Bearer " + key, "Idempotency-Key": idem,
                                          "Content-Type": "application/x-www-form-urlencoded"})
    try:
        with urllib.request.build_opener(NoRedirect()).open(req, timeout=12) as response:
            return json.load(response)
    except (OSError, ValueError) as exc:
        raise StripeUnavailable("Stripe Checkout is temporarily unavailable") from exc


def create_checkout(store, tenant, payload):
    mode, key, prices, urls = settings()
    if getattr(store, "db_path", None) == ":memory:":
        raise StripeWebhookConfigError("Stripe Checkout requires durable storage")
    request_id = validate_identifier(payload.get("request_id"), "request_id")
    plan = validate_identifier(payload.get("plan"), "plan")
    if plan not in prices:
        raise InputValidationError("plan is not enabled for Stripe Checkout")
    idem = hashlib.sha256(f"kca-checkout:{mode}:{tenant}:{request_id}".encode()).hexdigest()
    params = {
        **urls, "mode": "subscription", "line_items[0][price]": prices[plan],
        "line_items[0][quantity]": 1, "client_reference_id": tenant,
        "metadata[kca_checkout]": request_id, "metadata[kca_tenant]": tenant,
    }
    candidate = {"id": request_id, "plan": plan, "livemode": mode == "live", "params": params,
                 "created_at": time.time(), "status": "creating"}
    row = store.get_object(tenant, KIND, request_id)
    if row is None:
        try:
            row = store.put_object(tenant, KIND, request_id, candidate)
        except ValueError:
            row = store.get_object(tenant, KIND, request_id)
            if row is None:
                raise
    if row["plan"] != plan or row["livemode"] != candidate["livemode"]:
        raise InputValidationError("request_id is bound to another checkout")
    if "session_id" not in row:
        if time.time() - row["created_at"] >= 20 * 3600:
            raise InputValidationError("expired Checkout request. Create a new request_id")
        created = create_at_stripe(key, idem, row["params"])
        if not isinstance(created, dict):
            raise StripeUnavailable("Stripe returned an invalid Checkout session")
        url = urllib.parse.urlsplit(str(created.get("url", "")))
        session = created.get("id", "")
        if (
            created.get("livemode") is not row["livemode"] or created.get("mode") != "subscription"
            or created.get("client_reference_id") != tenant
            or not isinstance(session, str) or not re.fullmatch(r"cs_[A-Za-z0-9_]+", session)
            or url.scheme != "https" or url.netloc != "checkout.stripe.com"
        ):
            raise StripeUnavailable("Stripe returned an invalid Checkout session")
        try:
            row = store.put_object(tenant, KIND, request_id, {
                **row, "session_id": session, "checkout_url": created["url"], "status": "open",
            }, row["version"])
        except ValueError:
            row = store.get_object(tenant, KIND, request_id)
            if not row or row.get("session_id") != session:
                raise StripeUnavailable("could not persist the Stripe Checkout session")
    return public_status(row)


def public_status(row):
    return {k: row[k] for k in ("id", "plan", "livemode", "status", "session_id", "checkout_url", "amount_total", "currency") if k in row
            and (k != "checkout_url" or row["status"] == "open")}


def reconcile_checkout_event(store, event, session):
    meta = session.get("metadata")
    if event["type"] not in CHECKOUT_EVENTS or not isinstance(meta, dict) or "kca_checkout" not in meta:
        return None
    if store is None:
        raise StripeUnavailable("Checkout persistence is unavailable")
    tenant = validate_identifier(meta.get("kca_tenant"), "checkout tenant")
    request_id = validate_identifier(meta.get("kca_checkout"), "checkout request")
    row = store.get_object(tenant, KIND, request_id)
    if (
        row is None or not row.get("session_id") or session.get("id") != row["session_id"]
        or event.get("livemode") is not row["livemode"] or session.get("livemode") is not row["livemode"]
        or session.get("client_reference_id") != tenant or session.get("mode") != "subscription"
    ):
        raise InputValidationError("Stripe event does not match a recorded checkout")
    if row["status"] == "paid" or row.get("last_event_id") == event["id"]:
        return tenant
    if event["type"] in ("checkout.session.completed", "checkout.session.async_payment_succeeded"):
        if session.get("status") != "complete":
            return tenant
        if session.get("payment_status") == "paid":
            total, currency = session.get("amount_total"), session.get("currency")
            if type(total) is not int or total <= 0 or not isinstance(currency, str) or not re.fullmatch(r"[a-z]{3}", currency):
                raise InputValidationError("Stripe event has no valid paid amount")
            row.update(status="paid", amount_total=total, currency=currency)
        elif row["status"] == "open":
            row["status"] = "processing"
    else:
        row["status"] = "expired" if event["type"].endswith(".expired") else "payment_failed"
    store.put_object(tenant, KIND, request_id, {**row, "last_event_id": event["id"]}, row["version"])
    return tenant
