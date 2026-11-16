#!/usr/bin/env python3
"""Reproducible benchmark harness: the same workloads, the same load, the same measurements for any optimiser.

    python benchmarks/bench.py traces                      # write the deterministic load traces (benchmarks/traces/*.csv)
    python benchmarks/bench.py render <scenario> <ns>      # print the Kubernetes manifests of one scenario
    python benchmarks/bench.py collect --prom URL --namespace NS --start T --end T --tool NAME [...] > result.json
    python benchmarks/bench.py compare a.json b.json ...   # one Markdown table, one column per result

It measures what Prometheus (cAdvisor + kube-state-metrics) saw. It installs, configures and judges nothing: the tool
under test is installed by the person running the benchmark, following that tool's own documentation.
Standard library only. See docs/BENCHMARKING.md for the method and the rules for reporting a result.
"""

from __future__ import annotations

import argparse
import json
import math
import random
import sys
import urllib.parse
import urllib.request
from pathlib import Path

HERE = Path(__file__).resolve().parent
SCENARIOS = json.loads((HERE / "scenarios.json").read_text(encoding="utf-8"))
SEED = 20261115  # fixed: the traces are part of the benchmark definition


# ---------------------------------------------------------------- traces

def trace(shape: dict, minutes: int, rng: random.Random) -> list[int]:
    """Requests per second for each minute of the run. Deterministic for a given seed."""
    base, kind = shape["base_rps"], shape["kind"]
    out = []
    for m in range(minutes):
        if kind == "steady":
            v = base
        elif kind == "diurnal":  # one full day/night cycle compressed into `period_min`
            v = base * (1 + shape["amplitude"] * math.sin(2 * math.pi * m / shape["period_min"]))
        elif kind == "step":
            v = base * (shape["factor"] if m >= shape["at_min"] else 1)
        elif kind == "spikes":  # short bursts on a flat base, at seeded moments
            v = base
        elif kind == "growth":
            v = base * (1 + shape["per_min"] * m)
        else:
            raise ValueError(f"unknown trace kind {kind!r}")
        v *= 1 + rng.uniform(-shape.get("noise", 0.0), shape.get("noise", 0.0))
        out.append(max(0, round(v)))
    if kind == "spikes":
        for _ in range(shape["count"]):
            at = rng.randrange(5, minutes - shape["length_min"])
            for m in range(at, at + shape["length_min"]):
                out[m] = round(base * shape["factor"])
    return out


def all_traces() -> dict[str, list[int]]:
    return {name: trace(sc["load"], sc["minutes"], random.Random(f"{SEED}:{name}")) for name, sc in SCENARIOS.items()}


def write_traces(directory: Path) -> list[Path]:
    directory.mkdir(parents=True, exist_ok=True)
    written = []
    for name, rps in all_traces().items():
        path = directory / f"{name}.csv"
        path.write_text("minute,rps\n" + "".join(f"{m},{v}\n" for m, v in enumerate(rps)), encoding="utf-8", newline="\n")
        written.append(path)
    return written


# ---------------------------------------------------------------- manifests

MANIFEST = """# Scenario {name}: {description}
# Rendered by benchmarks/bench.py. The workload is deliberately configured as described; do not tune it per tool.
apiVersion: v1
kind: Namespace
metadata: {{name: {ns}, labels: {{benchmark: {name}}}}}
---
apiVersion: apps/v1
kind: Deployment
metadata: {{name: app, namespace: {ns}, labels: {{app: app}}}}
spec:
  replicas: {replicas}
  selector: {{matchLabels: {{app: app}}}}
  template:
    metadata:
      labels: {{app: app}}
      annotations: {{prometheus.io/scrape: "true", prometheus.io/port: "8080", prometheus.io/path: /metrics}}
    spec:
      containers:
      - name: app
        image: quay.io/brancz/prometheus-example-app:v0.3.0
        ports: [{{containerPort: 8080}}]
        readinessProbe: {{httpGet: {{path: /, port: 8080}}, periodSeconds: 5}}
        resources:
          requests: {{cpu: {cpu_request}, memory: {mem_request}}}
          limits: {{cpu: "{cpu_limit}", memory: {mem_limit}}}
---
apiVersion: v1
kind: Service
metadata: {{name: app, namespace: {ns}}}
spec: {{selector: {{app: app}}, ports: [{{port: 8080}}]}}
---
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata: {{name: app, namespace: {ns}}}
spec: {{minAvailable: 1, selector: {{matchLabels: {{app: app}}}}}}
---
apiVersion: v1
kind: ConfigMap
metadata: {{name: trace, namespace: {ns}}}
data:
  trace.txt: |
{trace}
---
# Open-loop generator: every second it starts as many requests as the trace says for that minute, whether or not the
# earlier ones returned, so a slow application does not slow the load down. One line of the trace is one minute.
apiVersion: apps/v1
kind: Deployment
metadata: {{name: loadgen, namespace: {ns}, labels: {{app: loadgen}}}}
spec:
  replicas: 1
  selector: {{matchLabels: {{app: loadgen}}}}
  template:
    metadata: {{labels: {{app: loadgen}}}}
    spec:
      containers:
      - name: gen
        image: busybox:1.36
        command:
        - sh
        - -c
        - |
          while read -r rps; do
            end=$(( $(date +%s) + 60 ))
            while [ "$(date +%s)" -lt "$end" ]; do
              i=0
              while [ "$i" -lt "$rps" ]; do wget -q -T 2 -O /dev/null http://app:8080/ & i=$((i+1)); done
              sleep 1
            done
          done < /trace/trace.txt
          echo "trace finished"; sleep 86400
        volumeMounts: [{{name: trace, mountPath: /trace}}]
        resources: {{requests: {{cpu: 100m, memory: 32Mi}}, limits: {{memory: 128Mi}}}}
      volumes: [{{name: trace, configMap: {{name: trace}}}}]
"""


