Scenario: `step-increase`

| Metric | none  (run 1, MEASURED) | kubecost-auto-hpa 73fd385 (run 1, MEASURED) |
|---|---|---|
| CPU requested and unused (%) | 87.1 | 78.9 |
| Memory requested and unused (%) | 98.1 | 96.9 |
| CPU wasted (core-hours) | 1.06 | 0.58 |
| Memory wasted (GiB-hours) | 1.00 | 0.60 |
| Node-hours | 2.03 | 2.03 |
| Node cost at list price (USD) | not measured | not measured |
| Reaction: first replica change after the event (s) | not measured | not measured |
| Reaction: first request change after the event (s) | not measured | 43 |
| Interventions on replicas | 0 | 0 |
| Interventions on requests | 0 | 2 |
| Availability of desired replicas (%) | 99.898 | 99.590 |
| Container restarts | 0 | 0 |
| OOM kills | not measured | not measured |
| Worst p95 latency (s) | 0.0048 | 0.0048 |
| Worst p99 latency (s) | 0.0050 | 0.0050 |
| Worst 5xx ratio | not measured | not measured |
| Tool's own CPU (cores, average) | not measured | not measured |
| Tool's own memory (MiB, peak) | not measured | not measured |
