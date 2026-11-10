#!/usr/bin/env bash
# Real traffic, real latency: compares EQUIVALENT 5-minute windows before and after an applied rightsizing change, with the SLO check
# driven by the application's real latency histogram. Needs the full lab (hack/lab-up.sh) and Prometheus on :9090.
# REAL: Kubernetes, the HTTP app, constant request load, cAdvisor throttling, the latency histogram. NOT real: the workload is trivial
# and the prices used for cost per 1k requests are flags (ESTIMATED, not a bill). One run on a laptop VM proves nothing about production.
set -u
CTX="${CTX:-kubecost-lab}"; PROM="${PROM:-http://localhost:9090}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"; LAB="$HERE/.lab"; mkdir -p "$LAB"
K="kubectl --context $CTX"
CPU_PRICE="${CPU_PRICE:-0.031}"; MEM_PRICE="${MEM_PRICE:-0.004}"   # USD per core-hour / GiB-hour: ESTIMATE inputs
PASS=0; FAILN=0
ok()  { echo "PASS  $*"; PASS=$((PASS+1)); }
bad() { echo "FAIL  $*"; FAILN=$((FAILN+1)); }
check() { if eval "$2"; then ok "$1"; else bad "$1"; fi; }
[ "$CTX" = "kubecost-lab" ] || [ "${ALLOW_CTX:-}" = "1" ] || { echo "refusing context '$CTX'"; exit 2; }
curl -fs "$PROM/-/ready" >/dev/null || { echo "Prometheus not reachable at $PROM"; exit 2; }
(cd "$HERE/operator" && go build -o "$LAB/rightsize.exe" ./cmd/rightsize) || exit 2
RS="$LAB/rightsize.exe"; AUDIT="$LAB/audit-traffic.jsonl"; : > "$AUDIT"
POL="$HERE/hack/lab/policy.yaml"
SEL='namespace="lab",pod=~"shop-.*"'