def render(name: str, namespace: str) -> str:
    sc = SCENARIOS[name]
    rps = all_traces()[name]
    w = sc["workload"]
    return MANIFEST.format(name=name, ns=namespace, description=sc["description"], replicas=w["replicas"], cpu_request=w["cpu_request"],
                           mem_request=w["mem_request"], cpu_limit=w["cpu_limit"], mem_limit=w["mem_limit"],
                           trace="\n".join(f"    {v}" for v in rps))


# ---------------------------------------------------------------- measurements (pure)

def integrate_hours(points: list[tuple[float, float]]) -> float:
    """Trapezoid integral of (unix seconds, value) in value-hours. Gaps longer than 5 minutes are not bridged."""
    total = 0.0
    for (t0, v0), (t1, v1) in zip(points, points[1:]):
        if 0 < t1 - t0 <= 300:
            total += (v0 + v1) / 2 * (t1 - t0) / 3600
    return total


def count_changes(points: list[tuple[float, float]], tolerance: float = 1e-9) -> int:
    """How many times the value changed between consecutive samples: the number of interventions on that value."""
    return sum(1 for (_, a), (_, b) in zip(points, points[1:]) if abs(a - b) > tolerance)


def first_change_after(points: list[tuple[float, float]], after: float) -> float | None:
    """Seconds from `after` to the first sample whose value differs from the one before it. None when nothing changed."""
    for (_, a), (t1, b) in zip(points, points[1:]):
        if t1 >= after and abs(a - b) > 1e-9:
            return t1 - after
    return None


def summarise(series: dict[str, list[tuple[float, float]]], node_hourly_usd: float = 0.0, event_time: float | None = None) -> dict:
    """Turn the collected series into the benchmark's metrics. Missing series give None, never zero."""
    def hours(key):
        return integrate_hours(series[key]) if series.get(key) else None

    def peak(key):
        return max(v for _, v in series[key]) if series.get(key) else None

    def waste(req, used):
        if req is None or used is None:
            return None, None
        w = max(0.0, req - used)
        return w, (w / req * 100 if req > 0 else None)

    cpu_req, cpu_used = hours("cpu_requests"), hours("cpu_usage")
    mem_req, mem_used = hours("mem_requests"), hours("mem_usage")
    cpu_waste, cpu_waste_pct = waste(cpu_req, cpu_used)
    mem_waste, mem_waste_pct = waste(mem_req, mem_used)
    desired, unavailable = hours("replicas_desired"), hours("replicas_unavailable")
    node_hours = hours("nodes")
    out = {
        "cpu_requested_core_hours": cpu_req, "cpu_used_core_hours": cpu_used,
        "cpu_wasted_core_hours": cpu_waste, "cpu_wasted_pct": cpu_waste_pct,
        "mem_requested_gib_hours": mem_req, "mem_used_gib_hours": mem_used,
        "mem_wasted_gib_hours": mem_waste, "mem_wasted_pct": mem_waste_pct,
        "replica_changes": count_changes(series["replicas_desired"]) if series.get("replicas_desired") else None,
        "request_changes": count_changes(series["cpu_requests_per_pod"]) + count_changes(series.get("mem_requests_per_pod", []))
        if series.get("cpu_requests_per_pod") else None,
        "max_replicas": peak("replicas_desired"),
        "availability_pct": (1 - unavailable / desired) * 100 if desired and unavailable is not None else None,
        "restarts": peak("restarts"), "oom_kills": peak("oom_kills"),
        "latency_p95_s": peak("latency_p95"), "latency_p99_s": peak("latency_p99"), "error_ratio_max": peak("error_ratio"),
        "node_hours": node_hours,
        "node_cost_usd_at_list_price": node_hours * node_hourly_usd if node_hours is not None and node_hourly_usd > 0 else None,
        "tool_cpu_cores_avg": (hours("tool_cpu") / (span / 3600)) if series.get("tool_cpu") and (span := series["tool_cpu"][-1][0] - series["tool_cpu"][0][0]) > 0 else None,
        "tool_memory_mib_max": peak("tool_mem") / (1 << 20) if series.get("tool_mem") else None,
        "seconds_to_first_replica_change": None, "seconds_to_first_request_change": None,
    }
    if event_time is not None:
        if series.get("replicas_desired"):
            out["seconds_to_first_replica_change"] = first_change_after(series["replicas_desired"], event_time)
        if series.get("cpu_requests_per_pod"):
            out["seconds_to_first_request_change"] = first_change_after(series["cpu_requests_per_pod"], event_time)
    return out


