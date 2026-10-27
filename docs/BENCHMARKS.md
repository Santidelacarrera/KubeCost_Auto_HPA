# Benchmarks: native HPA vs KubeCostAutoscaler (SYNTHETIC)

> **Everything in this document is simulated.** Nothing here was measured on a cluster, with a real
> KubeCost, or with real traffic. There is **no latency, error-rate or throughput data**: a simulator with no
> request-level model cannot produce meaningful p50/p95/p99, so none are reported (the procedure to measure
> them on a real cluster is `docs/BENCHMARK_PROCEDURE.md`, and it has **not been run**). The figures compare
> controllers *with each other*, under the assumptions below. They are not savings you should expect in production.

## What is simulated

`operator/internal/sim` drives the **real decision engine** (`internal/policy`) and a re-implementation of the
documented native HPA algorithm over **identical demand traces, initial conditions and disturbances**. It is deterministic;
`go run ./cmd/simbench` prints exactly the block at the end of this file and `TestBenchmarksDocMatchesTheSimulator`
fails if the file and the simulator disagree.

### Assumptions (one fixed set for every result)

| Item | Value | Where a real system differs |
|---|---|---|
| Capacity | each *ready* pod serves 100 rps at 100% of its CPU request; utilisation = demand / (ready pods x 100) | real pods saturate non-linearly; latency rises well before 100% |
| Startup | 30 s from scale-up to ready; scale-down removes not-ready pods first, then the newest | image pulls, node provisioning, readiness probes |
| Price | $0.05 per replica-hour, constant unless the scenario says otherwise | spot/on-demand mix, node-level rounding, idle cost |
| Bounds | 2 to 60 replicas; warm start at the replicas needed for the first demand sample | |
| Evaluation period | HPA every 15 s (its sync period); KubeCostAutoscaler every 30 s (`spec.evaluationInterval` default) | |
| HPA baseline | `desired = ceil(ready x util/target)`, target 70%, tolerance 10%, scale-down = highest recommendation in the window, scale-up capped per period at `max(+4 pods, +100%)`. Two variants: the default 5 min scale-down window, and 10 min **matched to KubeCostAutoscaler** (this matched one is the baseline every other row is judged against, so a longer window is not mistaken for a better algorithm) | real HPA has more branches (unready pods, missing metrics, multiple metrics) |
| KubeCostAutoscaler | the shipped defaults: target 70%, cost-pressure 80%, ceiling 90%, tolerance 10%, scale-down 10 min, scale-up 30 s, cooldown 60 s; with a cost policy a $1.00/h target and $2.00/h budget (20 and 40 replicas at the base price) | |
| Cost signal given to the controller | the average of (replicas x price) over the cost window; *stable* only if no replica change happened inside the window, by this controller or by anyone else (the controller records the latter in `status.lastReplicaChangeTime`; before this was added only its own writes counted, and the external-write scenarios would have been optimistic) | real KubeCost may differ in lag and granularity |

### Formulas

* **Estimated infrastructure cost** = sum over time of `replicas(t) x price(t) x dt` (replicas include pods that are starting). USD, no idle, no node cost.
* **Unmet demand %** = `sum max(0, demand - readyReplicas x 100) x dt / sum demand x dt`.
* **Saturated seconds** = time with `demand > readyReplicas x 100`; **>90% util seconds** = time with `demand / capacity > 0.9`.
* **Missing replica-seconds (capacity penalty)** = `sum max(0, ceil(demand / (0.9 x 100)) - readyReplicas) x dt`. It has no price attached on purpose.
* **Reversals** = direction changes between consecutive scale actions (a proxy for oscillation).
* **Acted after last change** = seconds between the scenario's last demand/price change and the last scale action (a proxy for stabilisation time).

### Scenarios (same trace for every scaler)

