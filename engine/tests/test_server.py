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


def test_memory_and_replicas_in_response():
    r = evaluate({**BASE, "memory": {"request_mib": 1000, "usage_mib": 900}})
    assert r["memory"]["action"] == "grow" and r["memory"]["mib"] == 1250
    r = evaluate({**BASE, "cpu_millicores": 8000, "max_millicores": 8000, "p99_ms": 195,
                  "replicas": {"current": 3, "min": 1, "max": 10}})
    assert r["replicas"]["target"] == 4
    frozen = evaluate({**BASE, "cpu_millicores": 8000, "max_millicores": 8000, "p99_ms": 195,
                       "replicas": {"current": 3, "min": 1, "max": 10},
                       "budget": {"monthly_limit": 10, "spent": 10}})
    assert frozen["replicas"]["target"] == 3
    assert evaluate(BASE)["memory"] is None and evaluate(BASE)["replicas"] is None


def _serve(monkeypatch=None):
    srv = make_server("127.0.0.1", 0)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv, f"http://127.0.0.1:{srv.server_address[1]}"


def test_bearer_token(monkeypatch):
    monkeypatch.setenv("ENGINE_TOKEN", "s3cret")
    srv, url = _serve()
    try:
        def post(h):
            return urllib.request.Request(url + "/v1/evaluate", json.dumps(BASE).encode(),
                                          {"Content-Type": "application/json", **h})
        with pytest.raises(urllib.error.HTTPError) as e:
            urllib.request.urlopen(post({}))
        assert e.value.code == 401
        with pytest.raises(urllib.error.HTTPError):
            urllib.request.urlopen(post({"Authorization": "Bearer nope"}))
        assert json.load(urllib.request.urlopen(post({"Authorization": "Bearer s3cret"})))["action"] == "shrink"
        assert urllib.request.urlopen(url + "/healthz").status == 200  # probes stay open
    finally:
        srv.shutdown()


def test_tls_and_mtls(monkeypatch, tmp_path):
    import shutil, ssl, subprocess
    if not shutil.which("openssl"):
        pytest.skip("openssl not available")
    def run(*a):
        subprocess.run(a, check=True, cwd=tmp_path, capture_output=True)
    run("openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", "ca.key", "-out", "ca.crt",
        "-days", "1", "-subj", "/CN=test-ca",
        "-addext", "basicConstraints=critical,CA:TRUE", "-addext", "keyUsage=critical,keyCertSign,cRLSign")
    for name, ext in (("server", "subjectAltName=IP:127.0.0.1"), ("client", "extendedKeyUsage=clientAuth")):
        run("openssl", "req", "-newkey", "rsa:2048", "-nodes", "-keyout", f"{name}.key", "-out", f"{name}.csr", "-subj", f"/CN={name}")
        (tmp_path / f"{name}.ext").write_text(ext)
        run("openssl", "x509", "-req", "-in", f"{name}.csr", "-CA", "ca.crt", "-CAkey", "ca.key", "-CAcreateserial",
            "-out", f"{name}.crt", "-days", "1", "-extfile", f"{name}.ext")
    monkeypatch.setenv("TLS_CERT", str(tmp_path / "server.crt"))
    monkeypatch.setenv("TLS_KEY", str(tmp_path / "server.key"))
    monkeypatch.setenv("TLS_CLIENT_CA", str(tmp_path / "ca.crt"))
    srv = make_server("127.0.0.1", 0)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    url = f"https://127.0.0.1:{srv.server_address[1]}/healthz"
    try:
        good = ssl.create_default_context(cafile=str(tmp_path / "ca.crt"))
        good.load_cert_chain(str(tmp_path / "client.crt"), str(tmp_path / "client.key"))
        assert urllib.request.urlopen(url, context=good).status == 200
        no_cert = ssl.create_default_context(cafile=str(tmp_path / "ca.crt"))
        with pytest.raises(Exception):
            urllib.request.urlopen(url, context=no_cert).read()
    finally:
        srv.shutdown()


@pytest.mark.parametrize("patch", [
    {"p99_ms": -1}, {"rps": -5}, {"cpu_millicores": 0}, {"slo_p99_ms": 0},
    {"min_millicores": 9000}, {"p99_ms": "50"}, {"p99_ms": True}, {"cpu_usage_millicores": -3},
])
def test_invalid_input_rejected(patch):
    with pytest.raises((ValueError, KeyError)):
        evaluate({**BASE, **patch})


def test_nan_and_infinity_rejected_over_http():
    srv = make_server("127.0.0.1", 0)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    url = f"http://127.0.0.1:{srv.server_address[1]}/v1/evaluate"
    try:
        for raw in (b'{"cpu_millicores":1000,"p99_ms":NaN,"slo_p99_ms":200}',
                    b'{"cpu_millicores":1000,"p99_ms":Infinity,"slo_p99_ms":200}', b'[1,2]'):
            with pytest.raises(urllib.error.HTTPError) as e:
                urllib.request.urlopen(urllib.request.Request(url, raw, {"Content-Type": "application/json"}))
            assert e.value.code == 400
    finally:
        srv.shutdown()
