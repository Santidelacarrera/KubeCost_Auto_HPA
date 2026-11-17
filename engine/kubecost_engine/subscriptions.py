"""Payment boundary and local commercial test environment. No live transactions."""

from __future__ import annotations

from typing import Protocol
import time

from .finops import money
from .security import validate_identifier


class PaymentProvider(Protocol):
    def charge(
        self, organization: str, amount: str, currency: str, idempotency_key: str
    ) -> dict: ...


class LocalSandboxProvider:
    def charge(self, organization, amount, currency, idempotency_key):
        return {
            "provider": "local_sandbox",
            "reference": "sandbox-" + idempotency_key,
            "organization": organization,
            "amount": amount,
            "currency": currency,
            "status": "simulated_success",
            "live_transaction": False,
        }


def transition(store, tenant, payload, plans, now=None):
    now = time.time() if now is None else now
    action = payload["action"]
    if action not in ("activate", "renew", "cancel"):
        raise ValueError("action must be activate, renew or cancel")
    event_id = validate_identifier(payload["event_id"], "event_id")
    existing = store.list_objects(tenant, "subscription")
    state = (
        existing[0]
        if existing
        else {"id": "subscription", "status": "inactive", "history": [], "version": 0}
    )
    if any(e["event_id"] == event_id for e in state["history"]):
        return {**state, "duplicate_ignored": True}
    if (
        action == "activate"
        and state["status"] == "active"
        and state.get("expires_at", 0) > now
    ):
        raise ValueError("subscription already active")
    if action in ("renew", "cancel") and not existing:
        raise ValueError("subscription does not exist")
    event = {
        "event_id": event_id,
        "action": action,
        "timestamp": now,
        "environment": "local_sandbox",
    }
    if action == "cancel":
        state["status"] = "cancelled"
        state["expires_at"] = now
    else:
        plan = payload["plan"]
        if plan not in plans:
            raise ValueError("unknown plan")
        days = payload.get("term_days", 30)
        if type(days) is not int or not 1 <= days <= 366:
            raise ValueError("term_days must be 1..366")
        amount = money(payload["amount"])
        if amount < 0:
            raise ValueError("amount cannot be negative")
        currency = payload["currency"]
        if (
            not isinstance(currency, str)
            or len(currency) != 3
            or not currency.isalpha()
            or not currency.isupper()
        ):
            raise ValueError("invalid currency")
        payment = LocalSandboxProvider().charge(tenant, str(amount), currency, event_id)
        event["payment"] = payment
        state.update(
            plan=plan,
            status="active",
            expires_at=max(now, state.get("expires_at", 0)) + days * 86400,
        )
    state["history"].append(event)
    state["environment"] = "local_sandbox"
    state["grants_production_entitlements"] = False
    return store.put_object(
        tenant, "subscription", "subscription", state, state["version"]
    )