Stable demand; sudden increase; ramp up and down; short (2 min) and long (5 min) spikes; gradual decline; intermittent load;
a price shock; metrics reaching the controller 90 s late; a 30 minute KubeCost outage; the controller process down for 5 minutes
and restarted (the native HPA loses its in-memory recommendations, the KubeCostAutoscaler keeps its history in the resource status;
a gap longer than the engine's MaxGap restarts its warm-up); the emergency pause during a demand increase; and another actor setting the
replicas to 2 or to 60. Details per scenario are in the tables.

## How the policy was judged

Acceptance criteria were **fixed before the results were generated** (`sim.Judge`): on every scenario a scaler must not exceed the
baseline by more than 0.25 pp of unmet demand, 120 s of saturation, 60 s of longest saturation, or 2 reversals. Cost is **not**
allowed to excuse a breach: acceptability is decided first, cost is compared only among acceptable scalers. After the first results
it became clear those criteria look at saturation and not at headroom, so an *advisory* column (extra seconds above 90% utilisation) was
added; it has no threshold, and is the reason the discussion below does not call the shipped default "free".

<!-- simbench:begin -->
Workload: 100 rps per ready pod, 30s startup, $0.05 per replica-hour, replicas 2-60, warm start. SYNTHETIC.

#### steady

constant demand.

| Scaler | Cost USD | vs baseline | Avg repl. | Max repl. | Actions | Reversals | Unmet % | Saturated s | >90% util s | Longest sat. s | Missing repl.-s | Acted after last change s |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| HPA default (down 5m) | 2.20 | +0.0% | 22.0 | 22 | 0 | 0 | 0.00 | 0 | 0 | 0 | 0 | 0 |
| HPA (down 10m) | 2.20 | - | 22.0 | 22 | 0 | 0 | 0.00 | 0 | 0 | 0 | 0 | 0 |
| KCA demand-only | 2.20 | +0.0% | 22.0 | 22 | 0 | 0 | 0.00 | 0 | 0 | 0 | 0 | 0 |
| KCA shipped (cost window 15m) | 1.93 | -12.5% | 19.3 | 22 | 1 | 0 | 0.00 | 0 | 0 | 0 | 0 | 600 |
| KCA cost window 5m | 1.93 | -12.5% | 19.3 | 22 | 1 | 0 | 0.00 | 0 | 0 | 0 | 0 | 600 |
| KCA cost window 30m | 1.93 | -12.5% | 19.3 | 22 | 1 | 0 | 0.00 | 0 | 0 | 0 | 0 | 600 |
| KCA unstable: demand-only moves (experimental, rejected) | 2.19 | -0.6% | 21.9 | 22 | 10 | 9 | 0.00 | 0 | 0 | 0 | 0 | 6900 |
| KCA unstable: scale up to cost target (experimental) | 1.93 | -12.5% | 19.3 | 22 | 1 | 0 | 0.00 | 0 | 0 | 0 | 0 | 600 |
| KCA conservative (ceiling 80%, cost target 75%) | 2.20 | +0.0% | 22.0 | 22 | 0 | 0 | 0.00 | 0 | 0 | 0 | 0 | 0 |
| KCA conservative + scale up to cost target (experimental) | 2.20 | +0.0% | 22.0 | 22 | 0 | 0 | 0.00 | 0 | 0 | 0 | 0 | 0 |
| KCA down window 5m | 1.91 | -13.1% | 19.1 | 22 | 1 | 0 | 0.00 | 0 | 0 | 0 | 0 | 300 |
| KCA down window 20m | 1.95 | -11.4% | 19.5 | 22 | 1 | 0 | 0.00 | 0 | 0 | 0 | 0 | 1200 |
| KCA fallback mode (15m) | 1.93 | -12.5% | 19.3 | 22 | 1 | 0 | 0.00 | 0 | 0 | 0 | 0 | 600 |

#### sudden-increase

demand 800 to 3000 rps at 30 min and stays.

| Scaler | Cost USD | vs baseline | Avg repl. | Max repl. | Actions | Reversals | Unmet % | Saturated s | >90% util s | Longest sat. s | Missing repl.-s | Acted after last change s |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| HPA default (down 5m) | 3.52 | +0.0% | 35.2 | 43 | 3 | 0 | 0.36 | 45 | 45 | 45 | 810 | 30 |
| HPA (down 10m) | 3.52 | - | 35.2 | 43 | 3 | 0 | 0.36 | 45 | 45 | 45 | 810 | 30 |
| KCA demand-only | 3.52 | -0.0% | 35.2 | 43 | 2 | 0 | 0.31 | 30 | 30 | 30 | 660 | 60 |
| KCA shipped (cost window 15m) | 3.09 | -12.0% | 30.9 | 38 | 3 | 0 | 0.31 | 30 | 60 | 30 | 780 | 930 |
| KCA cost window 5m | 3.13 | -11.1% | 31.3 | 38 | 3 | 0 | 0.31 | 30 | 60 | 30 | 780 | 330 |
| KCA cost window 30m | 3.04 | -13.4% | 30.4 | 38 | 3 | 0 | 0.31 | 30 | 60 | 30 | 780 | 1830 |
| KCA unstable: demand-only moves (experimental, rejected) | 3.50 | -0.3% | 35.0 | 43 | 8 | 6 | 0.31 | 30 | 60 | 30 | 780 | 4710 |
| KCA unstable: scale up to cost target (experimental) | 3.14 | -10.6% | 31.4 | 38 | 2 | 0 | 0.31 | 30 | 60 | 30 | 780 | 30 |
| KCA conservative (ceiling 80%, cost target 75%) | 3.15 | -10.6% | 31.5 | 38 | 2 | 0 | 0.31 | 30 | 60 | 30 | 720 | 30 |
| KCA conservative + scale up to cost target (experimental) | 3.29 | -6.3% | 32.9 | 40 | 2 | 0 | 0.31 | 30 | 60 | 30 | 720 | 30 |
| KCA down window 5m | 3.09 | -12.0% | 30.9 | 38 | 3 | 0 | 0.31 | 30 | 60 | 30 | 780 | 930 |
| KCA down window 20m | 3.09 | -12.0% | 30.9 | 38 | 3 | 0 | 0.31 | 30 | 60 | 30 | 780 | 930 |
| KCA fallback mode (15m) | 3.09 | -12.0% | 30.9 | 38 | 3 | 0 | 0.31 | 30 | 60 | 30 | 780 | 930 |

#### ramp-up-and-down

500 to 3000 over 40 min, hold, back down over 40 min.

| Scaler | Cost USD | vs baseline | Avg repl. | Max repl. | Actions | Reversals | Unmet % | Saturated s | >90% util s | Longest sat. s | Missing repl.-s | Acted after last change s |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| HPA default (down 5m) | 3.07 | -3.2% | 30.7 | 39 | 39 | 1 | 0.00 | 0 | 0 | 0 | 0 | 0 |
| HPA (down 10m) | 3.18 | - | 31.8 | 39 | 34 | 1 | 0.00 | 0 | 0 | 0 | 0 | 0 |
| KCA demand-only | 3.29 | +3.8% | 32.9 | 42 | 35 | 1 | 0.00 | 0 | 0 | 0 | 0 | 0 |
| KCA shipped (cost window 15m) | 3.05 | -3.9% | 30.5 | 38 | 29 | 1 | 0.00 | 0 | 1075 | 0 | 1075 | 0 |
| KCA cost window 5m | 3.00 | -5.6% | 30.0 | 38 | 29 | 1 | 0.00 | 0 | 1075 | 0 | 1075 | 0 |
| KCA cost window 30m | 2.96 | -6.7% | 29.6 | 38 | 28 | 1 | 0.00 | 0 | 1075 | 0 | 1075 | 0 |
| KCA unstable: demand-only moves (experimental, rejected) | 3.28 | +3.1% | 32.8 | 43 | 37 | 3 | 0.00 | 0 | 0 | 0 | 0 | 0 |
| KCA unstable: scale up to cost target (experimental) | 2.90 | -8.5% | 29.0 | 35 | 12 | 1 | 0.00 | 0 | 315 | 0 | 315 | 0 |
| KCA conservative (ceiling 80%, cost target 75%) | 3.22 | +1.2% | 32.2 | 38 | 30 | 1 | 0.00 | 0 | 0 | 0 | 0 | 0 |
| KCA conservative + scale up to cost target (experimental) | 3.13 | -1.5% | 31.3 | 38 | 31 | 1 | 0.00 | 0 | 0 | 0 | 0 | 0 |
| KCA down window 5m | 2.92 | -8.0% | 29.2 | 38 | 29 | 1 | 0.00 | 0 | 1075 | 0 | 1075 | 0 |
| KCA down window 20m | 3.09 | -2.6% | 30.9 | 38 | 28 | 1 | 0.00 | 0 | 1075 | 0 | 1075 | 0 |
| KCA fallback mode (15m) | 3.05 | -3.9% | 30.5 | 38 | 29 | 1 | 0.00 | 0 | 1075 | 0 | 1075 | 0 |

#### short-spikes

three 2-minute spikes to 3000 rps over a 800 rps base.

| Scaler | Cost USD | vs baseline | Avg repl. | Max repl. | Actions | Reversals | Unmet % | Saturated s | >90% util s | Longest sat. s | Missing repl.-s | Acted after last change s |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| HPA default (down 5m) | 1.73 | -18.3% | 17.3 | 43 | 12 | 5 | 2.88 | 135 | 135 | 45 | 2430 | 300 |
| HPA (down 10m) | 2.11 | - | 21.1 | 43 | 12 | 5 | 2.88 | 135 | 135 | 45 | 2430 | 600 |
| KCA demand-only | 2.11 | -0.1% | 21.1 | 43 | 9 | 5 | 2.47 | 90 | 90 | 30 | 1980 | 600 |
| KCA shipped (cost window 15m) | 2.59 | +22.8% | 25.9 | 34 | 9 | 5 | 2.75 | 150 | 180 | 60 | 2520 | 1410 |
| KCA cost window 5m | 2.05 | -3.1% | 20.5 | 34 | 9 | 5 | 2.47 | 90 | 180 | 30 | 2340 | 810 |
| KCA cost window 30m | 2.66 | +25.7% | 26.6 | 38 | 4 | 1 | 0.82 | 30 | 60 | 30 | 780 | 600 |
| KCA unstable: demand-only moves (experimental, rejected) | 2.11 | +0.0% | 21.1 | 43 | 9 | 5 | 2.47 | 90 | 180 | 30 | 2340 | 600 |
| KCA unstable: scale up to cost target (experimental) | 2.20 | +4.0% | 22.0 | 38 | 9 | 5 | 2.47 | 90 | 180 | 30 | 2340 | 810 |
| KCA conservative (ceiling 80%, cost target 75%) | 2.85 | +34.8% | 28.5 | 38 | 9 | 5 | 2.47 | 90 | 180 | 30 | 2280 | 1410 |
| KCA conservative + scale up to cost target (experimental) | 2.28 | +7.7% | 22.8 | 40 | 9 | 5 | 2.47 | 90 | 180 | 30 | 2160 | 810 |
| KCA down window 5m | 2.32 | +9.8% | 23.2 | 34 | 9 | 5 | 2.75 | 150 | 180 | 60 | 2520 | 1110 |
| KCA down window 20m | 2.87 | +36.0% | 28.7 | 38 | 4 | 1 | 0.82 | 30 | 60 | 30 | 780 | 1200 |
| KCA fallback mode (15m) | 2.59 | +22.8% | 25.9 | 34 | 9 | 5 | 2.75 | 150 | 180 | 60 | 2520 | 1410 |

#### burst

two 5-minute spikes to 3200 rps over a 800 rps base.

| Scaler | Cost USD | vs baseline | Avg repl. | Max repl. | Actions | Reversals | Unmet % | Saturated s | >90% util s | Longest sat. s | Missing repl.-s | Acted after last change s |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| HPA default (down 5m) | 1.75 | -13.9% | 17.5 | 46 | 8 | 3 | 2.00 | 90 | 120 | 45 | 1830 | 300 |
| HPA (down 10m) | 2.04 | - | 20.4 | 46 | 8 | 3 | 2.00 | 90 | 120 | 45 | 1830 | 600 |
| KCA demand-only | 2.04 | +0.2% | 20.4 | 46 | 6 | 3 | 1.67 | 60 | 120 | 30 | 1500 | 600 |
| KCA shipped (cost window 15m) | 2.21 | +8.8% | 22.1 | 36 | 6 | 3 | 1.83 | 120 | 120 | 60 | 1800 | 1230 |
| KCA cost window 5m | 1.81 | -10.9% | 18.1 | 36 | 6 | 3 | 1.83 | 120 | 120 | 60 | 1800 | 630 |
| KCA cost window 30m | 2.81 | +38.2% | 28.1 | 36 | 6 | 3 | 1.96 | 120 | 120 | 60 | 1890 | 2130 |
| KCA unstable: demand-only moves (experimental, rejected) | 2.04 | +0.0% | 20.4 | 46 | 6 | 3 | 1.83 | 120 | 120 | 60 | 1800 | 600 |
| KCA unstable: scale up to cost target (experimental) | 1.91 | -6.0% | 19.1 | 40 | 6 | 3 | 1.83 | 120 | 120 | 60 | 1800 | 630 |
| KCA conservative (ceiling 80%, cost target 75%) | 2.38 | +17.0% | 23.8 | 40 | 6 | 3 | 1.67 | 60 | 120 | 30 | 1680 | 1230 |
| KCA conservative + scale up to cost target (experimental) | 1.99 | -2.2% | 19.9 | 43 | 6 | 3 | 1.67 | 60 | 120 | 30 | 1680 | 630 |
| KCA down window 5m | 2.01 | -1.0% | 20.1 | 36 | 6 | 3 | 1.83 | 120 | 120 | 60 | 1800 | 930 |
| KCA down window 20m | 2.61 | +28.4% | 26.1 | 36 | 6 | 3 | 1.96 | 120 | 120 | 60 | 1890 | 1830 |
| KCA fallback mode (15m) | 2.21 | +8.8% | 22.1 | 36 | 6 | 3 | 1.83 | 120 | 120 | 60 | 1800 | 1230 |

#### gradual-decline

3000 down to 500 rps over an hour.

| Scaler | Cost USD | vs baseline | Avg repl. | Max repl. | Actions | Reversals | Unmet % | Saturated s | >90% util s | Longest sat. s | Missing repl.-s | Acted after last change s |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| HPA default (down 5m) | 2.43 | -5.1% | 24.3 | 43 | 27 | 0 | 0.00 | 0 | 0 | 0 | 0 | 225 |
| HPA (down 10m) | 2.57 | - | 25.7 | 43 | 32 | 0 | 0.00 | 0 | 0 | 0 | 0 | 525 |
| KCA demand-only | 2.57 | +0.1% | 25.7 | 43 | 32 | 0 | 0.00 | 0 | 0 | 0 | 0 | 540 |
| KCA shipped (cost window 15m) | 2.65 | +3.4% | 26.5 | 43 | 5 | 0 | 0.00 | 0 | 0 | 0 | 0 | 1890 |
| KCA cost window 5m | 2.54 | -0.8% | 25.4 | 43 | 6 | 0 | 0.00 | 0 | 0 | 0 | 0 | 990 |
| KCA cost window 30m | 2.76 | +7.6% | 27.6 | 43 | 3 | 0 | 0.00 | 0 | 0 | 0 | 0 | 600 |
| KCA unstable: demand-only moves (experimental, rejected) | 2.55 | -0.5% | 25.5 | 43 | 28 | 2 | 0.00 | 0 | 0 | 0 | 0 | 540 |
| KCA unstable: scale up to cost target (experimental) | 2.51 | -2.3% | 25.1 | 43 | 6 | 0 | 0.00 | 0 | 0 | 0 | 0 | 990 |
| KCA conservative (ceiling 80%, cost target 75%) | 2.72 | +6.1% | 27.2 | 43 | 5 | 0 | 0.00 | 0 | 0 | 0 | 0 | 1950 |
| KCA conservative + scale up to cost target (experimental) | 2.61 | +1.9% | 26.1 | 43 | 6 | 0 | 0.00 | 0 | 0 | 0 | 0 | 1050 |
| KCA down window 5m | 2.46 | -4.1% | 24.6 | 43 | 5 | 0 | 0.00 | 0 | 0 | 0 | 0 | 690 |
| KCA down window 20m | 3.05 | +19.0% | 30.5 | 43 | 3 | 0 | 0.00 | 0 | 0 | 0 | 0 | 600 |
| KCA fallback mode (15m) | 2.65 | +3.4% | 26.5 | 43 | 5 | 0 | 0.00 | 0 | 0 | 0 | 0 | 1890 |

#### intermittent

square wave 400/2000 rps, 10 min each.

| Scaler | Cost USD | vs baseline | Avg repl. | Max repl. | Actions | Reversals | Unmet % | Saturated s | >90% util s | Longest sat. s | Missing repl.-s | Acted after last change s |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| HPA default (down 5m) | 2.19 | -18.8% | 21.9 | 29 | 23 | 10 | 3.96 | 360 | 360 | 60 | 4500 | 30 |
| HPA (down 10m) | 2.70 | - | 27.0 | 29 | 3 | 0 | 0.66 | 60 | 60 | 60 | 750 | 0 |
| KCA demand-only | 2.70 | +0.0% | 27.0 | 29 | 2 | 0 | 0.56 | 60 | 60 | 60 | 660 | 0 |
| KCA shipped (cost window 15m) | 2.16 | -20.2% | 21.6 | 23 | 2 | 0 | 0.56 | 60 | 60 | 60 | 660 | 0 |
| KCA cost window 5m | 2.16 | -20.2% | 21.6 | 23 | 2 | 0 | 0.56 | 60 | 60 | 60 | 660 | 0 |
| KCA cost window 30m | 2.16 | -20.2% | 21.6 | 23 | 2 | 0 | 0.56 | 60 | 60 | 60 | 660 | 0 |
| KCA unstable: demand-only moves (experimental, rejected) | 2.69 | -0.6% | 26.9 | 29 | 12 | 10 | 0.56 | 60 | 60 | 60 | 660 | 60 |
| KCA unstable: scale up to cost target (experimental) | 2.34 | -13.5% | 23.4 | 25 | 2 | 0 | 0.56 | 60 | 60 | 60 | 660 | 0 |
| KCA conservative (ceiling 80%, cost target 75%) | 2.34 | -13.5% | 23.4 | 25 | 2 | 0 | 0.56 | 60 | 60 | 60 | 660 | 0 |
| KCA conservative + scale up to cost target (experimental) | 2.52 | -6.7% | 25.2 | 27 | 2 | 0 | 0.56 | 60 | 60 | 60 | 660 | 0 |
| KCA down window 5m | 2.01 | -25.7% | 20.1 | 23 | 8 | 4 | 1.94 | 180 | 180 | 60 | 2220 | 0 |
| KCA down window 20m | 2.16 | -20.2% | 21.6 | 23 | 2 | 0 | 0.56 | 60 | 60 | 60 | 660 | 0 |
| KCA fallback mode (15m) | 2.16 | -20.2% | 21.6 | 23 | 2 | 0 | 0.56 | 60 | 60 | 60 | 660 | 0 |

#### price-shock

constant demand; replica price doubles from 40 to 80 min (capacity reclaimed, on-demand fallback).

| Scaler | Cost USD | vs baseline | Avg repl. | Max repl. | Actions | Reversals | Unmet % | Saturated s | >90% util s | Longest sat. s | Missing repl.-s | Acted after last change s |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| HPA default (down 5m) | 2.93 | +0.0% | 22.0 | 22 | 0 | 0 | 0.00 | 0 | 0 | 0 | 0 | 0 |
| HPA (down 10m) | 2.93 | - | 22.0 | 22 | 0 | 0 | 0.00 | 0 | 0 | 0 | 0 | 0 |
| KCA demand-only | 2.93 | +0.0% | 22.0 | 22 | 0 | 0 | 0.00 | 0 | 0 | 0 | 0 | 0 |
| KCA shipped (cost window 15m) | 2.56 | -12.8% | 19.3 | 22 | 1 | 0 | 0.00 | 0 | 0 | 0 | 0 | 0 |
| KCA cost window 5m | 2.56 | -12.8% | 19.3 | 22 | 1 | 0 | 0.00 | 0 | 0 | 0 | 0 | 0 |
| KCA cost window 30m | 2.56 | -12.8% | 19.3 | 22 | 1 | 0 | 0.00 | 0 | 0 | 0 | 0 | 0 |
| KCA unstable: demand-only moves (experimental, rejected) | 2.92 | -0.5% | 21.9 | 22 | 10 | 9 | 0.00 | 0 | 0 | 0 | 0 | 2100 |
| KCA unstable: scale up to cost target (experimental) | 2.56 | -12.8% | 19.3 | 22 | 1 | 0 | 0.00 | 0 | 0 | 0 | 0 | 0 |
| KCA conservative (ceiling 80%, cost target 75%) | 2.87 | -2.1% | 21.5 | 22 | 1 | 0 | 0.00 | 0 | 0 | 0 | 0 | 0 |
| KCA conservative + scale up to cost target (experimental) | 2.87 | -2.1% | 21.5 | 22 | 1 | 0 | 0.00 | 0 | 0 | 0 | 0 | 0 |
| KCA down window 5m | 2.55 | -13.2% | 19.1 | 22 | 1 | 0 | 0.00 | 0 | 0 | 0 | 0 | 0 |
| KCA down window 20m | 2.58 | -11.9% | 19.5 | 22 | 1 | 0 | 0.00 | 0 | 0 | 0 | 0 | 0 |
| KCA fallback mode (15m) | 2.56 | -12.8% | 19.3 | 22 | 1 | 0 | 0.00 | 0 | 0 | 0 | 0 | 0 |

#### metrics-lag

the 5-minute spikes again, but the controller sees demand 90 s late.

| Scaler | Cost USD | vs baseline | Avg repl. | Max repl. | Actions | Reversals | Unmet % | Saturated s | >90% util s | Longest sat. s | Missing repl.-s | Acted after last change s |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| HPA default (down 5m) | 1.75 | -13.9% | 17.5 | 46 | 8 | 3 | 7.00 | 270 | 300 | 135 | 6150 | 390 |
| HPA (down 10m) | 2.04 | - | 20.4 | 46 | 8 | 3 | 7.00 | 270 | 300 | 135 | 6150 | 690 |
| KCA demand-only | 2.04 | +0.2% | 20.4 | 46 | 6 | 3 | 6.67 | 240 | 300 | 120 | 5820 | 690 |
| KCA shipped (cost window 15m) | 2.21 | +8.8% | 22.1 | 36 | 6 | 3 | 6.83 | 300 | 300 | 150 | 6120 | 1320 |
| KCA cost window 5m | 1.81 | -10.9% | 18.1 | 36 | 6 | 3 | 6.83 | 300 | 300 | 150 | 6120 | 720 |
| KCA cost window 30m | 2.81 | +38.2% | 28.1 | 36 | 6 | 3 | 6.96 | 300 | 300 | 150 | 6210 | 2220 |
| KCA unstable: demand-only moves (experimental, rejected) | 2.04 | +0.0% | 20.4 | 46 | 6 | 3 | 6.83 | 300 | 300 | 150 | 6120 | 690 |
| KCA unstable: scale up to cost target (experimental) | 1.91 | -6.0% | 19.1 | 40 | 6 | 3 | 6.83 | 300 | 300 | 150 | 6120 | 720 |
| KCA conservative (ceiling 80%, cost target 75%) | 2.38 | +17.0% | 23.8 | 40 | 6 | 3 | 6.67 | 240 | 300 | 120 | 6000 | 1320 |
| KCA conservative + scale up to cost target (experimental) | 1.99 | -2.2% | 19.9 | 43 | 6 | 3 | 6.67 | 240 | 300 | 120 | 6000 | 720 |
| KCA down window 5m | 2.01 | -1.0% | 20.1 | 36 | 6 | 3 | 6.83 | 300 | 300 | 150 | 6120 | 1020 |
| KCA down window 20m | 2.61 | +28.4% | 26.1 | 36 | 6 | 3 | 6.96 | 300 | 300 | 150 | 6210 | 1920 |
| KCA fallback mode (15m) | 2.21 | +8.8% | 22.1 | 36 | 6 | 3 | 6.83 | 300 | 300 | 150 | 6120 | 1320 |

#### kubecost-outage

ramp; KubeCost unreachable from 40 to 70 min (cost-free scalers are unaffected).

| Scaler | Cost USD | vs baseline | Avg repl. | Max repl. | Actions | Reversals | Unmet % | Saturated s | >90% util s | Longest sat. s | Missing repl.-s | Acted after last change s |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| HPA default (down 5m) | 3.07 | -3.2% | 30.7 | 39 | 39 | 1 | 0.00 | 0 | 0 | 0 | 0 | 0 |
| HPA (down 10m) | 3.18 | - | 31.8 | 39 | 34 | 1 | 0.00 | 0 | 0 | 0 | 0 | 0 |
| KCA demand-only | 3.29 | +3.8% | 32.9 | 42 | 35 | 1 | 0.00 | 0 | 0 | 0 | 0 | 0 |
| KCA shipped (cost window 15m) | 3.01 | -5.2% | 30.1 | 38 | 28 | 1 | 0.00 | 0 | 1075 | 0 | 1075 | 0 |
| KCA cost window 5m | 2.91 | -8.3% | 29.1 | 38 | 29 | 1 | 0.00 | 0 | 1075 | 0 | 1075 | 0 |
| KCA cost window 30m | 2.96 | -6.7% | 29.6 | 38 | 28 | 1 | 0.00 | 0 | 1075 | 0 | 1075 | 0 |
| KCA unstable: demand-only moves (experimental, rejected) | 3.29 | +3.6% | 32.9 | 42 | 37 | 3 | 0.00 | 0 | 0 | 0 | 0 | 0 |
| KCA unstable: scale up to cost target (experimental) | 2.90 | -8.5% | 29.0 | 35 | 12 | 1 | 0.00 | 0 | 315 | 0 | 315 | 0 |
| KCA conservative (ceiling 80%, cost target 75%) | 3.22 | +1.2% | 32.2 | 38 | 30 | 1 | 0.00 | 0 | 0 | 0 | 0 | 0 |
| KCA conservative + scale up to cost target (experimental) | 3.13 | -1.5% | 31.3 | 38 | 31 | 1 | 0.00 | 0 | 0 | 0 | 0 | 0 |
| KCA down window 5m | 2.88 | -9.4% | 28.8 | 38 | 29 | 1 | 0.00 | 0 | 1075 | 0 | 1075 | 0 |
| KCA down window 20m | 3.04 | -4.1% | 30.4 | 38 | 28 | 1 | 0.00 | 0 | 1075 | 0 | 1075 | 0 |
| KCA fallback mode (15m) | 3.21 | +1.2% | 32.1 | 43 | 29 | 1 | 0.00 | 0 | 1075 | 0 | 1075 | 0 |

#### controller-restart

gradual decline; the controller is down from 40 to 45 min and restarts.

| Scaler | Cost USD | vs baseline | Avg repl. | Max repl. | Actions | Reversals | Unmet % | Saturated s | >90% util s | Longest sat. s | Missing repl.-s | Acted after last change s |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| HPA default (down 5m) | 2.43 | -4.6% | 24.3 | 43 | 22 | 0 | 0.00 | 0 | 0 | 0 | 0 | 225 |
| HPA (down 10m) | 2.55 | - | 25.5 | 43 | 22 | 0 | 0.00 | 0 | 0 | 0 | 0 | 525 |
| KCA demand-only | 2.63 | +3.2% | 26.3 | 43 | 23 | 0 | 0.00 | 0 | 0 | 0 | 0 | 540 |
| KCA shipped (cost window 15m) | 2.65 | +4.1% | 26.5 | 43 | 5 | 0 | 0.00 | 0 | 0 | 0 | 0 | 1890 |
| KCA cost window 5m | 2.57 | +1.0% | 25.7 | 43 | 6 | 0 | 0.00 | 0 | 0 | 0 | 0 | 1200 |
| KCA cost window 30m | 2.79 | +9.3% | 27.9 | 43 | 3 | 0 | 0.00 | 0 | 0 | 0 | 0 | 900 |
| KCA unstable: demand-only moves (experimental, rejected) | 2.57 | +0.7% | 25.7 | 43 | 22 | 2 | 0.00 | 0 | 0 | 0 | 0 | 540 |
| KCA unstable: scale up to cost target (experimental) | 2.51 | -1.5% | 25.1 | 43 | 6 | 0 | 0.00 | 0 | 0 | 0 | 0 | 1200 |
| KCA conservative (ceiling 80%, cost target 75%) | 2.72 | +6.9% | 27.2 | 43 | 5 | 0 | 0.00 | 0 | 0 | 0 | 0 | 1950 |
| KCA conservative + scale up to cost target (experimental) | 2.62 | +2.9% | 26.2 | 43 | 6 | 0 | 0.00 | 0 | 0 | 0 | 0 | 1200 |
| KCA down window 5m | 2.46 | -3.4% | 24.6 | 43 | 5 | 0 | 0.00 | 0 | 0 | 0 | 0 | 690 |
| KCA down window 20m | 3.01 | +18.0% | 30.1 | 43 | 3 | 0 | 0.00 | 0 | 0 | 0 | 0 | 1200 |
| KCA fallback mode (15m) | 2.65 | +4.1% | 26.5 | 43 | 5 | 0 | 0.00 | 0 | 0 | 0 | 0 | 1890 |

#### pause-then-resume

sudden increase at 30 min while the emergency pause is on from 25 to 55 min.

| Scaler | Cost USD | vs baseline | Avg repl. | Max repl. | Actions | Reversals | Unmet % | Saturated s | >90% util s | Longest sat. s | Missing repl.-s | Acted after last change s |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| HPA default (down 5m) | 2.87 | +0.0% | 28.7 | 43 | 3 | 0 | 15.66 | 1545 | 1545 | 1545 | 33810 | 30 |
| HPA (down 10m) | 2.87 | - | 28.7 | 43 | 3 | 0 | 15.66 | 1545 | 1545 | 1545 | 33810 | 30 |
| KCA demand-only | 2.87 | -0.0% | 28.7 | 43 | 2 | 0 | 15.61 | 1530 | 1530 | 1530 | 33660 | 60 |
| KCA shipped (cost window 15m) | 2.55 | -11.1% | 25.5 | 38 | 3 | 0 | 15.61 | 1530 | 1560 | 1530 | 33780 | 930 |
| KCA cost window 5m | 2.59 | -9.9% | 25.9 | 38 | 3 | 0 | 15.61 | 1530 | 1560 | 1530 | 33780 | 330 |
| KCA cost window 30m | 2.50 | -12.8% | 25.0 | 38 | 3 | 0 | 15.61 | 1530 | 1560 | 1530 | 33780 | 1830 |
| KCA unstable: demand-only moves (experimental, rejected) | 2.86 | -0.3% | 28.6 | 43 | 6 | 4 | 15.61 | 1530 | 1560 | 1530 | 33780 | 3150 |
| KCA unstable: scale up to cost target (experimental) | 2.60 | -9.3% | 26.0 | 38 | 2 | 0 | 15.61 | 1530 | 1560 | 1530 | 33780 | 30 |
| KCA conservative (ceiling 80%, cost target 75%) | 2.60 | -9.3% | 26.0 | 38 | 2 | 0 | 15.61 | 1530 | 1560 | 1530 | 33720 | 30 |
| KCA conservative + scale up to cost target (experimental) | 2.71 | -5.6% | 27.1 | 40 | 2 | 0 | 15.61 | 1530 | 1560 | 1530 | 33720 | 30 |
| KCA down window 5m | 2.55 | -11.1% | 25.5 | 38 | 3 | 0 | 15.61 | 1530 | 1560 | 1530 | 33780 | 930 |
| KCA down window 20m | 2.55 | -11.1% | 25.5 | 38 | 3 | 0 | 15.61 | 1530 | 1560 | 1530 | 33780 | 930 |
| KCA fallback mode (15m) | 2.55 | -11.1% | 25.5 | 38 | 3 | 0 | 15.61 | 1530 | 1560 | 1530 | 33780 | 930 |

#### external-scale-down

constant demand; another actor sets replicas to 2 at 40 min.

| Scaler | Cost USD | vs baseline | Avg repl. | Max repl. | Actions | Reversals | Unmet % | Saturated s | >90% util s | Longest sat. s | Missing repl.-s | Acted after last change s |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| HPA default (down 5m) | 2.19 | +0.0% | 21.9 | 22 | 4 | 0 | 0.65 | 75 | 75 | 75 | 855 | 60 |
| HPA (down 10m) | 2.19 | - | 21.9 | 22 | 4 | 0 | 0.65 | 75 | 75 | 75 | 855 | 60 |
| KCA demand-only | 2.19 | -0.0% | 21.9 | 22 | 3 | 0 | 0.61 | 60 | 60 | 60 | 780 | 90 |
| KCA shipped (cost window 15m) | 1.89 | -13.6% | 18.9 | 22 | 5 | 1 | 0.72 | 90 | 90 | 90 | 960 | 960 |
| KCA cost window 5m | 1.91 | -12.9% | 19.1 | 22 | 5 | 1 | 0.72 | 90 | 90 | 90 | 960 | 360 |
| KCA cost window 30m | 1.87 | -14.8% | 18.7 | 22 | 5 | 1 | 0.72 | 90 | 90 | 90 | 960 | 1860 |
| KCA unstable: demand-only moves (experimental, rejected) | 2.18 | -0.6% | 21.8 | 22 | 13 | 9 | 0.61 | 60 | 60 | 60 | 780 | 4770 |
| KCA unstable: scale up to cost target (experimental) | 1.92 | -12.5% | 19.2 | 22 | 4 | 1 | 0.69 | 90 | 90 | 90 | 930 | 60 |
| KCA conservative (ceiling 80%, cost target 75%) | 1.99 | -9.1% | 19.9 | 22 | 3 | 0 | 0.69 | 90 | 90 | 90 | 930 | 60 |
| KCA conservative + scale up to cost target (experimental) | 2.06 | -6.0% | 20.6 | 22 | 3 | 0 | 0.61 | 60 | 90 | 60 | 810 | 60 |
| KCA down window 5m | 1.88 | -14.2% | 18.8 | 22 | 5 | 1 | 0.72 | 90 | 90 | 90 | 960 | 960 |
| KCA down window 20m | 1.92 | -12.5% | 19.2 | 22 | 5 | 1 | 0.72 | 90 | 90 | 90 | 960 | 960 |
| KCA fallback mode (15m) | 1.89 | -13.6% | 18.9 | 22 | 5 | 1 | 0.72 | 90 | 90 | 90 | 960 | 960 |

#### external-scale-up

constant demand; another actor sets replicas to 60 at 40 min.

| Scaler | Cost USD | vs baseline | Avg repl. | Max repl. | Actions | Reversals | Unmet % | Saturated s | >90% util s | Longest sat. s | Missing repl.-s | Acted after last change s |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| HPA default (down 5m) | 2.38 | -6.2% | 23.8 | 60 | 1 | 0 | 0.00 | 0 | 0 | 0 | 0 | 330 |
| HPA (down 10m) | 2.54 | - | 25.4 | 60 | 1 | 0 | 0.00 | 0 | 0 | 0 | 0 | 630 |
| KCA demand-only | 2.54 | +0.0% | 25.4 | 60 | 1 | 0 | 0.00 | 0 | 0 | 0 | 0 | 630 |
| KCA shipped (cost window 15m) | 2.78 | +9.7% | 27.8 | 60 | 2 | 0 | 0.00 | 0 | 0 | 0 | 0 | 1500 |
| KCA cost window 5m | 2.44 | -3.7% | 24.4 | 60 | 2 | 0 | 0.00 | 0 | 0 | 0 | 0 | 900 |
| KCA cost window 30m | 3.29 | +30.0% | 32.9 | 60 | 2 | 0 | 0.00 | 0 | 0 | 0 | 0 | 2400 |
| KCA unstable: demand-only moves (experimental, rejected) | 2.53 | -0.4% | 25.3 | 60 | 9 | 7 | 0.00 | 0 | 0 | 0 | 0 | 3750 |
| KCA unstable: scale up to cost target (experimental) | 2.44 | -3.7% | 24.4 | 60 | 2 | 0 | 0.00 | 0 | 0 | 0 | 0 | 900 |
| KCA conservative (ceiling 80%, cost target 75%) | 2.90 | +14.5% | 29.0 | 60 | 1 | 0 | 0.00 | 0 | 0 | 0 | 0 | 1500 |
| KCA conservative + scale up to cost target (experimental) | 2.57 | +1.4% | 25.7 | 60 | 1 | 0 | 0.00 | 0 | 0 | 0 | 0 | 900 |
| KCA down window 5m | 2.60 | +2.5% | 26.0 | 60 | 2 | 0 | 0.00 | 0 | 0 | 0 | 0 | 1200 |
| KCA down window 20m | 3.15 | +24.2% | 31.5 | 60 | 2 | 0 | 0.00 | 0 | 0 | 0 | 0 | 2100 |
| KCA fallback mode (15m) | 2.78 | +9.7% | 27.8 | 60 | 2 | 0 | 0.00 | 0 | 0 | 0 | 0 | 1500 |

#### Summary against the baseline (HPA (down 10m))

Acceptance criteria, fixed before the results were produced: on every scenario a scaler must not exceed the baseline by more than 0.25 pp unmet demand, 120 s saturated, 60 s longest saturation, or 2 reversals.

| Scaler | Total cost USD | vs baseline | Scenarios cheaper (>2%) | Scenarios dearer (>2%) | Total actions | Total reversals | Extra s above 90% util (advisory) | Acceptable | Breaches |
|---|---|---|---|---|---|---|---|---|---|
| HPA default (down 5m) | 34.53 | -5.7% | 9 | 0 | 189 | 23 | +300 | NO | intermittent: unmet demand +3.30 pp; intermittent: saturated +300 s; intermittent: reversals +10 |
| HPA (down 10m) | 36.60 | - | 0 | 0 | 164 | 13 | +0 | (baseline) | - |
| KCA demand-only | 36.93 | +0.9% | 0 | 3 | 156 | 13 | -90 | yes | - |
| KCA shipped (cost window 15m) | 35.35 | -3.4% | 8 | 6 | 105 | 14 | +2240 | yes | - |
| KCA cost window 5m | 33.41 | -8.7% | 12 | 0 | 108 | 14 | +2240 | yes | - |
| KCA cost window 30m | 37.10 | +1.4% | 8 | 6 | 95 | 10 | +2120 | yes | - |
| KCA unstable: demand-only moves (experimental, rejected) | 36.73 | +0.4% | 0 | 2 | 213 | 75 | +60 | NO | steady: reversals +9; sudden-increase: reversals +6; intermittent: reversals +10; price-shock: reversals +9; pause-then-resume: reversals +4; external-scale-down: reversals +9; external-scale-up: reversals +7 |
| KCA unstable: scale up to cost target (experimental) | 33.78 | -7.7% | 12 | 1 | 71 | 14 | +720 | yes | - |
| KCA conservative (ceiling 80%, cost target 75%) | 37.54 | +2.6% | 5 | 6 | 102 | 13 | +90 | yes | - |
| KCA conservative + scale up to cost target (experimental) | 35.98 | -1.7% | 7 | 2 | 106 | 13 | +90 | yes | - |
| KCA down window 5m | 33.66 | -8.0% | 10 | 2 | 112 | 18 | +2360 | NO | intermittent: unmet demand +1.28 pp; intermittent: reversals +4 |
| KCA down window 20m | 37.70 | +3.0% | 8 | 6 | 95 | 10 | +2120 | yes | - |
| KCA fallback mode (15m) | 35.56 | -2.8% | 7 | 6 | 106 | 14 | +2240 | yes | - |

The "extra s above 90% util" column is advisory and has no threshold: it is the sum, over all scenarios, of the seconds a scaler spent above 90% utilisation beyond the baseline. It was added after the first results showed that the pre-declared criteria (which look at saturation, not at headroom) do not see a controller that rides the ceiling.

<!-- simbench:end -->

## What the results say (and what they do not)

Every statement below can be checked in the tables above.

1. **Without cost, the engine is an HPA with a different shape, not a better one.** `KCA demand-only` against the matched
   `HPA (down 10m)`: total cost +0.9%, the same reversals, and it acts less often. Against the *default* HPA the visible gain in
   `intermittent` (fewer reversals, less unmet demand) comes from the longer window: the matched HPA gets it too.
2. **The cost policy saves money by running with less headroom.** The shipped configuration (`KCA shipped`) is cheaper than the matched
   HPA in some scenarios and **dearer in others** (see "cheaper/dearer" columns; it costs more in several traces, e.g. short spikes,
   bursts and external scale-ups). It never saturated more than the baseline beyond the criteria, **but it spent far more time above
   90% utilisation** (advisory column), concentrated in `ramp-up-and-down` and `kubecost-outage`. Cause (confirmed on a timeline): during any
   continuous change of replicas the cost signal is "unstable" for a full cost window, in which only safety scale-ups are allowed, so the
   count tracks the 90% ceiling instead of the 70% target.
3. **The cost window length does not fix that.** `cost window 5m` and `30m` carry about the same headroom penalty as 15 min
   (+2240 s and +2120 s against +2240 s); only total cost moves (-8.7% and +1.4%). The simulation does not say why the totals differ.
4. **Rejected alternatives.** `scale-down window 5m` fails the pre-declared criteria (flapping under the square wave: more unmet demand and
   reversals). `unstable: demand-only moves` fails them in 7 of 14 scenarios (75 reversals in total): its demand-only moves while the cost
   is unstable fight the cost-pressure target once the signal is stable again.
5. **Two alternatives look better in simulation and are NOT shipped.** `unstable: scale up to cost target` (experimental, off, not exposed in the
   CRD) cuts the extra time above 90% utilisation by roughly two thirds and has the fewest actions, at the price of not enforcing the budget while the
   signal is unstable. `conservative (ceiling 80%, cost target 75%)` uses only existing CRD fields and removes the headroom penalty almost entirely
   (+90 s), but its total cost is +2.6% against the matched HPA: **on these traces the safe configuration saves nothing**. Savings and headroom
   are the same dial.
6. **No controller handles delayed metrics, a sudden jump, or a pause well.** `metrics-lag` saturates for 4 to 5 minutes for every scaler,
   `sudden-increase` for 30-45 s, and the `pause-then-resume` row shows what an emergency pause costs when demand rises during it
   (about 25 minutes saturated for everyone): the pause is a stop switch, not a degraded mode.
7. **Restarts and outages.** In `controller-restart` (a 5 minute downtime during a decline) no scaler saturated, so the restart did not change
   the outcome measurably in this trace; the engine's stabilisation history is persisted but is discarded across a gap longer than its MaxGap (3 min
   minimum), which the 5 minute downtime exceeds. The cost-policy rows keep stepping down for about 30 minutes after the last demand change
   (`Acted after last change`), the same unstable-window effect as in point 2. In `kubecost-outage`, the `fallback mode` row (a native HPA takes the
   replicas after 2 minutes) has the same headroom penalty as the Freeze row because that penalty is already present in `ramp-up-and-down`, before and
   outside the outage.

## Recommendation (conservative default)

* Keep the shipped stabilisation windows (scale-down 10 min, scale-up 30 s, cooldown 60 s) and `Freeze`: shorter scale-down windows fail the
  criteria and a 20 minute window buys no safety here at +3% cost.
* For workloads whose latency matters, set `demand.ceilingUtilizationPercent: 80` and `demand.costPressureUtilizationPercent: 75`
  (the `conservative` row). Expect roughly no savings against an HPA with the same windows; that is the honest price of headroom.
* Use the 90%/80% defaults only where running close to the ceiling is acceptable, in `dryRun` first, watching `kubecost_autoscaler_*` and your latency SLO.
* Do not enable the experimental unstable-cost modes. They are not in the CRD. Revisit `scale up to cost target` after a real-cluster measurement.
* None of this replaces measuring on a real cluster with real latency (`docs/BENCHMARK_PROCEDURE.md`).

## Reproduce

```bash
cd operator
go run ./cmd/simbench          # prints the block above
go test ./internal/sim         # determinism, bounds, shared traces, oscillation, HPA arithmetic, doc/simulator agreement
```