# ---------------------------------------------------------------- collection

def queries(ns: str, tool_ns: str | None) -> dict[str, str]:
    sel = f'namespace="{ns}",pod=~"app-.*"'
    q = {
        "cpu_requests": f'sum(kube_pod_container_resource_requests{{{sel},resource="cpu"}})',
        "cpu_usage": f'sum(rate(container_cpu_usage_seconds_total{{{sel},container="app"}}[5m]))',
        "mem_requests": f'sum(kube_pod_container_resource_requests{{{sel},resource="memory"}}) / 2^30',
        "mem_usage": f'sum(container_memory_working_set_bytes{{{sel},container="app"}}) / 2^30',
        "cpu_requests_per_pod": f'max(kube_pod_container_resource_requests{{{sel},resource="cpu"}})',
        "mem_requests_per_pod": f'max(kube_pod_container_resource_requests{{{sel},resource="memory"}})',
        "replicas_desired": f'kube_deployment_spec_replicas{{namespace="{ns}",deployment="app"}}',
        "replicas_unavailable": f'kube_deployment_status_replicas_unavailable{{namespace="{ns}",deployment="app"}}',
        "restarts": f'sum(kube_pod_container_status_restarts_total{{{sel}}})',
        "oom_kills": f'sum(kube_pod_container_status_last_terminated_reason{{{sel},reason="OOMKilled"}})',
        "latency_p95": f'histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{{namespace="{ns}"}}[5m])))',
        "latency_p99": f'histogram_quantile(0.99, sum by (le) (rate(http_request_duration_seconds_bucket{{namespace="{ns}"}}[5m])))',
        "error_ratio": f'sum(rate(http_requests_total{{namespace="{ns}",code=~"5.."}}[5m])) / sum(rate(http_requests_total{{namespace="{ns}"}}[5m]))',
        "nodes": "count(kube_node_info)",
    }
    if tool_ns:
        q["tool_cpu"] = f'sum(rate(container_cpu_usage_seconds_total{{namespace="{tool_ns}",container!=""}}[5m]))'
        q["tool_mem"] = f'sum(container_memory_working_set_bytes{{namespace="{tool_ns}",container!=""}})'
    return q


def query_range(prom: str, query: str, start: float, end: float, step: int, token: str | None = None) -> list[tuple[float, float]]:
    url = prom.rstrip("/") + "/api/v1/query_range?" + urllib.parse.urlencode({"query": query, "start": start, "end": end, "step": step})
    req = urllib.request.Request(url, headers={"Authorization": f"Bearer {token}"} if token else {})
    with urllib.request.urlopen(req, timeout=60) as resp:  # noqa: S310 - the URL is the operator's own Prometheus
        body = json.load(resp)
    if body.get("status") != "success":
        raise RuntimeError(f"prometheus: {body.get('error', body)}")
    result = body["data"]["result"]
    if not result:
        return []
    out = []
    for t, v in result[0]["values"]:
        f = float(v)
        if math.isfinite(f):
            out.append((float(t), f))
    return out


def collect(args) -> dict:
    series = {}
    for key, q in queries(args.namespace, args.tool_namespace).items():
        series[key] = query_range(args.prom, q, args.start, args.end, args.step, args.token)
    return {
        "schema": "kubecost-auto-hpa/benchmark-result/v1",
        "kind": "MEASURED",
        "tool": args.tool, "tool_version": args.tool_version, "scenario": args.scenario,
        "environment": {"cluster": args.cluster, "kubernetes": args.kubernetes, "provider": args.provider, "node_type": args.node_type,
                        "node_hourly_usd_list_price": args.node_hourly_usd},
        "window": {"start": args.start, "end": args.end, "hours": (args.end - args.start) / 3600},
        "run": args.run, "notes": args.notes,
        "metrics": summarise(series, args.node_hourly_usd, args.event_time),
        "missing_series": sorted(k for k, v in series.items() if not v),
    }


