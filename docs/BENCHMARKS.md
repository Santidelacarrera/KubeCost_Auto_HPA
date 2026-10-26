# Benchmarks: native HPA vs KubeCostAutoscaler (synthetic)

**What this is.** A deterministic discrete-time simulation (`operator/internal/sim`, printed by
`go run ./cmd/simbench`) that drives the *real* decision engine (`internal/policy`) and a reimplementation of the documented
native HPA algorithm on identical demand. The same binary always prints the same numbers.

**What this is not.** A measurement of a real cluster. There is no latency model, no scheduler, no node provisioning, no
real KubeCost and no network. **No latency percentiles (p50/p95/p99), error rates or throughput were measured**, because a
simulator cannot produce meaningful ones; "unmet demand" and "saturated seconds" are the proxies. Do not read any figure
here as a production saving. Numbers are only meaningful relative to each other, under the assumptions below.

## Method

| Assumption | Value |
|---|---|
| Capacity | each *ready* pod serves 100 rps at 100% of its request; utilisation = demand / (ready pods x 100) |
| Pod startup | 30 s from scale-up to ready; scale-down removes not-ready pods first, then the newest |
| Price | $0.05 per replica-hour, fixed (so the "cost" is replica-hours x price) |
| Bounds | min 2, max 60 replicas; warm start at the replicas needed for the first demand sample |
| Control period | every scaler is evaluated every 15 s; simulation step 5 s; 2 h per scenario |
| Cost signal given to KCA | the window-average replica count over the last 15 min x price; marked *stable* only if no scale action happened inside the window (mirrors the real "replicas changed inside the cost window" rule) |
| HPA baseline | `ceil(ready x util/target)`, target 70%, tolerance 10%, scale-down uses the highest recommendation of the window, scale-up capped per period at `max(+4 pods, +100%)` (Kubernetes defaults). Two variants: default 5 min window and a 10 min window matched to KCA |
| KCA demand-only | target 70%, ceiling 90%, tolerance 10%, scale-down window 10 min, scale-up window 30 s, 60 s cooldown, no cost policy |
| KCA with cost policy | as above plus target $1.00/h, budget $2.00/h (20 and 40 replicas at this price), cost-pressure utilisation 80%, guardrail on |

Scenarios: `steady` (1500 rps), `ramp-up-and-down` (500 to 3000 and back over 2 h), `burst` (800 rps with two 5-minute 4x
spikes), `gradual-decline` (3000 to 500 over an hour), `intermittent` (400/2000 rps square wave, 10 min each).

Columns: *Unmet demand %* is demand above ready capacity as a share of all demand; *Saturated s* is time with demand above
ready capacity; *>90% util s* is time above the 90% ceiling; *Reversals* counts direction changes between consecutive scale
actions (oscillation).

## Results

Workload: 100 rps per ready pod, 30s startup, $0.05 per replica-hour, replicas 2-60, warm start

### steady (2h0m0s)

| Scaler | Cost USD | Avg replicas | Max replicas | Scale actions | Reversals | Unmet demand % | Saturated s | >90% util s | Longest saturation s |
|---|---|---|---|---|---|---|---|---|---|
| HPA default (down 5m) | 2.20 | 22.0 | 22 | 0 | 0 | 0.00 | 0 | 0 | 0 |
| HPA (down 10m) | 2.20 | 22.0 | 22 | 0 | 0 | 0.00 | 0 | 0 | 0 |
| KCA demand-only | 2.20 | 22.0 | 22 | 0 | 0 | 0.00 | 0 | 0 | 0 |
| KCA with cost policy | 1.93 | 19.3 | 22 | 1 | 0 | 0.00 | 0 | 0 | 0 |

### ramp-up-and-down (2h0m0s)

| Scaler | Cost USD | Avg replicas | Max replicas | Scale actions | Reversals | Unmet demand % | Saturated s | >90% util s | Longest saturation s |
|---|---|---|---|---|---|---|---|---|---|
| HPA default (down 5m) | 3.07 | 30.7 | 39 | 39 | 1 | 0.00 | 0 | 0 | 0 |
| HPA (down 10m) | 3.18 | 31.8 | 39 | 34 | 1 | 0.00 | 0 | 0 | 0 |
| KCA demand-only | 3.29 | 32.9 | 42 | 35 | 1 | 0.00 | 0 | 0 | 0 |
| KCA with cost policy | 3.07 | 30.7 | 38 | 29 | 1 | 0.00 | 0 | 880 | 0 |

