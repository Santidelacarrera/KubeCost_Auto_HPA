"""MOCK KubeCost allocation API for the lab. THIS IS NOT KUBECOST.

It answers GET /model/allocation with one accumulated set whose window ends now, with a constant, invented cost
for the workload named in the environment. It exists only so the operator's KubeCost integration can run in a lab
that has no KubeCost. Nothing it returns is a price, a bill or evidence about a real KubeCost response.
"""
import json
import os
import re
from datetime import datetime, timedelta, timezone
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import parse_qs, urlparse

NAMESPACE = os.environ.get("MOCK_NAMESPACE", "lab")
CONTROLLER = os.environ.get("MOCK_CONTROLLER", "scaletest")
HOURLY = float(os.environ.get("MOCK_HOURLY_USD", "0.20"))  # invented


def window_minutes(w):
    m = re.fullmatch(r"(\d+)([smh])", w or "")
    if not m:
        return 15.0
    n = int(m.group(1))
    return {"s": n / 60.0, "m": float(n), "h": n * 60.0}[m.group(2)]


class H(BaseHTTPRequestHandler):
    def do_GET(self):
        u = urlparse(self.path)
        if u.path in ("/healthz", "/"):
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b"mock-kubecost")
            return
        if u.path != "/model/allocation":
            self.send_response(404)
            self.end_headers()
            return
        mins = window_minutes(parse_qs(u.query).get("window", ["15m"])[0])
        end = datetime.now(timezone.utc).replace(microsecond=0)
        start = end - timedelta(minutes=mins)
        iso = lambda t: t.strftime("%Y-%m-%dT%H:%M:%SZ")
        key = f"{NAMESPACE}/{CONTROLLER}"
        body = {"code": 200, "status": "success", "data": [{key: {
            "name": key,
            "properties": {"cluster": "lab", "namespace": NAMESPACE, "controllerKind": "deployment", "controller": CONTROLLER},
            "window": {"start": iso(start), "end": iso(end)}, "start": iso(start), "end": iso(end),
            "minutes": mins, "totalCost": round(HOURLY * mins / 60.0, 6),
        }}]}
        raw = json.dumps(body).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def log_message(self, *a):
        pass


HTTPServer(("0.0.0.0", 9090), H).serve_forever()
