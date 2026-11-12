#!/usr/bin/env bash
# The recommender against SIMULATED SHOPPERS (hack/lab/synthetic-users.yaml): a compressed week (one simulated day = PERIOD_MIN
# minutes, default 60, so 7 h) of sessions with a daily and weekly cycle and random flash sales, on the minikube lab.
# Then, with only that history:
#   1. backtest: train on the first part, test on the unseen last HOLDOUT (does the recommendation hold through days it never saw?);
#   2. plan from the whole history, apply with the SLO on the store's real latency histogram (p95 > 250 ms or 5xx > 1% breaches),
#      timed so the observation covers the next simulated peak, and compare latency/errors in the hour before and after.
# What it is not: real users or real weeks. It answers "does the recommender survive bursty, cyclic, multi-route load", not
# "are the defaults right for production".
#   bash hack/lab-up.sh && bash hack/lab-synthetic-users.sh          (CTX=kubecost-lab, Prometheus port-forwarded on :9090)
set -u
CTX="${CTX:-kubecost-lab}"; PROM="${PROM:-http://localhost:9090}"; PERIOD_MIN="${PERIOD_MIN:-60}"; DAYS="${DAYS:-7}"
HOLDOUT="${HOLDOUT:-$((PERIOD_MIN * 2))m}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"; LAB="$HERE/.lab/synthetic-users"; mkdir -p "$LAB"
K="kubectl --context $CTX"
[ "$CTX" = "kubecost-lab" ] || [ "${ALLOW_CTX:-}" = "1" ] || { echo "refusing context '$CTX'"; exit 2; }
PASS=0; FAILN=0
ok()  { echo "PASS  $*"; PASS=$((PASS+1)); }
bad() { echo "FAIL  $*"; FAILN=$((FAILN+1)); }
check() { if eval "$2"; then ok "$1"; else bad "$1"; fi; }
pf() { curl -fs -m 3 "$PROM/-/ready" >/dev/null || { ($K -n monitoring port-forward svc/prom-prometheus-server "${PROM##*:}":80 >/dev/null 2>&1 &); sleep 5; }; }
q() { pf; curl -s "$PROM/api/v1/query" --data-urlencode "query=$1" | python -c "import json,sys; r=json.load(sys.stdin)['data']['result']; print(r[0]['value'][1] if r else 'nan')"; }
(cd "$HERE/operator" && go build -o "$LAB/rightsize.exe" ./cmd/rightsize) || exit 2
RS="$LAB/rightsize.exe"
SEL='namespace="lab",pod=~"shop-.*"'
P95="histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{$SEL}[5m])))"

