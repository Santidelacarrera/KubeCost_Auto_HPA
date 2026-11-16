# Results

One real run is recorded: `local-step-increase-*` (2026-11-16, local lab cluster, this product against an untouched copy of the
same workload; `docs/LAB.md` result 28). It is one run on one laptop node with a trivial application.

* Against CAST AI, Kubecost and ScaleOps: not run. Each needs an account or licence with the vendor and a cluster you own.
* KubeCost Auto-HPA alone, before and after a change: measured at small scale, recorded in `docs/LAB.md` with its evidence
  files (they predate this harness, so they are not in this format).

Add result files as `<provider>-<scenario>-<tool>-<run>.json`, exactly as `bench.py collect` wrote them.
