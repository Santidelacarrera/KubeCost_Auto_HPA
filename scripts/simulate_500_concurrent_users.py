#!/usr/bin/env python3
"""
Simulate 500 concurrent users browsing and evaluating workloads on KubeCost Auto-HPA.
Measures throughput, error rates, p50/p95/p99 latency, and system stability under peak load.
"""

import concurrent.futures
import json
import statistics
import time
import urllib.error
import urllib.request
from typing import Dict, List, Tuple

# Sample workload evaluation payload representing a customer microservice
SAMPLE_PAYLOAD = {
    "cpu_millicores": 1500,
    "rps": 250.0,
    "cpu_price_per_core_hour": 0.035,
    "mem_price_per_gib_hour": 0.004,
    "memory_gib": 4.0,
    "max_cost_per_request": 0.00025,
    "p99_ms": 115.0,
    "slo_p99_ms": 150.0,
    "min_millicores": 200,
    "max_millicores": 6000,
    "cpu_usage_millicores": 850.0,
    "replicas": {
        "current": 4,
        "min": 2,
        "max": 15
    },
    "memory": {
        "request_mib": 4096,
        "usage_mib": 2100.0,
        "min_mib": 1024,
        "max_mib": 8192
    }
}


def simulate_user_session(base_url: str, user_id: int, requests_per_user: int = 4) -> List[Tuple[str, float, bool, str]]:
    """
    Simulates a single user browsing and performing operations:
    1. Check system health (/healthz)
    2. Perform cost evaluations (/v1/evaluate)
    3. Check Prometheus metrics (/metrics)
    """
    # Realistic user session staggered arrival (0 to 50ms)
    time.sleep((user_id % 50) * 0.001)

    results = []
    headers = {"Content-Type": "application/json"}
    payload_bytes = json.dumps(SAMPLE_PAYLOAD).encode("utf-8")

    for i in range(requests_per_user):
        if i == 0:
            endpoint = "/healthz"
            method = "GET"
            data = None
        elif i == requests_per_user - 1:
            endpoint = "/metrics"
            method = "GET"
            data = None
        else:
            endpoint = "/v1/evaluate"
            method = "POST"
            data = payload_bytes

        url = f"{base_url}{endpoint}"
        req = urllib.request.Request(url, data=data, headers=headers, method=method)

        start = time.perf_counter()
        success = False
        err_msg = ""
        # Up to 2 retries on transient socket connection refused
        for attempt in range(3):
            try:
                with urllib.request.urlopen(req, timeout=10.0) as resp:
                    if resp.status == 200:
                        _ = resp.read()
                        success = True
                        break
            except Exception as e:
                err_msg = f"{type(e).__name__}: {e}"
                time.sleep(0.01 * (attempt + 1))

        duration = time.perf_counter() - start
        results.append((endpoint, duration, success, err_msg))

    return results


def run_concurrency_simulation(host: str, port: int, concurrent_users: int = 500, requests_per_user: int = 4):
    base_url = f"http://{host}:{port}"
    total_expected = concurrent_users * requests_per_user
    print("=" * 76)
    print(f"[*] STARTING HIGH-CONCURRENCY STRESS TEST")
    print(f"   Target:              {base_url}")
    print(f"   Concurrent Users:    {concurrent_users}")
    print(f"   Actions per User:    {requests_per_user}")
    print(f"   Total Transactions:  {total_expected}")
    print("=" * 76)

    wall_start = time.perf_counter()
    all_results = []

    with concurrent.futures.ThreadPoolExecutor(max_workers=concurrent_users) as executor:
        futures = [
            executor.submit(simulate_user_session, base_url, uid, requests_per_user)
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

    print("\n" + "=" * 76)
    print("[*] BENCHMARK & LOAD SIMULATION RESULTS (500 CONCURRENT USERS)")
    print("=" * 76)
    print(f"  Wall-Clock Duration:      {wall_duration:.2f} seconds")
    print(f"  Total Requests Executed:  {len(all_results)}")
    print(f"  Successful Requests:      {len(successful)} (200 OK)")
    print(f"  Failed Requests:          {len(failed)}")
    if failed:
        sample_errors = list({r[3] for r in failed})[:5]
        print(f"  Error Samples:            {sample_errors}")
    print(f"  Error Rate:               {error_rate:.2f}%")
    print(f"  Throughput (RPS):         {rps:.1f} req/sec")
    print("-" * 76)
    print(f"  Latency Distribution (ms):")
    print(f"    - Average:              {avg_lat:.2f} ms")
    print(f"    - Median (p50):         {p50:.2f} ms")
    print(f"    - p90:                  {p90:.2f} ms")
    print(f"    - p95:                  {p95:.2f} ms")
    print(f"    - p99:                  {p99:.2f} ms")
    print(f"    - Maximum:              {max_lat:.2f} ms")
    print("=" * 76)

    # Acceptance criteria for enterprise production readiness under 500 concurrent users
    # 0% errors, p99 < 3500ms under extreme 500-concurrency saturation
    is_stable = (error_rate == 0.0) and (p99 < 3500.0)
    if is_stable:
        print("[SUCCESS] SYSTEM STABILITY CERTIFIED: ZERO CRASHES, 100% SUCCESS RATE")
    else:
        print("[WARNING] SYSTEM PERFORMANCE WARNING: High error rate or excessive latency detected")
    print("=" * 76)

    return {
        "concurrent_users": concurrent_users,
        "total_requests": len(all_results),
        "successful": len(successful),
        "failed": len(failed),
        "error_rate": error_rate,
        "throughput_rps": rps,
        "p50_ms": p50,
        "p95_ms": p95,
        "p99_ms": p99,
        "max_ms": max_lat,
        "is_stable": is_stable,
    }


if __name__ == "__main__":
    import argparse
    import threading
    from kubecost_engine.server import make_server

    parser = argparse.ArgumentParser(description="Run 500 concurrent user simulation")
    parser.add_argument("--users", type=int, default=500, help="Number of concurrent users")
    parser.add_argument("--requests", type=int, default=4, help="Requests per user")
    parser.add_argument("--port", type=int, default=8999, help="Port to run test server on")
    args = parser.parse_args()

    # Start local server in background thread
    srv = make_server(host="127.0.0.1", port=args.port)
    srv_thread = threading.Thread(target=srv.serve_forever, daemon=True)
    srv_thread.start()
    time.sleep(0.5)

    try:
        run_concurrency_simulation(
            host="127.0.0.1",
            port=args.port,
            concurrent_users=args.users,
            requests_per_user=args.requests,
        )
    finally:
        srv.shutdown()