if [ -z "${SKIP_RUN:-}" ]; then
echo "== 0. the store and the simulated shoppers; one simulated day = $PERIOD_MIN min, $DAYS days"
$K create ns lab >/dev/null 2>&1
sed "s/value: \"60\"}/value: \"$PERIOD_MIN\"}/" "$HERE/hack/lab/synthetic-users.yaml" | $K apply -f - >/dev/null
$K -n lab rollout status deploy/shop --timeout=5m >/dev/null && $K -n lab rollout status deploy/users --timeout=5m >/dev/null
check "store and shoppers run" "[ \"\$($K -n lab get deploy shop -o jsonpath='{.status.availableReplicas}')\" = 2 ]"
date -u +%FT%TZ > "$LAB/started"
echo "running $DAYS simulated days ($((PERIOD_MIN * DAYS)) min); progress in $LAB/progress.tsv"
echo -e "utc\trps\tp95_s\terr_ratio\tcpu_cores\tmem_mib\tshoppers" > "$LAB/progress.tsv"
END=$(( $(date +%s) + PERIOD_MIN * DAYS * 60 ))
while [ "$(date +%s)" -lt "$END" ]; do
  echo -e "$(date -u +%TZ)\t$(q "sum(rate(http_requests_total{$SEL}[1m]))")\t$(q "$P95")\t$(q "sum(rate(http_requests_total{$SEL,code!=\"200\"}[5m])) / sum(rate(http_requests_total{$SEL}[5m]))")\t$(q "sum(rate(container_cpu_usage_seconds_total{namespace=\"lab\",pod=~\"shop-.*\",container=\"app\"}[1m]))")\t$(q "sum(container_working_set_bytes{namespace=\"lab\",pod=~\"shop-.*\",container=\"app\"}) / 1048576")\t$($K -n lab logs deploy/users --tail=1 2>/dev/null | cut -c1-80)" >> "$LAB/progress.tsv"
  sleep 300
done
$K -n lab logs deploy/users --tail=2000 > "$LAB/users.log" 2>&1
fi
pf

echo "== 1. backtest: train on the history before the last $HOLDOUT, test on that unseen part"
"$RS" backtest --prometheus-url "$PROM" --namespace lab --workload shop --container app \
  --window "$((PERIOD_MIN * DAYS))m" --holdout "$HOLDOUT" --step 30s --min-samples 200 > "$LAB/backtest.json" 2> "$LAB/backtest.err"
check "backtest ran on the simulated week" "[ -s $LAB/backtest.json ]"
head -c 4000 "$LAB/backtest.json"; echo; head -c 400 "$LAB/backtest.err"

echo "== 2. plan from the whole history (production minimums: 200 samples), apply with the SLO, observe through a peak"
"$RS" plan --context "$CTX" --prometheus-url "$PROM" --namespace lab --workload shop --container app \
  --window "$((PERIOD_MIN * DAYS))m" --step 30s --min-samples 200 > "$LAB/rec.json" 2> "$LAB/plan.err"
python -c "import json,sys; r=json.load(open(sys.argv[1])); print('abstained', r['Abstained'], 'confidence', r['Confidence'], 'cpu', r['CPU'].get('Current'), '->', r['CPU'].get('Recommended'), 'mem', r['Memory'].get('Current'), '->', r['Memory'].get('Recommended'), 'reasons', r.get('Reasons') or r.get('AbstainReasons'))" "$LAB/rec.json" 2>&1 | tee "$LAB/rec-summary.txt"
check "the recommender produced a recommendation (not abstained)" "python -c \"import json,sys; sys.exit(1 if json.load(open('$LAB/rec.json'))['Abstained'] else 0)\""
BEFORE_P95=$(q "histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{$SEL}[${PERIOD_MIN}m])))")
BEFORE_ERR=$(q "sum(increase(http_requests_total{$SEL,code!=\"200\"}[${PERIOD_MIN}m])) / sum(increase(http_requests_total{$SEL}[${PERIOD_MIN}m]))")
BEFORE_REQ=$($K -n lab get deploy shop -o jsonpath='{.spec.template.spec.containers[0].resources.requests.cpu}/{.spec.template.spec.containers[0].resources.requests.memory}')
POL="$LAB/policy.yaml"; printf 'allowNamespaces: [lab]\nmaxChangeFraction: 0.6\nminConfidence: Low\n' > "$POL"   # Low: a 7 h window caps confidence below Medium by design (under 7 days)
SLO="(histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{$SEL}[2m]))) > bool 0.25) or (sum(rate(http_requests_total{$SEL,code!=\"200\"}[2m])) / sum(rate(http_requests_total{$SEL}[2m])) > bool 0.01)"
"$RS" approve --recommendation "$LAB/rec.json" --context "$CTX" --audit-file "$LAB/audit.jsonl" > "$LAB/approve.out" 2>&1
"$RS" apply --recommendation "$LAB/rec.json" --policy "$POL" --context "$CTX" --execute --observe-for "${PERIOD_MIN}m" --poll 30s --rollout-timeout 5m \
  --slo-prometheus-url "$PROM" --slo-breach-query "$SLO" --audit-file "$LAB/audit.jsonl" > "$LAB/apply.out" 2> "$LAB/apply.err"
AP=$?
AFTER_REQ=$($K -n lab get deploy shop -o jsonpath='{.spec.template.spec.containers[0].resources.requests.cpu}/{.spec.template.spec.containers[0].resources.requests.memory}')
AFTER_P95=$(q "histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{$SEL}[${PERIOD_MIN}m])))")
AFTER_ERR=$(q "sum(increase(http_requests_total{$SEL,code!=\"200\"}[${PERIOD_MIN}m])) / sum(increase(http_requests_total{$SEL}[${PERIOD_MIN}m]))")
THR=$(q "sum(increase(container_cpu_cfs_throttled_periods_total{namespace=\"lab\",pod=~\"shop-.*\",container=\"app\"}[${PERIOD_MIN}m])) / sum(increase(container_cpu_cfs_periods_total{namespace=\"lab\",pod=~\"shop-.*\",container=\"app\"}[${PERIOD_MIN}m]))")
RST=$($K -n lab get pods -l app=shop -o jsonpath='{range .items[*]}{.status.containerStatuses[0].restartCount}{" "}{end}')
echo "requests: $BEFORE_REQ -> $AFTER_REQ (apply exit $AP; tail of its output below)"; tail -n 5 "$LAB/apply.out"; head -c 300 "$LAB/apply.err"
echo "p95 latency over one simulated day: before $BEFORE_P95 s, after $AFTER_P95 s; error ratio before $BEFORE_ERR, after $AFTER_ERR"
echo "after the change: CPU throttled periods $THR, restarts [$RST]"
grep -q '"flash": *true\|flash=True' "$LAB/users.log" 2>/dev/null; $K -n lab logs deploy/users --since="${PERIOD_MIN}m" 2>/dev/null | grep -c "flash=True" | sed 's/^/minutes of flash sale inside the observation: /'
check "the change was applied and kept (no SLO breach during a full simulated day)" "[ $AP -eq 0 ] && [ \"$AFTER_REQ\" != \"$BEFORE_REQ\" ]"
check "p95 latency after stays under the 250 ms SLO" "python -c \"import sys; sys.exit(0 if float('$AFTER_P95') < 0.25 else 1)\""
echo; echo "RESULT: $PASS passed, $FAILN failed"
[ "$FAILN" -eq 0 ]
