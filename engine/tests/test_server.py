import json
import threading
import urllib.error
import urllib.request

import pytest

from kubecost_engine.server import evaluate, make_server

BASE = dict(cpu_millicores=1000, p99_ms=50, slo_p99_ms=200, rps=100,
            cpu_price_per_core_hour=0.04, mem_price_per_gib_hour=0.005, memory_gib=1)


def test_evaluate_shrink_and_cost():
    r = evaluate(BASE)
    assert r["action"] == "shrink" and r["cpu_millicores"] == 800
    assert r["cost_per_request_after"] < r["cost_per_request"]


def test_usage_floor_prevents_unsafe_shrink():
    r = evaluate({**BASE, "cpu_usage_millicores": 900})
    assert r["action"] == "hold"  # floor = 900/0.7 = 1286 > current


def test_cost_over_target_shrinks_in_band():
    base = {**BASE, "p99_ms": 120}
    assert evaluate(base)["action"] == "hold"
    assert evaluate({**base, "max_cost_per_request": 1e-9})["action"] == "shrink"


def test_budget_frozen_blocks_growth_only():
    grow = {**BASE, "p99_ms": 195, "budget": {"monthly_limit": 100, "spent": 100}}
    r = evaluate(grow)
    assert r["action"] == "hold" and r["budget_frozen"]
    assert evaluate({**BASE, "budget": {"monthly_limit": 100, "spent": 100}})["action"] == "shrink"


def test_spot_flag():
    r = evaluate({**BASE, "spot": {"fault_tolerant": True, "replicas": 3, "has_pdb": True}})
    assert r["spot_eligible"] is True


def test_missing_field():
    with pytest.raises(KeyError):
        evaluate({"cpu_millicores": 1})


def test_http_roundtrip():
    srv = make_server("127.0.0.1", 0)
    port = srv.server_address[1]
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    try:
        url = f"http://127.0.0.1:{port}"
        req = urllib.request.Request(url + "/v1/evaluate", json.dumps(BASE).encode(),
                                     {"Content-Type": "application/json"})
        assert json.load(urllib.request.urlopen(req))["action"] == "shrink"
        assert json.load(urllib.request.urlopen(url + "/healthz"))["status"] == "ok"
        assert b"kubecost_engine_decisions_total" in urllib.request.urlopen(url + "/metrics").read()
        bad = urllib.request.Request(url + "/v1/evaluate", b"{}", {"Content-Type": "application/json"})
        with pytest.raises(urllib.error.HTTPError) as e:
            urllib.request.urlopen(bad)
        assert e.value.code == 400
    finally:
        srv.shutdown()
