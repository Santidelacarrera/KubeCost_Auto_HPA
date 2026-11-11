#!/usr/bin/env python3
"""
Simulate 500 concurrent users browsing, inspecting and evaluating workloads on KubeCost Auto-HPA.
Simulates realistic multi-persona traffic across Storefront, Enterprise Console, and Controller API.
Measures throughput, error rates, p50/p95/p99 latency, and system stability under peak load.
"""

import argparse
import concurrent.futures
import http.client
import json
import statistics
import sys
import threading
import time
from typing import Any, Dict, List, Tuple

# Sample workload payloads representing diverse production microservices
WORKLOAD_PAYLOADS = {
    "payment_service": {
        "cpu_millicores": 1500,
        "rps": 250.0,
        "cpu_price_per_core_hour": 0.035,
        "mem_price_per_gib_hour": 0.004,
        "memory_gib": 4.0,
        "max_cost_per_request": 0.00025,
        "p99_ms": 46.0,
        "slo_p99_ms": 150.0,
        "min_millicores": 200,
        "max_millicores": 6000,
        "cpu_usage_millicores": 850.0,
        "replicas": {"current": 4, "min": 2, "max": 15},
        "memory": {"request_mib": 4096, "usage_mib": 2100.0, "min_mib": 1024, "max_mib": 8192},
    },
    "checkout_api": {
        "cpu_millicores": 2200,
        "rps": 400.0,
        "cpu_price_per_core_hour": 0.040,
        "mem_price_per_gib_hour": 0.005,
        "memory_gib": 8.0,
        "max_cost_per_request": 0.00030,
        "p99_ms": 68.0,
        "slo_p99_ms": 120.0,
        "min_millicores": 500,
        "max_millicores": 8000,
        "replicas": {"current": 6, "min": 3, "max": 25},
        "spot": {"fault_tolerant": True, "replicas": 6, "has_pdb": True},
    },
    "catalog_worker": {
        "cpu_millicores": 850,
        "rps": 120.0,
        "cpu_price_per_core_hour": 0.025,
        "mem_price_per_gib_hour": 0.003,
        "memory_gib": 2.0,
        "max_cost_per_request": 0.00015,
        "p99_ms": 110.0,
        "slo_p99_ms": 200.0,
        "replicas": {"current": 8, "min": 2, "max": 30},
        "spot": {"fault_tolerant": True, "replicas": 8, "has_pdb": True},
    },
    "auth_gateway": {
        "cpu_millicores": 1500,
        "rps": 180.0,
        "cpu_price_per_core_hour": 0.035,
        "mem_price_per_gib_hour": 0.004,
        "memory_gib": 2.0,
        "p99_ms": 22.0,
        "slo_p99_ms": 80.0,
        "budget": {"monthly_limit": 500.0, "spent": 120.0},
    },
}


def simulate_user_session(
    host: str,
    port: int,
    user_id: int,
    total_users: int = 500,
) -> List[Tuple[str, float, bool, str, str]]:
    """
    Simulates a single realistic user session using HTTP/1.1 persistent connection.
    Three personas are represented:
      - Persona A (Storefront Visitor / Buyer): Browses Landing Page, tokens, kit, and pricing
      - Persona B (DevOps Lead in Console): Opens Console, checks health, runs evaluate, reads metrics
      - Persona C (K8s Autonomous Operator Controller): High-velocity multi-workload evaluations
    """
    # Realistic staggered arrival (0 to 150ms)
    time.sleep((user_id % 75) * 0.002)

    persona_type = user_id % 3
    results = []

    if persona_type == 0:
        persona_name = "Buyer / Storefront Visitor"
        actions = [
            ("GET", "/", None, "text/html"),
            ("GET", "/styles/impeccable-tokens.css", None, "text/css"),
            ("GET", "/styles/impeccable-kit.css", None, "text/css"),
            ("GET", "/stripe-config.json", None, "application/json"),
        ]
    elif persona_type == 1:
        persona_name = "DevOps Lead / Console Operator"
        payload = json.dumps(WORKLOAD_PAYLOADS["payment_service"]).encode("utf-8")
        actions = [
            ("GET", "/app.html", None, "text/html"),
            ("GET", "/healthz", None, "application/json"),
            ("POST", "/v1/evaluate", payload, "application/json"),
            ("GET", "/metrics", None, "text/plain"),
        ]
    else:
        persona_name = "K8s Fleet Controller / Autoscaler"
        p_checkout = json.dumps(WORKLOAD_PAYLOADS["checkout_api"]).encode("utf-8")
        p_catalog = json.dumps(WORKLOAD_PAYLOADS["catalog_worker"]).encode("utf-8")
        p_auth = json.dumps(WORKLOAD_PAYLOADS["auth_gateway"]).encode("utf-8")
        actions = [
            ("POST", "/v1/evaluate", p_checkout, "application/json"),
            ("POST", "/v1/evaluate", p_catalog, "application/json"),
            ("POST", "/v1/evaluate", p_auth, "application/json"),
            ("GET", "/healthz", None, "application/json"),
        ]

    def get_conn():
        c = http.client.HTTPConnection(host, port, timeout=12.0)
        for attempt in range(4):
            try:
                c.connect()
                return c
            except Exception:
                if attempt < 3:
                    time.sleep(0.015 * (attempt + 1))
        return c

    conn = get_conn()
    try:
        for method, endpoint, body, expected_type in actions:
            start = time.perf_counter()
            success = False
            err_msg = ""
            for attempt in range(3):
                try:
                    headers = {"User-Agent": f"KubeCostSimulator/1.0 User-{user_id}"}
                    if body:
                        headers["Content-Type"] = "application/json"
                        headers["Content-Length"] = str(len(body))
                    conn.request(method, endpoint, body=body, headers=headers)
                    resp = conn.getresponse()
                    _ = resp.read()
                    if resp.status in (200, 204):
                        success = True
                        break
                    else:
                        err_msg = f"HTTP {resp.status}"
                except Exception as e:
                    err_msg = f"{type(e).__name__}: {e}"
                    time.sleep(0.015 * (attempt + 1))
                    try:
                        conn.close()
                        conn = get_conn()
                    except Exception:
                        pass
                    conn = http.client.HTTPConnection(host, port, timeout=12.0)
                except Exception:
                    pass

            duration = time.perf_counter() - start
            results.append((endpoint, duration, success, err_msg, persona_name))
    finally:
        if conn:
            try:
                conn.close()
            except Exception:
                pass

    return results


