"""HTTP front-end so the Go operator can consume the engine.

stdlib only. Endpoints: GET /healthz, GET /metrics, POST /v1/evaluate.
"""
import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from .economics import cost_per_request_from_pod
from .guardrails import BudgetGuardrail
from .scaling import recommend_resize
from .spot import SpotCandidate, is_spot_eligible

_counters: dict[str, int] = {}
_lock = threading.Lock()


def _count(action: str) -> None:
    with _lock:
        _counters[action] = _counters.get(action, 0) + 1


def evaluate(req: dict) -> dict:
    """Pure evaluation of one workload. Raises ValueError/KeyError on bad input."""
    cpu = int(req["cpu_millicores"])
    rps = float(req.get("rps", 0))
    cpu_price = float(req.get("cpu_price_per_core_hour", 0))
    mem_price = float(req.get("mem_price_per_gib_hour", 0))
    mem_gib = float(req.get("memory_gib", 0))
    max_cpr = float(req.get("max_cost_per_request", 0))

    cpr = cost_per_request_from_pod(cpu / 1000, mem_gib, rps, cpu_price, mem_price)
    over = max_cpr > 0 and cpr > max_cpr

    d = recommend_resize(
        cpu,
        float(req["p99_ms"]),
        float(req["slo_p99_ms"]),
        min_millicores=int(req.get("min_millicores", 50)),
        max_millicores=int(req.get("max_millicores", 8000)),
        cpu_usage_millicores=req.get("cpu_usage_millicores"),
        cost_over_target=over,
    )

    b = req.get("budget") or {}
    frozen = False
    action, new_cpu, reason = d.action, d.cpu_millicores, d.reason
    if b.get("monthly_limit"):
        g = BudgetGuardrail(float(b["monthly_limit"]), float(b.get("spent", 0)))
        frozen = g.frozen
        if not g.allows(action):
            action, new_cpu, reason = "hold", cpu, "budget frozen: growth blocked"

    spot = None
    if req.get("spot"):
        s = req["spot"]
        spot = is_spot_eligible(
            SpotCandidate(bool(s.get("fault_tolerant")), int(s.get("replicas", 0)),
                          bool(s.get("has_pdb")), bool(s.get("stateful"))),
            int(s.get("min_replicas", 2)),
        )

    after = cost_per_request_from_pod(new_cpu / 1000, mem_gib, rps, cpu_price, mem_price)
    _count(action)
    return {
        "action": action,
        "cpu_millicores": new_cpu,
        "reason": reason,
        "cost_per_request": None if cpr == float("inf") else cpr,
        "cost_per_request_after": None if after == float("inf") else after,
        "budget_frozen": frozen,
        "spot_eligible": spot,
    }


def render_metrics() -> str:
    lines = ["# TYPE kubecost_engine_decisions_total counter"]
    with _lock:
        for k, v in sorted(_counters.items()):
            lines.append(f'kubecost_engine_decisions_total{{action="{k}"}} {v}')
    return "\n".join(lines) + "\n"


class Handler(BaseHTTPRequestHandler):
    def _send(self, code: int, body: str, ctype: str = "application/json") -> None:
        data = body.encode()
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):  # noqa: N802
        if self.path == "/healthz":
            self._send(200, '{"status":"ok"}')
        elif self.path == "/metrics":
            self._send(200, render_metrics(), "text/plain; version=0.0.4")
        else:
            self._send(404, '{"error":"not found"}')

    def do_POST(self):  # noqa: N802
        if self.path != "/v1/evaluate":
            return self._send(404, '{"error":"not found"}')
        try:
            n = int(self.headers.get("Content-Length", 0))
            if n > 1 << 20:
                return self._send(413, '{"error":"payload too large"}')
            self._send(200, json.dumps(evaluate(json.loads(self.rfile.read(n)))))
        except (ValueError, KeyError, TypeError) as e:
            self._send(400, json.dumps({"error": str(e)}))

    def log_message(self, *args):  # keep stdout quiet
        pass


def make_server(host: str = "0.0.0.0", port: int = 8000) -> ThreadingHTTPServer:
    return ThreadingHTTPServer((host, port), Handler)


def main() -> None:
    import os
    srv = make_server(port=int(os.environ.get("PORT", "8000")))
    srv.serve_forever()


if __name__ == "__main__":
    main()