q() { curl -s --data-urlencode "query=$1" "$PROM/api/v1/query" | python -c "import json,sys
try:
    r = json.load(sys.stdin)['data']['result']
    print(r[0]['value'][1] if r else 'nan')
except Exception:
    print('nan')"; }
isnum() { python -c "import math,sys
try:
    sys.exit(0 if not math.isnan(float(sys.argv[1])) else 1)
except Exception:
    sys.exit(1)" "$1"; }
P95='histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{'$SEL'}[5m])))'
RPS='sum(rate(http_request_duration_seconds_count{'$SEL'}[5m]))'
ERR='sum(rate(http_request_duration_seconds_count{'$SEL',code!~"2.."}[5m])) / sum(rate(http_request_duration_seconds_count{'$SEL'}[5m]))'
THR='sum(increase(container_cpu_cfs_throttled_periods_total{'$SEL',container="app"}[5m])) / sum(increase(container_cpu_cfs_periods_total{'$SEL',container="app"}[5m]))'
CPU='quantile_over_time(0.95, sum(rate(container_cpu_usage_seconds_total{'$SEL',container="app"}[1m]))[5m:15s])'
RST='sum(increase(kube_pod_container_status_restarts_total{'$SEL',container="app"}[5m]))'
req() { $K -n lab get deploy shop -o jsonpath='{.spec.template.spec.containers[0].resources.requests.cpu}/{.spec.template.spec.containers[0].resources.requests.memory}'; }
snapshot() { # prints one JSON line with the window metrics
  python -c "import json,sys; print(json.dumps(dict(p95=float(sys.argv[1]), rps=float(sys.argv[2]), err=float(sys.argv[3]), thr=float(sys.argv[4]), cpu95=float(sys.argv[5]), rst=float(sys.argv[6]))))" \
    "$(q "$P95")" "$(q "$RPS")" "$(q "$ERR")" "$(q "$THR")" "$(q "$CPU")" "$(q "$RST")"; }
jv() { python -c "import json,sys; print(json.loads(sys.argv[1])[sys.argv[2]])" "$1" "$2"; }

echo "== 0. deploy the app and the load, wait for history"
$K create ns lab >/dev/null 2>&1; $K apply -f "$HERE/hack/lab/traffic.yaml" >/dev/null
$K -n lab rollout status deploy/shop deploy/loadgen --timeout=240s >/dev/null 2>&1 || $K -n lab rollout status deploy/shop --timeout=240s >/dev/null
plan() { "$RS" plan --context "$CTX" --prometheus-url "$PROM" --namespace lab --workload shop --container app --window 14m --step 15s --min-window 8m --min-samples 40 \
  --cpu-price-per-core-hour "$CPU_PRICE" --mem-price-per-gib-hour "$MEM_PRICE" > "$LAB/rec-shop.json" 2>"$LAB/plan-shop.err"; }
for _ in $(seq 1 70); do plan && [ "$(python -c "import json,sys; print(json.load(open(sys.argv[1]))['Abstained'])" "$LAB/rec-shop.json")" = False ] && isnum "$(q "$RPS")" && break; sleep 20; done
sleep 60   # a full 5-minute window of steady state before the baseline
RPS0="$(q "$RPS")"; check "the app serves real traffic (requests per second > 5)" "python -c 'import sys; sys.exit(0 if float(sys.argv[1]) > 5 else 1)' $RPS0"

echo "== 1. BEFORE window (last 5 minutes), requests $(req)"
BEFORE="$(snapshot)"; REQ_BEFORE="$(req)"; echo "$BEFORE"
plan
check "the recommendation proposes a CPU decrease"      "[ \"\$(python -c \"import json,sys; print(json.load(open(sys.argv[1]))['CPU']['Action'])\" $LAB/rec-shop.json)\" = Decrease ]"

echo "== 2. approve and apply, SLO driven by the REAL latency histogram (p95 > 250 ms breaches)"
"$RS" approve --recommendation "$LAB/rec-shop.json" --context "$CTX" --audit-file "$AUDIT" >/dev/null 2>&1
SLO_OK='histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{'$SEL'}[1m]))) > bool 0.25'
"$RS" apply --recommendation "$LAB/rec-shop.json" --policy "$POL" --context "$CTX" --execute --observe-for 120s --poll 10s --rollout-timeout 4m \
  --slo-prometheus-url "$PROM" --slo-breach-query "$SLO_OK" --audit-file "$AUDIT" > "$LAB/traffic-apply.out" 2>/dev/null
tr -d '\n ' < "$LAB/traffic-apply.out"; echo
REQ_AFTER="$(req)"; echo "requests: $REQ_BEFORE -> $REQ_AFTER"
check "applied and healthy under real traffic"           "grep -q '\"healthy\": true' $LAB/traffic-apply.out"
check "the requests really changed"                      "[ \"$REQ_AFTER\" != \"$REQ_BEFORE\" ]"

echo "== 3. AFTER window: wait 6 minutes so the 5-minute window is entirely after the change"
sleep 360
AFTER="$(snapshot)"; echo "$AFTER"
python - "$BEFORE" "$AFTER" "$REQ_BEFORE" "$REQ_AFTER" "$CPU_PRICE" "$MEM_PRICE" <<'PYEOF'
import json, sys
b, a = json.loads(sys.argv[1]), json.loads(sys.argv[2])
def money(req, cpu_p, mem_p):
    c, m = req.split('/')
    cores = float(c[:-1]) / 1000 if c.endswith('m') else float(c)
    gib = float(m[:-2]) / 1024 if m.endswith('Mi') else float(m[:-2])
    return 2 * (cores * cpu_p + gib * mem_p)          # 2 replicas, USD per hour of REQUESTED capacity (an estimate)
cp, mp = float(sys.argv[5]), float(sys.argv[6])
hb, ha = money(sys.argv[3], cp, mp), money(sys.argv[4], cp, mp)
print("%-22s %12s %12s" % ("metric", "before", "after"))
for k, label in [("rps", "requests/s"), ("p95", "p95 latency (s)"), ("err", "error ratio"), ("thr", "cpu throttled ratio"), ("cpu95", "cpu usage p95 (cores)"), ("rst", "restarts (5m)")]:
    print("%-22s %12.5f %12.5f" % (label, b[k], a[k]))
print("%-22s %12.5f %12.5f   ESTIMATE (flag prices, requests x replicas)" % ("requested USD/hour", hb, ha))
for name, h, r in (("before", hb, b["rps"]), ("after", ha, a["rps"])):
    print("cost per 1k requests (%s): %.8f USD  ESTIMATE" % (name, h / (r * 3.6) if r > 0 else float('nan')))
PYEOF
python - "$BEFORE" "$AFTER" > "$LAB/traffic-verdict.txt" <<'PYEOF'
import json, math, sys
b, a = json.loads(sys.argv[1]), json.loads(sys.argv[2])
fin = lambda x: not math.isnan(x)
checks = {
 "traffic is comparable (rps within 25 percent)": abs(a["rps"] - b["rps"]) <= 0.25 * b["rps"],
 "p95 latency did not regress (<= max(1.5x, +5 ms))": a["p95"] <= max(1.5 * b["p95"], b["p95"] + 0.005),
 "error ratio did not increase": (not fin(a["err"])) or a["err"] <= (b["err"] if fin(b["err"]) else 0) + 0.001,
 "throttling did not increase by more than 2 points": (not fin(a["thr"])) or a["thr"] <= (b["thr"] if fin(b["thr"]) else 0) + 0.02,
 "no restarts after the change": a["rst"] < 0.5,
}
for k, v in checks.items():
    print(("PASS  " if v else "FAIL  ") + k)
PYEOF
cat "$LAB/traffic-verdict.txt"; PASS=$((PASS+$(grep -c '^PASS' "$LAB/traffic-verdict.txt"))); FAILN=$((FAILN+$(grep -c '^FAIL' "$LAB/traffic-verdict.txt")))

echo "== 4. a rollback driven by the REAL latency metric (threshold made absurdly tight on purpose)"
plan
if [ "$(python -c "import json,sys; print(json.load(open(sys.argv[1]))['CPU']['Action'])" "$LAB/rec-shop.json")" = Decrease ]; then
  STATE="$(req)"
  "$RS" approve --recommendation "$LAB/rec-shop.json" --context "$CTX" --audit-file "$AUDIT" >/dev/null 2>&1
  SLO_TIGHT='histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{'$SEL'}[1m]))) > bool 0.0000001'
  "$RS" apply --recommendation "$LAB/rec-shop.json" --policy "$POL" --context "$CTX" --execute --observe-for 120s --poll 10s --rollout-timeout 4m \
    --slo-prometheus-url "$PROM" --slo-breach-query "$SLO_TIGHT" --audit-file "$AUDIT" > "$LAB/traffic-tight.out" 2>/dev/null
  tr -d '\n ' < "$LAB/traffic-tight.out"; echo
  check "the real-latency SLO query breached and the change was rolled back" "grep -q 'SLO breached' $LAB/traffic-tight.out && grep -q '\"rolledBack\": true' $LAB/traffic-tight.out"
  check "the previous requests are back"                   "[ \"\$(req)\" = \"$STATE\" ]"
else
  echo "SKIP  no further decrease is proposed (already right-sized)"
fi
echo; echo "RESULT: $PASS passed, $FAILN failed"
[ "$FAILN" -eq 0 ]
