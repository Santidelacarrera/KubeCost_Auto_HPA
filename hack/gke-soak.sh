#!/usr/bin/env bash
# Seven-day soak on a REAL GKE cluster, aimed at docs/CLOUD_VALIDATION.md section 5 (7 days of history, an approved apply kept
# through full daily/weekly cycles, a cost report reconciled against the provider's billing export).
#   PROJECT=<gcp project> bash hack/gke-soak.sh up        # day 0: cluster, Prometheus (10 d retention, on a disk), diurnal traffic
#   PROJECT=<gcp project> bash hack/gke-soak.sh status    # any time: nodes, samples of history, lab pods
#   PROJECT=<gcp project> bash hack/gke-soak.sh approve   # day >= 1: plan with PRODUCTION defaults, approve, apply with a real-latency SLO
#   PROJECT=<gcp project> BILLING_TABLE=<dataset.table> [FOCUS_TABLE=<dataset.table>] bash hack/gke-soak.sh report   # day >= 7, into .lab/soak/
#   PROJECT=<gcp project> bash hack/gke-soak.sh down      # deletes the cluster (asks nothing: only call it when done)
# What is REAL: GKE, its autoscaler, Prometheus history, HTTP latency, Google's billing export. What is NOT: the traffic is a written
# schedule against a trivial app (hack/lab/traffic-diurnal.yaml), so "business cycle" means its shape, not real users.
set -u
: "${PROJECT:?set PROJECT}"
ZONE="${ZONE:-us-central1-a}"; NAME="${NAME:-kubecost-soak}"; CTX="gke_${PROJECT}_${ZONE}_${NAME}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"; LAB="$HERE/.lab/soak"; mkdir -p "$LAB"
K="kubectl --context $CTX"; H="helm --kube-context $CTX"; RS="$HERE/.lab/rightsize.exe"
NODE_HOURLY_USD="${NODE_HOURLY_USD:-0.06701142}"   # e2-standard-2 list price, Cloud Billing Catalog API, 2026-11-10
SEL='namespace="lab",pod=~"shop-.*"'
PP="${PROM_PORT:-9091}"   # its own local port, so a parallel run on 9090 does not get this cluster's Prometheus
pf() { curl -fs -m 3 http://localhost:$PP/-/ready >/dev/null || { ($K -n monitoring port-forward svc/prom-prometheus-server $PP:80 >/dev/null 2>&1 &); sleep 6; }; }
q() { curl -s --data-urlencode "query=$1" http://localhost:$PP/api/v1/query | python -c "import json,sys
try:
    r = json.load(sys.stdin)['data']['result']; print(r[0]['value'][1] if r else 'nan')
except Exception:
    print('nan')"; }
build() { (cd "$HERE/operator" && go build -o "$RS" ./cmd/rightsize) || exit 2; }

case "${1:-}" in
up)
  gcloud container clusters create "$NAME" --project "$PROJECT" --zone "$ZONE" --machine-type e2-standard-2 \
    --num-nodes 1 --enable-autoscaling --min-nodes 1 --max-nodes 3 --disk-type pd-standard --disk-size 30 \
    --enable-cost-allocation --labels purpose=kubecost-soak --quiet || exit 1
  gcloud container clusters get-credentials "$NAME" --project "$PROJECT" --zone "$ZONE" >/dev/null 2>&1
  date -u +%Y-%m-%dT%H:%M:%SZ > "$LAB/started"
  helm repo add prometheus-community https://prometheus-community.github.io/helm-charts >/dev/null 2>&1 || true
  helm repo update prometheus-community >/dev/null 2>&1
  $K create ns monitoring --dry-run=client -o yaml | $K apply -f - >/dev/null
  $H upgrade --install prom prometheus-community/prometheus -n monitoring \
    --set alertmanager.enabled=false --set prometheus-pushgateway.enabled=false --set prometheus-node-exporter.enabled=false \
    --set server.persistentVolume.enabled=true --set server.persistentVolume.size=10Gi --set server.retention=10d \
    --set server.global.scrape_interval=15s --set server.resources.requests.memory=512Mi --wait --timeout 10m || exit 1
  $K create ns lab >/dev/null 2>&1; $K apply -f "$HERE/hack/lab/traffic-diurnal.yaml"
  $K -n lab rollout status deploy/shop --timeout=15m
  echo "started $(cat "$LAB/started"); nodes: $($K get nodes --no-headers | wc -l)";;
status)
  pf; echo "started: $(cat "$LAB/started" 2>/dev/null)  nodes: $($K get nodes --no-headers 2>/dev/null | grep -c ' Ready')"
  echo "samples of shop memory history: $(q "max(count_over_time(container_memory_working_set_bytes{$SEL,container=\"app\"}[10d]))")"
  echo "requests/s now: $(q "sum(rate(http_request_duration_seconds_count{$SEL}[5m]))")"
  echo "requests now: $($K -n lab get deploy shop -o jsonpath='{.spec.template.spec.containers[0].resources.requests}')"
  $K -n lab get pods --no-headers;;
approve)
  build; pf
  "$RS" plan --context "$CTX" --prometheus-url http://localhost:$PP --namespace lab --workload shop --container app \
    --cpu-price-per-core-hour 0.02181159 --mem-price-per-gib-hour 0.00292353 > "$LAB/rec-shop.json" 2> "$LAB/plan.err" || { cat "$LAB/plan.err"; exit 1; }
  python -c "import json,sys; r=json.load(open(sys.argv[1])); print('abstained', r['Abstained'], 'confidence', r['Confidence'], 'samples', r['Samples']); [print(k, r[k]['Action'], r[k]['Current'], '->', r[k]['Recommended']) for k in ('CPU','Memory')]; [print('risk:', x) for x in (r.get('Risks') or [])]" "$LAB/rec-shop.json"
  "$RS" approve --recommendation "$LAB/rec-shop.json" --context "$CTX" --audit-file "$LAB/audit.jsonl" || exit 1
  SLO='histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{'$SEL'}[1m]))) > bool 0.25'
  "$RS" apply --recommendation "$LAB/rec-shop.json" --policy "$HERE/hack/lab/policy.yaml" --context "$CTX" --execute \
    --observe-for 30m --poll 30s --rollout-timeout 10m --slo-prometheus-url http://localhost:$PP --slo-breach-query "$SLO" \
    --audit-file "$LAB/audit.jsonl" | tee "$LAB/apply.out"
  date -u +%Y-%m-%dT%H:%M:%SZ > "$LAB/applied";;
report)
  : "${BILLING_TABLE:?set BILLING_TABLE=<dataset>.gcp_billing_export_resource_v1_<id> (detailed export)}"
  build; pf
  S="$(cat "$LAB/started")"; A="$(cat "$LAB/applied" 2>/dev/null || echo "$S")"; E="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "# GKE soak report: started $S, applied $A, report $E" | tee "$LAB/report.md"
  echo "## health through the cycles after the apply ($A .. $E)" | tee -a "$LAB/report.md"
  R=$(( ($(date -u +%s) - $(date -u -d "$A" +%s)) / 60 ))m
  for x in "restarts|sum(increase(kube_pod_container_status_restarts_total{$SEL}[$R]))" \
           "OOMKilled now|sum(kube_pod_container_status_last_terminated_reason{$SEL,reason=\"OOMKilled\"})" \
           "fraction of 5m windows with p95 > 250 ms|avg_over_time((histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{$SEL}[5m]))) > bool 0.25)[$R:5m])" \
           "worst 5m p95 latency (s)|max_over_time(histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{$SEL}[5m])))[$R:5m])" \
           "error ratio|sum(increase(http_request_duration_seconds_count{$SEL,code!~\"2..\"}[$R])) / sum(increase(http_request_duration_seconds_count{$SEL}[$R]))" \
           "CPU throttled fraction|sum(increase(container_cpu_cfs_throttled_periods_total{$SEL,container=\"app\"}[$R])) / sum(increase(container_cpu_cfs_periods_total{$SEL,container=\"app\"}[$R]))" \
           "peak requests/s|max_over_time(sum(rate(http_request_duration_seconds_count{$SEL}[5m]))[$R:5m])" \
           "trough requests/s|min_over_time(sum(rate(http_request_duration_seconds_count{$SEL}[5m]))[$R:5m])"; do
    echo "- ${x%%|*}: $(q "${x#*|}")" | tee -a "$LAB/report.md"; done
  echo "## node-hours from Prometheus (kube_node_info), before and after the apply" | tee -a "$LAB/report.md"
  python - "$S" "$A" "$E" "$NODE_HOURLY_USD" "$PP" <<'PYEOF' | tee -a "$LAB/report.md"
import json, sys, urllib.parse, urllib.request
from datetime import datetime
ts = lambda s: datetime.strptime(s, "%Y-%m-%dT%H:%M:%SZ").timestamp()
s, a, e, p = ts(sys.argv[1]), ts(sys.argv[2]), ts(sys.argv[3]), float(sys.argv[4])
def series(lo, hi):
    u = ("http://localhost:%s/api/v1/query_range?" % sys.argv[5]) + urllib.parse.urlencode({"query": "count(kube_node_info)", "start": lo, "end": hi, "step": 300})
    r = json.load(urllib.request.urlopen(u))["data"]["result"]
    return [float(v) for _, v in r[0]["values"]] if r else []
b, f = series(s + 3600, a), series(a + 3600, e)   # skip the first hour of each period (scale-up, rollout)
nb, nf = (sum(b) / len(b) if b else float("nan")), (sum(f) / len(f) if f else float("nan"))
print("- mean nodes before: %.3f over %.1f h; after: %.3f over %.1f h" % (nb, len(b) * 300 / 3600, nf, len(f) * 300 / 3600))
print("- at LIST price %.8f USD/node-h: before %.4f USD/h, after %.4f USD/h, change %.4f USD/h" % (p, nb * p, nf * p, (nb - nf) * p))
print("- the two periods have different lengths and different weekday/weekend mix: compare per hour, not totals")
PYEOF
  echo "## billing export (Google's own numbers) for the cluster's resources, $S .. $E" | tee -a "$LAB/report.md"
  bq --project_id "$PROJECT" query --use_legacy_sql=false --format=csv --max_rows=1000000 "
    SELECT IFNULL(resource.name, service.description) AS ResourceId,
           IF(currency = 'USD', cost, IF(currency_conversion_rate > 0, cost / currency_conversion_rate, cost)) AS BilledCost,
           IF(currency = 'USD' OR currency_conversion_rate > 0, 'USD', currency) AS BillingCurrency, cost AS LocalCost, currency AS LocalCurrency,
           FORMAT_TIMESTAMP('%Y-%m-%dT%H:%M:%SZ', usage_start_time) AS ChargePeriodStart,
           FORMAT_TIMESTAMP('%Y-%m-%dT%H:%M:%SZ', usage_end_time) AS ChargePeriodEnd, 'Usage' AS ChargeCategory
    FROM \`$PROJECT.$BILLING_TABLE\`
    WHERE usage_start_time >= TIMESTAMP('$S') AND usage_end_time <= TIMESTAMP('$E') AND cost > 0
      AND (resource.name LIKE 'gke-$NAME-%' OR resource.name = '$NAME' OR EXISTS (SELECT 1 FROM UNNEST(labels) l WHERE l.key = 'goog-k8s-cluster-name' AND l.value = '$NAME'))
    UNION ALL
    SELECT 'credit:' || c.type, IF(currency = 'USD', c.amount, IF(currency_conversion_rate > 0, c.amount / currency_conversion_rate, c.amount)),
           IF(currency = 'USD' OR currency_conversion_rate > 0, 'USD', currency), c.amount, currency, FORMAT_TIMESTAMP('%Y-%m-%dT%H:%M:%SZ', usage_start_time),
           FORMAT_TIMESTAMP('%Y-%m-%dT%H:%M:%SZ', usage_end_time), 'Credit'
    FROM \`$PROJECT.$BILLING_TABLE\`, UNNEST(credits) c
    WHERE usage_start_time >= TIMESTAMP('$S') AND usage_end_time <= TIMESTAMP('$E')
      AND (resource.name LIKE 'gke-$NAME-%' OR resource.name = '$NAME' OR EXISTS (SELECT 1 FROM UNNEST(labels) l WHERE l.key = 'goog-k8s-cluster-name' AND l.value = '$NAME'))" \
    > "$LAB/focus.csv" 2> "$LAB/bq.err" || { cat "$LAB/bq.err"; exit 1; }
  python - "$LAB/focus.csv" <<'PYEOF' | tee -a "$LAB/report.md"
import csv, sys, collections
rows = list(csv.DictReader(open(sys.argv[1])))
cur = {r["BillingCurrency"] for r in rows}
by = collections.defaultdict(float)
for r in rows: by[r["ChargeCategory"]] += float(r["BilledCost"])
loc = collections.defaultdict(float)
for r in rows: loc[(r["LocalCurrency"], r["ChargeCategory"])] += float(r["LocalCost"])
print("- rows: %d, currency: %s, usage %.4f, credits %.4f, net %.4f (converted per row with Google's currency_conversion_rate)" % (len(rows), ",".join(sorted(cur)), by["Usage"], by["Credit"], by["Usage"] + by["Credit"]))
print("- in the billing currency (what the invoice shows): %s" % dict((k[0] + " " + k[1], round(v, 2)) for k, v in loc.items()))
PYEOF
  USE="$LAB/focus.csv"   # the detailed export reduced to FOCUS column names
  if [ -n "${FOCUS_TABLE:-}" ]; then
    # Google's NATIVE FOCUS export (preview, dataset gcp_billing_immutable_*): the cost report reads it as delivered, and its total is
    # compared with the detailed export above as a cross-check. Rows are selected by the cluster name anywhere in the row.
    echo "## native FOCUS export ($FOCUS_TABLE)" | tee -a "$LAB/report.md"
    bq --project_id "$PROJECT" query --use_legacy_sql=false --format=csv --max_rows=1000000 "
      SELECT ResourceId,
             IF(BillingCurrency = 'USD', BilledCost, IF(x_CurrencyConversionRate > 0, BilledCost / x_CurrencyConversionRate, BilledCost)) AS BilledCost,
             IF(BillingCurrency = 'USD' OR x_CurrencyConversionRate > 0, 'USD', BillingCurrency) AS BillingCurrency, ChargeCategory,
             BilledCost AS LocalCost, BillingCurrency AS LocalCurrency,
             FORMAT_TIMESTAMP('%Y-%m-%dT%H:%M:%SZ', ChargePeriodStart) AS ChargePeriodStart,
             FORMAT_TIMESTAMP('%Y-%m-%dT%H:%M:%SZ', ChargePeriodEnd) AS ChargePeriodEnd
      FROM \`$PROJECT.$FOCUS_TABLE\` f
      WHERE ChargePeriodStart >= TIMESTAMP('$S') AND ChargePeriodEnd <= TIMESTAMP('$E') AND TO_JSON_STRING(f) LIKE '%$NAME%'" \
      > "$LAB/focus-native.csv" 2> "$LAB/bq-native.err" || cat "$LAB/bq-native.err"
    if [ -s "$LAB/focus-native.csv" ]; then
      python - "$LAB/focus-native.csv" <<'PYEOF' | tee -a "$LAB/report.md"
import csv, sys, collections
rows = list(csv.DictReader(open(sys.argv[1])))
by = collections.defaultdict(float)
for r in rows: by[r["ChargeCategory"]] += float(r["BilledCost"] or 0)
loc = collections.defaultdict(float)
for r in rows: loc[(r["LocalCurrency"], r["ChargeCategory"])] += float(r["LocalCost"] or 0)
print("- rows: %d, currency: %s, by ChargeCategory: %s (converted per row with x_CurrencyConversionRate)" % (len(rows), ",".join(sorted({r["BillingCurrency"] for r in rows})), dict((k, round(v, 4)) for k, v in by.items())))
print("- in the billing currency (what the invoice shows): %s" % dict((k[0] + " " + k[1], round(v, 2)) for k, v in loc.items()))
PYEOF
      USE="$LAB/focus-native.csv"
    fi
  fi
  CTX="$CTX" CONFIRM_CTX="$CTX" PROM=http://localhost:$PP NS=lab WORKLOAD=shop CONTAINER=app FOCUS="$USE" \
    COST_START="$S" COST_END="$E" NODE_HOURLY_USD="$NODE_HOURLY_USD" bash "$HERE/hack/cloud-validate.sh" > "$LAB/cloud-validate.log" 2>&1
  grep -E "^(PASS|FAIL|RESULT|- )" "$LAB/cloud-validate.log" | tee -a "$LAB/report.md"
  "$RS" backtest --prometheus-url http://localhost:$PP --namespace lab --workload shop --container app --window 7d --holdout 24h \
    > "$LAB/backtest-7d.json" 2> "$LAB/backtest-7d.err" && echo "- backtest 7d/24h: $LAB/backtest-7d.json" | tee -a "$LAB/report.md"
  echo "The invoice itself is monthly; the billing export is the same source Google invoices from, but credits, taxes and the" \
       "monthly GKE free-tier credit land on the invoice as account-level lines. Compare against the invoice when it is issued." | tee -a "$LAB/report.md";;
down)
  gcloud container clusters delete "$NAME" --project "$PROJECT" --zone "$ZONE" --quiet;;
*) echo "usage: PROJECT=... bash hack/gke-soak.sh up|status|approve|report|down"; exit 2;;
esac