# ---------------------------------------------------------------- comparison

ROWS = [
    ("cpu_wasted_pct", "CPU requested and unused (%)", "{:.1f}"), ("mem_wasted_pct", "Memory requested and unused (%)", "{:.1f}"),
    ("cpu_wasted_core_hours", "CPU wasted (core-hours)", "{:.2f}"), ("mem_wasted_gib_hours", "Memory wasted (GiB-hours)", "{:.2f}"),
    ("node_hours", "Node-hours", "{:.2f}"), ("node_cost_usd_at_list_price", "Node cost at list price (USD)", "{:.2f}"),
    ("seconds_to_first_replica_change", "Reaction: first replica change after the event (s)", "{:.0f}"),
    ("seconds_to_first_request_change", "Reaction: first request change after the event (s)", "{:.0f}"),
    ("replica_changes", "Interventions on replicas", "{:.0f}"), ("request_changes", "Interventions on requests", "{:.0f}"),
    ("availability_pct", "Availability of desired replicas (%)", "{:.3f}"), ("restarts", "Container restarts", "{:.0f}"),
    ("oom_kills", "OOM kills", "{:.0f}"), ("latency_p95_s", "Worst p95 latency (s)", "{:.4f}"), ("latency_p99_s", "Worst p99 latency (s)", "{:.4f}"),
    ("error_ratio_max", "Worst 5xx ratio", "{:.4f}"), ("tool_cpu_cores_avg", "Tool's own CPU (cores, average)", "{:.3f}"),
    ("tool_memory_mib_max", "Tool's own memory (MiB, peak)", "{:.0f}"),
]


def compare(results: list[dict]) -> str:
    scenarios = {r["scenario"] for r in results}
    if len(scenarios) != 1:
        raise SystemExit(f"refusing to compare different scenarios: {sorted(scenarios)}")
    head = [f"{r['tool']} {r.get('tool_version') or ''} (run {r.get('run') or '?'}, {r['kind']})".strip() for r in results]
    lines = [f"Scenario: `{scenarios.pop()}`", "", "| Metric | " + " | ".join(head) + " |", "|---|" + "---|" * len(results)]
    for key, label, fmt in ROWS:
        cells = []
        for r in results:
            v = r["metrics"].get(key)
            cells.append("not measured" if v is None else fmt.format(v))
        lines.append(f"| {label} | " + " | ".join(cells) + " |")
    envs = {json.dumps(r["environment"], sort_keys=True) for r in results}
    if len(envs) > 1:
        lines += ["", "**The runs were made in different environments: the columns are not comparable.**"]
    if any(r["kind"] != "MEASURED" for r in results):
        lines += ["", "**At least one column is not a measurement (see its kind).**"]
    return "\n".join(lines) + "\n"


# ---------------------------------------------------------------- cli

def main(argv: list[str]) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)
    sub.add_parser("traces")
    r = sub.add_parser("render")
    r.add_argument("scenario", choices=sorted(SCENARIOS))
    r.add_argument("namespace")
    c = sub.add_parser("collect")
    c.add_argument("--prom", required=True)
    c.add_argument("--token")
    c.add_argument("--namespace", required=True)
    c.add_argument("--tool-namespace", help="namespace the optimiser itself runs in, for its own resource use")
    c.add_argument("--start", type=float, required=True, help="unix seconds")
    c.add_argument("--end", type=float, required=True)
    c.add_argument("--step", type=int, default=30)
    c.add_argument("--event-time", type=float, help="unix seconds of the load change, for the reaction times")
    c.add_argument("--tool", required=True, help="what optimised the workload in this run, or 'none' for the baseline")
    c.add_argument("--tool-version")
    c.add_argument("--scenario", required=True, choices=sorted(SCENARIOS))
    c.add_argument("--cluster"), c.add_argument("--kubernetes"), c.add_argument("--provider"), c.add_argument("--node-type")
    c.add_argument("--node-hourly-usd", type=float, default=0.0)
    c.add_argument("--run"), c.add_argument("--notes")
    m = sub.add_parser("compare")
    m.add_argument("results", nargs="+")
    args = ap.parse_args(argv)

    if args.cmd == "traces":
        for p in write_traces(HERE / "traces"):
            print(p.relative_to(HERE.parent).as_posix())
    elif args.cmd == "render":
        sys.stdout.write(render(args.scenario, args.namespace))
    elif args.cmd == "collect":
        if args.end <= args.start:
            raise SystemExit("--end must be after --start")
        json.dump(collect(args), sys.stdout, indent=2)
        sys.stdout.write("\n")
    else:
        sys.stdout.write(compare([json.loads(Path(p).read_text(encoding="utf-8")) for p in args.results]))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
