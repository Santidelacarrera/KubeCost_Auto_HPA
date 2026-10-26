# KubeCost contract fixtures

These files are **representative, hand-written responses**. Their field names and nesting come from the
OpenCost source (`core/pkg/opencost/allocation_json.go`, `allocationprops.go`), which KubeCost's allocation API
extends. They were **not captured from a live KubeCost installation**; no fixture here is evidence that a real
KubeCost returns exactly this. `internal/kubecost/live_test.go` (build tag `kubecost_live`) is the test that
checks a real installation once one is available.

| File | Purpose |
|---|---|
| `opencost_accumulated.json` | `aggregate=controller&accumulate=true` shaped answer: several controllers, extra upstream fields, and the synthetic `__idle__` / `__unallocated__` entries real aggregations contain |
| `drift_namespace_is_number.json` | schema drift: a property changes type; must fail closed, never read as zero cost |
