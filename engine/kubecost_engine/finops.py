"""Strict financial import subset. No invented prices, FX rates, or savings evidence."""

from __future__ import annotations

from collections import defaultdict
from datetime import datetime
from decimal import Decimal, InvalidOperation
import hashlib
import json


def money(value):
    try:
        result = Decimal(str(value))
    except InvalidOperation as exc:
        raise ValueError("invalid monetary value") from exc
    if not result.is_finite() or abs(result) > Decimal("1e15"):
        raise ValueError("non-finite or excessive monetary value")
    return result


def instant(value):
    parsed = datetime.fromisoformat(str(value).replace("Z", "+00:00"))
    if parsed.tzinfo is None:
        raise ValueError("financial periods require a timezone")
    return parsed


def normalize_records(records, provider):
    """Accept FOCUS subset or explicit GCP/Azure exported rows; report invalid rows."""
    if provider not in ("focus", "gcp", "azure"):
        raise ValueError("provider must be focus, gcp or azure")
    if not isinstance(records, list) or not 1 <= len(records) <= 5000:
        raise ValueError("records must contain 1..5000 rows")
    valid, errors, seen = [], [], set()
    for index, raw in enumerate(records):
        try:
            if not isinstance(raw, dict):
                raise ValueError("row must be an object")
            if provider == "focus":
                billed, effective = (
                    money(raw["BilledCost"]),
                    money(raw["EffectiveCost"]),
                )
                currency, start, end = (
                    raw["BillingCurrency"],
                    raw["ChargePeriodStart"],
                    raw["ChargePeriodEnd"],
                )
                resource = raw.get("ResourceId")
            elif provider == "gcp":
                billed = money(raw["cost"]) + sum(
                    (money(c["amount"]) for c in raw.get("credits", [])), Decimal(0)
                )
                effective = money(raw.get("effective_cost", billed))
                currency, start, end = (
                    raw["currency"],
                    raw["usage_start_time"],
                    raw["usage_end_time"],
                )
                resource = raw.get("resource", {}).get("name")
            else:
                billed = money(raw["CostInBillingCurrency"])
                effective = money(raw.get("EffectiveCost", billed))
                currency, start, end = (
                    raw["BillingCurrency"],
                    raw["ChargePeriodStart"],
                    raw["ChargePeriodEnd"],
                )
                resource = raw.get("ResourceId")
            if (
                not isinstance(currency, str)
                or len(currency) != 3
                or not currency.isalpha()
                or currency != currency.upper()
            ):
                raise ValueError("currency must be an uppercase ISO code")
            if instant(end) <= instant(start):
                raise ValueError("charge period end must follow start")
            # Caller must identify rows to avoid silently dropping identical legitimate charges.
            row_id = raw.get("record_id")
            if not isinstance(row_id, str) or not row_id.strip():
                raise ValueError("record_id required for import idempotency")
            if row_id in seen:
                raise ValueError("duplicate record_id")
            seen.add(row_id)
            valid.append(
                {
                    "record_id": row_id,
                    "provider": provider,
                    "currency": currency,
                    "billed_cost": str(billed),
                    "effective_cost": str(effective),
                    "start": start,
                    "end": end,
                    "resource_id": resource,
                    "cluster_id": raw.get("cluster_id"),
                    "namespace": raw.get("namespace"),
                    "workload": raw.get("workload"),
                    "node": raw.get("node"),
                    "allocation_status": "allocated"
                    if raw.get("cluster_id")
                    else "unallocated",
                    "source_sha256": hashlib.sha256(
                        json.dumps(raw, sort_keys=True).encode()
                    ).hexdigest(),
                }
            )
        except (ValueError, KeyError, TypeError, AttributeError) as exc:
            errors.append({"row": index, "error": str(exc)})
    return {
        "records": valid,
        "errors": errors,
        "schema": "kca-financial-import-v1 (FOCUS subset, not conformance certification)",
    }


def summarize(records, dimension="cluster_id"):
    if dimension not in ("cluster_id", "namespace", "workload", "node", "provider"):
        raise ValueError("unsupported allocation dimension")
    groups = defaultdict(
        lambda: {"billed_cost": Decimal(0), "effective_cost": Decimal(0), "records": 0}
    )
    for row in records:
        group = groups[(row["currency"], row.get(dimension) or "unallocated")]
        group["billed_cost"] += money(row["billed_cost"])
        group["effective_cost"] += money(row["effective_cost"])
        group["records"] += 1
    return [
        {
            "currency": currency,
            dimension: key,
            **{k: str(v) if isinstance(v, Decimal) else v for k, v in group.items()},
        }
        for (currency, key), group in sorted(groups.items())
    ]


def reconcile(records, invoice):
    required = ("invoice_id", "source_sha256", "currency", "start", "end", "total")
    if (
        any(not invoice.get(k) for k in required if k != "total")
        or "total" not in invoice
    ):
        raise ValueError("invoice identity, hash, currency, period and total required")
    digest = invoice["source_sha256"]
    if len(digest) != 64 or any(c not in "0123456789abcdef" for c in digest):
        raise ValueError("source_sha256 must identify the supplied invoice file")
    start, end = instant(invoice["start"]), instant(invoice["end"])
    if end <= start:
        raise ValueError("invalid invoice period")
    selected = [
        r
        for r in records
        if r["currency"] == invoice["currency"]
        and instant(r["start"]) >= start
        and instant(r["end"]) <= end
    ]
    total = sum((money(r["billed_cost"]) for r in selected), Decimal(0))
    difference = money(invoice["total"]) - total
    return {
        "invoice_id": invoice["invoice_id"],
        "source_sha256": digest,
        "currency": invoice["currency"],
        "billed_total": str(total),
        "invoice_total": str(money(invoice["total"])),
        "difference": str(difference),
        "record_count": len(selected),
        "status": "matched"
        if selected and abs(difference) <= Decimal("0.01")
        else "unmatched",
        "evidence_kind": "user_supplied_invoice_metadata",
        "invoice_reconciled_savings": None,
        "reason": "Invoice matching does not establish attributable savings; baseline, demand, discounts and rate changes require separate review",
    }