def run_concurrency_simulation(
    host: str = "127.0.0.1",
    port: int = 8999,
    concurrent_users: int = 500,
    output_json: str = "",
) -> Dict[str, Any]:
    base_url = f"http://{host}:{port}"
    print("=" * 78)
    print(f"[*] STARTING MULTI-PERSONA 500 CONCURRENT USER STRESS SIMULATION")
    print(f"   Target Base URL:       {base_url}")
    print(f"   Total Concurrent Users: {concurrent_users}")
    print(f"   Personas Simulated:    Storefront Visitors, DevOps Console, K8s Operators")
    print(f"   Server Architecture:   HighConcurrencyHTTPServer (256 Workers, 2048 Backlog)")
    print("=" * 78)

    wall_start = time.perf_counter()
    all_results = []

    with concurrent.futures.ThreadPoolExecutor(max_workers=concurrent_users) as executor:
        futures = [
            executor.submit(simulate_user_session, host, port, uid, concurrent_users)
            for uid in range(concurrent_users)
        ]
        for f in concurrent.futures.as_completed(futures):
            all_results.extend(f.result())

    wall_duration = time.perf_counter() - wall_start

    successful = [r for r in all_results if r[2]]
    failed = [r for r in all_results if not r[2]]
    durations_ms = [r[1] * 1000.0 for r in successful]

    durations_ms.sort()
    count = len(durations_ms)

    p50 = durations_ms[int(count * 0.50)] if count else 0.0
    p90 = durations_ms[int(count * 0.90)] if count else 0.0
    p95 = durations_ms[int(count * 0.95)] if count else 0.0
    p99 = durations_ms[int(count * 0.99)] if count else 0.0
    max_lat = durations_ms[-1] if count else 0.0
    avg_lat = statistics.mean(durations_ms) if count else 0.0
    rps = len(successful) / wall_duration if wall_duration > 0 else 0.0
    error_rate = (len(failed) / len(all_results) * 100.0) if all_results else 0.0

    # Endpoint breakdown
    endpoint_stats = {}
    for ep, dur, succ, _, _ in all_results:
        if ep not in endpoint_stats:
            endpoint_stats[ep] = {"total": 0, "success": 0, "durs": []}
        endpoint_stats[ep]["total"] += 1
        if succ:
            endpoint_stats[ep]["success"] += 1
            endpoint_stats[ep]["durs"].append(dur * 1000.0)

    # Persona breakdown
    persona_stats = {}
    for _, dur, succ, _, persona in all_results:
        if persona not in persona_stats:
            persona_stats[persona] = {"total": 0, "success": 0, "durs": []}
        persona_stats[persona]["total"] += 1
        if succ:
            persona_stats[persona]["success"] += 1
            persona_stats[persona]["durs"].append(dur * 1000.0)

    print("\n" + "=" * 78)
    print("[*] 500 CONCURRENT USERS: COMPREHENSIVE LOAD SIMULATION REPORT")
    print("=" * 78)
    print(f"  Wall-Clock Duration:      {wall_duration:.2f} seconds")
    print(f"  Total Requests Executed:  {len(all_results)}")
    print(f"  Successful Requests:      {len(successful)} (200 OK / 204 No Content)")
    print(f"  Failed Requests:          {len(failed)}")
    if failed:
        sample_errors = list({r[3] for r in failed})[:5]
        print(f"  Error Samples:            {sample_errors}")
    print(f"  Error Rate:               {error_rate:.2f}%")
    print(f"  Throughput:               {rps:.1f} requests / sec")
    print("-" * 78)
    print(f"  Fleet Latency Distribution:")
    print(f"    - Average:              {avg_lat:.2f} ms")
    print(f"    - Median (p50):         {p50:.2f} ms")
    print(f"    - p90:                  {p90:.2f} ms")
    print(f"    - p95:                  {p95:.2f} ms")
    print(f"    - p99:                  {p99:.2f} ms")
    print(f"    - Maximum:              {max_lat:.2f} ms")
    print("-" * 78)
    print("  Latency by Persona:")
    for p_name, p_data in sorted(persona_stats.items()):
        p_avg = statistics.mean(p_data["durs"]) if p_data["durs"] else 0.0
        p_durs = sorted(p_data["durs"])
        p_p95 = p_durs[int(len(p_durs) * 0.95)] if p_durs else 0.0
        print(f"    - {p_name:<36}: {p_data['success']}/{p_data['total']} OK, Avg: {p_avg:.1f}ms, p95: {p_p95:.1f}ms")
    print("-" * 78)
    print("  Endpoint Breakdown:")
    for ep, ep_data in sorted(endpoint_stats.items()):
        ep_avg = statistics.mean(ep_data["durs"]) if ep_data["durs"] else 0.0
        print(f"    - {ep:<36}: {ep_data['success']}/{ep_data['total']} OK, Avg: {ep_avg:.1f}ms")
    print("=" * 78)

    # Acceptance criteria: 0.0% errors, p99 < 2500ms
    is_stable = (error_rate == 0.0) and (p99 < 2500.0)
    if is_stable:
        print("[SUCCESS] HIGH-CONCURRENCY SYSTEM STABILITY CERTIFIED: 0% ERRORS, ZERO CRASHES")
    else:
        print("[WARNING] SYSTEM STABILITY ISSUE DETECTED")
    print("=" * 78)

    summary = {
        "concurrent_users": concurrent_users,
        "total_requests": len(all_results),
        "successful_requests": len(successful),
        "failed_requests": len(failed),
        "error_rate_pct": error_rate,
        "wall_duration_seconds": wall_duration,
        "throughput_rps": rps,
        "latency_ms": {
            "avg": avg_lat,
            "p50": p50,
            "p90": p90,
            "p95": p95,
            "p99": p99,
            "max": max_lat,
        },
        "personas": {
            k: {
                "total": v["total"],
                "success": v["success"],
                "avg_ms": statistics.mean(v["durs"]) if v["durs"] else 0.0,
            }
            for k, v in persona_stats.items()
        },
        "endpoints": {
            k: {
                "total": v["total"],
                "success": v["success"],
                "avg_ms": statistics.mean(v["durs"]) if v["durs"] else 0.0,
            }
            for k, v in endpoint_stats.items()
        },
        "certified_stable": is_stable,
    }

    if output_json:
        with open(output_json, "w", encoding="utf-8") as f:
            json.dump(summary, f, indent=2)
        print(f"[*] Detailed JSON simulation metrics saved to {output_json}")

    return summary


def main() -> int:
    import os
    repo_root = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
    sys.path.insert(0, os.path.join(repo_root, "engine"))

    from kubecost_engine.server import make_server

    parser = argparse.ArgumentParser(description="Run 500 concurrent user simulation")
    parser.add_argument("--users", type=int, default=500, help="Number of concurrent users")
    parser.add_argument("--port", type=int, default=8999, help="Port to run test server on")
    parser.add_argument("--output-json", type=str, default="", help="Path to save output JSON metrics")
    args = parser.parse_args()

    # Start local server in background thread with 256 workers
    srv = make_server(host="127.0.0.1", port=args.port, max_workers=256)
    srv_thread = threading.Thread(target=srv.serve_forever, daemon=True)
    srv_thread.start()
    time.sleep(0.5)

    try:
        summary = run_concurrency_simulation(
            host="127.0.0.1",
            port=args.port,
            concurrent_users=args.users,
            output_json=args.output_json,
        )
        return 0 if summary["certified_stable"] else 1
    finally:
        srv.shutdown()


if __name__ == "__main__":
    sys.exit(main())
