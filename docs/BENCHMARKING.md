# Competitive benchmarking

How to compare KubeCost Auto-HPA with CAST AI, Kubecost and ScaleOps (or anything else) on the same workloads, the same load
and the same measurements.

## State of the results

| Comparison | State |
|---|---|
| KubeCost Auto-HPA against CAST AI | **Not run.** Needs a CAST AI account connected to a cluster you own |
| KubeCost Auto-HPA against ScaleOps | **Not run.** Needs a ScaleOps licence or trial |
| KubeCost Auto-HPA against Kubecost | **Not run** as a benchmark. Kubecost was only run as a cost source ([LAB.md](LAB.md) results 11, 16, 18); Kubecost recommends, it does not apply |
| `KubeCostAutoscaler` against a native HPA | **Simulated** only ([BENCHMARKS.md](BENCHMARKS.md)): on those traces the safe configuration saves nothing against an HPA with the same windows |
| KubeCost Auto-HPA alone, before and after | **Measured**, small scale: [LAB.md](LAB.md) results 9, 12, 14, 17, 19, 27 |
| This harness, KubeCost Auto-HPA against an untouched copy of the same workload | **Measured once** on the local lab cluster ([LAB.md](LAB.md) result 28, `benchmarks/results/`): one run, one laptop node, trivial application |

The harness below was run for real once, on the local lab cluster (`hack/bench-local.sh`). It has not been run on a managed
cluster, and never with another product.

There is no measured result against any competitor, so **nothing in this repository supports a claim of being better, equal or
worse than any of them.** [COMPETITIVE.md](COMPETITIVE.md) compares documented features from public pages, with sources; it is
not a test.

The three products are proprietary. This harness uses no code of theirs and drives them only through what their vendors
document for customers. Check each vendor's terms before publishing a comparison: some licences restrict publishing benchmarks.

## The harness

`benchmarks/` contains everything that defines a run. Standard-library Python, no dependencies.

| File | What it is |
|---|---|
| `scenarios.json` | Five workloads with their load shape and their deliberately fixed requests |
| `traces/*.csv` | The load of each scenario, requests per second for every minute. Generated from a fixed seed and committed, so every run of every tool gets the same load |
| `bench.py render <scenario> <namespace>` | The Kubernetes manifests: application, Service, PodDisruptionBudget, the trace, an open-loop load generator |
| `bench.py collect ...` | Reads Prometheus for a window and writes one result file (JSON) |
| `bench.py compare a.json b.json` | One table, one column per result. Refuses to compare different scenarios; flags different environments and non-measured columns |
| `test_bench.py` | Pins the traces, the manifests and the arithmetic (run in CI) |

### Scenarios

| Scenario | Load | What it tests |
|---|---|---|
| `overprovisioned-steady` | Flat, 2 h | Finding obvious waste without disturbing the service |
| `diurnal` | Day/night cycle every 60 min, 3 h | Sizing across a cycle |
| `step-increase` | Triples at minute 40 | Reaction time; whether an earlier reduction hurts |
| `spikes` | Four 3-minute bursts at 5x | Stability, oscillation |
| `steady-growth` | Doubles over 2 h | Sizing for where load is going |

The application is a trivial HTTP server. That is a limit of the benchmark: it says how a tool behaves on these shapes, not on
your services. Add your own scenario to `scenarios.json` for a workload that matters to you; the tests will tell you if it is
malformed.

### Metrics

| Metric | How it is measured |
|---|---|
| Cost reduction | Node-hours over the window x a list price you supply. List price, never an invoice. To reconcile with a bill, use `rightsize reconcile` on two billing periods ([FINOPS.md](FINOPS.md)) |
| Wasted CPU, wasted memory | Integral of requests minus integral of usage, in core-hours and GiB-hours, and as a share of requests |
| Reaction time | Seconds from the load change (`--event-time`) to the first change of replicas, and of requests |
| Scaling time | From the same event to the last replica change before the series is flat again: read it from the result's series; not reduced to one number |
| Stability | Number of changes to replicas and to requests over the run (interventions). Fewer for the same outcome is better |
| Errors | Container restarts, OOM kills, worst 5xx ratio |
| Availability | Share of desired replicas that were available, over the window |
| Latency | Worst p95 and p99 from the application's histogram |
| Prediction accuracy | For this product: `rightsize backtest` (share of unseen samples above the recommendation). For the others: only if the vendor exposes its recommendation; otherwise "not measured" |
| The tool's own resource use | CPU and memory of the pods in the tool's namespace (`--tool-namespace`) |

A metric whose series is missing is reported as **not measured**. It is never zero.

## Procedure

One cluster per provider, one node pool, cluster autoscaler on. Each tool gets the same cluster shape, created fresh.

1. **Baseline (tool `none`).** Render and apply the scenario, wait for the trace to finish, collect.

   ```bash
   python benchmarks/bench.py render diurnal bench-baseline | kubectl apply -f -
   ```

   ```bash
   python benchmarks/bench.py collect --prom http://localhost:9090 --namespace bench-baseline \
     --start <unix> --end <unix> --scenario diurnal --tool none --run 1 \
     --cluster <name> --kubernetes 1.36 --provider gke --node-type e2-standard-4 --node-hourly-usd <list price> > baseline-1.json
   ```

2. **Each tool, on a fresh cluster of the same shape.** Install it as its vendor documents, in the mode you are comparing
   (recommend-only against recommend-only, automatic against automatic). Give it the learning period its documentation asks
   for **before** the measured window, and record how long that was. Apply the same scenario in its own namespace. Collect.
3. **Three runs per tool per scenario**, at least. Report each run, not only an average.
4. **Compare.**

   ```bash
   python benchmarks/bench.py compare baseline-1.json kubecost-auto-hpa-1.json castai-1.json
   ```

For KubeCost Auto-HPA: install the chart ([INSTALL.md](INSTALL.md)); for the automatic mode use the applier with a policy that
allows the benchmark namespace and `requireApproval: false`. State that in the result's notes: it is the experimental mode.

### Rules for a result that may be published

* The run's environment is in the file: cluster, Kubernetes version, provider, node type, list price, window, run number. A
  result without them is not a result.
* `kind` is `MEASURED` only for what `collect` wrote from a real cluster. Anything produced another way is labelled `SIMULATED`
  by hand, and `compare` flags the column.
* Same scenario, same cluster shape, same learning period policy, same mode for every tool. `compare` flags different
  environments; it cannot see a different learning period, so write it in `--notes`.
* Judge reliability first. A tool with worse availability, more errors or worse latency than the baseline beyond a margin you
  fixed in advance fails that scenario, whatever it saved.
* Publish the result files, not only the table.
* Do not extrapolate to other workloads, and do not turn "less waste in this scenario" into "x% savings".

## What this cannot tell you

* Anything about a product's features outside rightsizing and replica scaling (commitment management, Spot automation, node
  provisioning). CAST AI and ScaleOps do things in those areas that this product does not do at all ([STATUS.md](STATUS.md)).
* Behaviour on real services, real traffic and weeks of history.
* What you would pay: list price and one node pool are not your bill.
