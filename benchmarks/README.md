# Benchmark harness

Same workloads, same load, same measurements for any Kubernetes optimiser. Method, rules and the state of the results:
[docs/BENCHMARKING.md](../docs/BENCHMARKING.md).

```bash
python benchmarks/bench.py traces
```

```bash
python benchmarks/bench.py render diurnal bench-baseline
```

```bash
python -m pytest benchmarks -q
```

## Results

`results/` holds result files written by `bench.py collect`. **It is empty of competitor results: none has been run.**

A result file has `kind: MEASURED` only when `collect` produced it from a real cluster. The simulated comparison of the replica
autoscaler with a native HPA is in [docs/BENCHMARKS.md](../docs/BENCHMARKS.md) and is not stored here, so a simulation can
never sit next to a measurement by accident.

Result schema (`kubecost-auto-hpa/benchmark-result/v1`):

| Field | Meaning |
|---|---|
| `kind` | `MEASURED`, or `SIMULATED` set by hand for anything not collected from a cluster |
| `tool`, `tool_version` | What optimised the workload; `none` for the baseline |
| `scenario`, `run` | One of `scenarios.json`, and the run number |
| `environment` | Cluster, Kubernetes version, provider, node type, node list price |
| `window` | Start, end, hours |
| `metrics` | The numbers. `null` means not measured |
| `missing_series` | Prometheus series that returned nothing |
| `notes` | Mode the tool ran in, learning period, anything not controlled |
