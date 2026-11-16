"""Endpoints that answer from example inputs must say so; endpoints that compute from the caller's data must not."""

import json
import threading
import urllib.request

import pytest

from kubecost_engine.auth_billing import generate_stripe_billing_invoice
from kubecost_engine.server import SAMPLE_DATA_NOTICE, _is_sample_data_path, make_server

BASE = dict(cpu_millicores=1000, p99_ms=50, slo_p99_ms=200, rps=100,
            cpu_price_per_core_hour=0.04, mem_price_per_gib_hour=0.005, memory_gib=1)


@pytest.fixture()
def url():
    srv = make_server("127.0.0.1", 0)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    try:
        yield f"http://127.0.0.1:{srv.server_address[1]}"
    finally:
        srv.shutdown()
        srv.server_close()


@pytest.mark.parametrize("path", [
    "/v1/cast/full-suite",
    "/v1/gpu/optimize",
    "/v1/greenops/report?region=us-east-1",
    "/v1/gameday/simulate",
    "/v1/pricing/lookup?instance_type=n2-standard-8&region=us-central1",
])
def test_sample_endpoints_are_labelled_in_header_and_body(url, path):
    with urllib.request.urlopen(url + path) as resp:
        assert resp.headers["X-KubeCost-Data-Source"] == "sample"
        body = json.load(resp)
    assert body["data_source"] == SAMPLE_DATA_NOTICE
    assert "not a provider bill" in body["data_source"]


def test_computation_on_caller_data_is_not_labelled_sample(url):
    req = urllib.request.Request(url + "/v1/evaluate", json.dumps(BASE).encode(), {"Content-Type": "application/json"})
    with urllib.request.urlopen(req) as resp:
        assert resp.headers.get("X-KubeCost-Data-Source") is None
        assert "data_source" not in json.load(resp)
    with urllib.request.urlopen(url + "/healthz") as resp:
        assert resp.headers.get("X-KubeCost-Data-Source") is None


def test_path_classification():
    assert _is_sample_data_path("/v1/cast/commitments")
    assert _is_sample_data_path("/v1/billing/invoice")
    assert not _is_sample_data_path("/v1/evaluate")
    assert not _is_sample_data_path("/v1/audit/decisions")
    assert not _is_sample_data_path("/v1/finops/guardrail")  # computed from the figures the caller sends


def test_invoice_preview_never_claims_a_charge_or_verified_savings():
    inv = generate_stripe_billing_invoice("acme", "gke-prod", 3420.0, 6)
    assert inv["simulation"] is True and inv["charge_created"] is False
    assert "not reconciled" in inv["savings_evidence"]