### burst (2h0m0s)

| Scaler | Cost USD | Avg replicas | Max replicas | Scale actions | Reversals | Unmet demand % | Saturated s | >90% util s | Longest saturation s |
|---|---|---|---|---|---|---|---|---|---|
| HPA default (down 5m) | 1.75 | 17.5 | 46 | 8 | 3 | 2.00 | 90 | 120 | 45 |
| HPA (down 10m) | 2.04 | 20.4 | 46 | 8 | 3 | 2.00 | 90 | 120 | 45 |
| KCA demand-only | 2.04 | 20.4 | 46 | 6 | 3 | 1.67 | 60 | 120 | 30 |
| KCA with cost policy | 2.21 | 22.1 | 36 | 6 | 3 | 1.83 | 120 | 120 | 60 |

### gradual-decline (2h0m0s)

| Scaler | Cost USD | Avg replicas | Max replicas | Scale actions | Reversals | Unmet demand % | Saturated s | >90% util s | Longest saturation s |
|---|---|---|---|---|---|---|---|---|---|
| HPA default (down 5m) | 2.43 | 24.3 | 43 | 27 | 0 | 0.00 | 0 | 0 | 0 |
| HPA (down 10m) | 2.57 | 25.7 | 43 | 32 | 0 | 0.00 | 0 | 0 | 0 |
| KCA demand-only | 2.57 | 25.7 | 43 | 32 | 0 | 0.00 | 0 | 0 | 0 |
| KCA with cost policy | 2.65 | 26.5 | 43 | 5 | 0 | 0.00 | 0 | 0 | 0 |

### intermittent (2h0m0s)

| Scaler | Cost USD | Avg replicas | Max replicas | Scale actions | Reversals | Unmet demand % | Saturated s | >90% util s | Longest saturation s |
|---|---|---|---|---|---|---|---|---|---|
| HPA default (down 5m) | 2.19 | 21.9 | 29 | 23 | 10 | 3.96 | 360 | 360 | 60 |
| HPA (down 10m) | 2.70 | 27.0 | 29 | 3 | 0 | 0.66 | 60 | 60 | 60 |
| KCA demand-only | 2.70 | 27.0 | 29 | 2 | 0 | 0.56 | 60 | 60 | 60 |
| KCA with cost policy | 2.16 | 21.6 | 23 | 2 | 0 | 0.56 | 60 | 60 | 60 |

## Reading the results honestly

* **KCA demand-only is essentially an HPA with a different stabilisation default.** With matched windows
  (`HPA (down 10m)` vs `KCA demand-only`) cost, replicas and saturation are within about 3% of each other (KCA costs 3% more in `ramp-up-and-down`, where its safety floor adds replicas); KCA is slightly better in
  `burst` and `intermittent` (fewer actions, a little less saturation) because it adds the 30 s scale-up window, the cooldown
  and the safety replicas floor. Against the *default* HPA (5 min window) the difference in `intermittent` (10 reversals and
  3.96% unmet demand vs 0 and 0.56%) comes from the longer window, not from the engine.
* **The cost policy trades headroom for money, as designed.** In `steady` it holds 19 replicas instead of 22 (about 12%
  cheaper) by running at 79% utilisation, below the 90% ceiling, with no saturation. In `intermittent` it is about 20%
  cheaper than the matched HPA with the same saturation.
* **It is not always cheaper.** In `gradual-decline` the cost policy costs *more* than the HPA ($2.65 vs $2.43-$2.57) and
  takes 5 actions instead of 27-32. Cause: after every scale action the cost signal is "unstable" for the 15 minute cost
  window, and in that state the engine only allows safety scale-ups, so scale-downs are delayed. In `ramp-up-and-down` the same
  rule keeps it at the utilisation ceiling for about 880 s (no saturation, but no headroom either).
* **Burst:** every scaler saturates for 60-120 s: demand quadruples instantly and new pods need 30 s. Cost policy does not
  worsen the *peak* but it spends more time saturated (120 s vs 60 s demand-only). Pre-warming or a higher `minReplicas` is the
  remedy, not a different controller.
* **A defect was found by this simulation** and fixed: before the fix, `steady` with the cost policy oscillated (5 reversals in
  2 h at constant demand), see "No cost feedback loop" in `docs/KUBECOST_AUTOSCALER.md` section 6.

## Reproduce

```bash
cd operator
go run ./cmd/simbench          # prints the tables above
go test ./internal/sim         # determinism, bounds, "constant demand never reverses", HPA arithmetic
```
