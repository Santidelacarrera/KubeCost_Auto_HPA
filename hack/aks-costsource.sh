#!/usr/bin/env bash
# Two cost sources side by side on ONE real AKS cluster (created here, deleted at the end), sampled for HOURS hours:
#   - Kubecost 2.8.7 with customPricesEnabled (the air-gapped pricing mode): no Azure pricing/disk API calls. On AKS without it,
#     Kubecost logged InvalidSubscriptionId every 5 min and stopped serving allocation data ~10 min after its first data
#     (docs/LAB.md result 16); this run tests whether that cloud integration error is the cause.
#   - OpenCost (the open-source core Kubecost is built on), reading a separate Prometheus that scrapes it.
# For each: the age of the newest data the operator would read, every 5 min, and the client's live contract test at the end.
#   bash hack/aks-costsource.sh            (needs `az login`; HOURS=2 by default)
set -u
AZ="${AZ:-az}"; RG="${RG:-kubecost-cs-rg}"; NAME="${NAME:-kubecost-cs}"; LOC="${LOC:-eastus}"; SIZE="${SIZE:-Standard_D4as_v4}"
HOURS="${HOURS:-2}"; KC_VERSION="${KUBECOST_CHART_VERSION:-2.8.7}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"; OUT="$HERE/.lab/aks-costsource"; mkdir -p "$OUT"
export KUBECONFIG="$(cygpath -w "$OUT/kubeconfig" 2>/dev/null || echo "$OUT/kubeconfig")"
"$AZ" account show >/dev/null 2>&1 || { echo "run az login first"; exit 2; }
"$AZ" group create -n "$RG" -l "$LOC" -o none || exit 1
cleanup() { pkill -f "port-forward" 2>/dev/null; [ "${KEEP:-}" = 1 ] || "$AZ" group delete -n "$RG" --yes -o none; }; trap cleanup EXIT
"$AZ" aks create -g "$RG" -n "$NAME" --tier free --node-count 1 --node-vm-size "$SIZE" --node-osdisk-size 30 \
  --generate-ssh-keys -o none || { echo "aks create failed"; exit 1; }
"$AZ" aks get-credentials -g "$RG" -n "$NAME" --file "$KUBECONFIG" --overwrite-existing -o none
K="kubectl --context $NAME"; H="helm --kube-context $NAME"
echo "== AKS $($K version -o json | python -c "import json,sys; print(json.load(sys.stdin)['serverVersion']['gitVersion'])"), 1 x $SIZE"

echo "== workload shop/payments-api"
$K create ns shop >/dev/null 2>&1
$K -n shop create deployment payments-api --image=nginx:1.27-alpine --replicas=2 --dry-run=client -o yaml | $K apply -f - >/dev/null
$K -n shop set resources deployment payments-api --requests=cpu=100m,memory=64Mi >/dev/null
$K -n shop rollout status deployment payments-api --timeout=180s >/dev/null && echo "PASS  the workload runs"

helm repo add kubecost https://kubecost.github.io/cost-analyzer/ >/dev/null 2>&1 || true
helm repo add opencost https://opencost.github.io/opencost-helm-chart >/dev/null 2>&1 || true
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts >/dev/null 2>&1 || true
helm repo update >/dev/null 2>&1

echo "== Kubecost $KC_VERSION, custom pricing (no cloud pricing API)"
$H upgrade --install kubecost kubecost/cost-analyzer --version "$KC_VERSION" -n kubecost --create-namespace \
  --set global.clusterId="$NAME" --set prometheus.server.global.external_labels.cluster_id="$NAME" --set forecasting.enabled=false \
  --set global.grafana.enabled=false --set global.grafana.proxy=false --set networkCosts.enabled=false \
  --set kubecostProductConfigs.customPricesEnabled=true --set kubecostProductConfigs.defaultModelPricing.enabled=true \
  --wait --timeout 15m > "$OUT/kubecost-install.log" 2>&1 && echo "PASS  Kubecost installed" || { echo "FAIL  Kubecost install"; tail -5 "$OUT/kubecost-install.log"; }

echo "== Prometheus for OpenCost, then OpenCost"
$H upgrade --install prom prometheus-community/prometheus -n monitoring --create-namespace \
  --set alertmanager.enabled=false --set prometheus-pushgateway.enabled=false --set prometheus-node-exporter.enabled=false \
  --set server.persistentVolume.enabled=false --set server.global.scrape_interval=30s -f "$HERE/hack/lab/prom-opencost.yaml" \
  --wait --timeout 10m > "$OUT/prom-install.log" 2>&1 || echo "FAIL  Prometheus install"
$H upgrade --install opencost opencost/opencost -n opencost --create-namespace \
  --set opencost.prometheus.internal.serviceName=prom-prometheus-server --set opencost.prometheus.internal.namespaceName=monitoring \
  --set opencost.prometheus.internal.port=80 --set opencost.exporter.defaultClusterId="$NAME" --set opencost.ui.enabled=true \
  --wait --timeout 10m > "$OUT/opencost-install.log" 2>&1 && echo "PASS  OpenCost installed" || { echo "FAIL  OpenCost install"; tail -5 "$OUT/opencost-install.log"; }
echo "OpenCost image: $($K -n opencost get deploy opencost -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null)"

# kubecost: the cost-analyzer UI service proxies /model/...; opencost: its UI service proxies /model/... too (hack/lab-opencost.sh).
declare -A URL=([kubecost]=http://localhost:19090 [opencost]=http://localhost:19091)
declare -A SVC=([kubecost]="kubecost svc/kubecost-cost-analyzer 19090:9090" [opencost]="opencost svc/opencost 19091:9090")
pf() { curl -fs -m 3 -o /dev/null "${URL[$1]}/model/allocation?window=10m" || { set -- ${SVC[$1]}; ($K -n "$1" port-forward "$2" "$3" >/dev/null 2>&1 &); sleep 5; }; }
for s in kubecost opencost; do echo -e "now_utc\tdata_end_utc\tage_min\tcost_usd_1h" > "$OUT/cadence-$s.tsv"; done

echo "== sampling both every 5 min for $HOURS h (end of the newest data served for shop/payments-api)"
for _ in $(seq 1 $((HOURS * 12))); do
  for s in kubecost opencost; do
    pf "$s"
    CODE=$(curl -s -m 30 -o "$OUT/last-$s.json" -w '%{http_code}' "${URL[$s]}/model/allocation?window=1h&aggregate=cluster,namespace,controllerKind,controller&accumulate=true" 2>/dev/null)
    python -c "
import json, sys, datetime
now = datetime.datetime.now(datetime.timezone.utc)
try:
    d = json.load(open(sys.argv[1]))['data']
    hit = [v for st in d if st for k, v in st.items() if 'payments-api' in k]
except Exception:
    hit = []
if not hit:
    print('%s\tNONE\t\t' % now.strftime('%Y-%m-%dT%H:%M:%SZ'))
else:
    v = hit[0]; end = datetime.datetime.fromisoformat(v['end'].replace('Z', '+00:00'))
    print('%s\t%s\t%.1f\t%.6f' % (now.strftime('%Y-%m-%dT%H:%M:%SZ'), end.strftime('%Y-%m-%dT%H:%M:%SZ'), (now - end).total_seconds() / 60, v.get('totalCost', 0)))
" "$OUT/last-$s.json" >> "$OUT/cadence-$s.tsv"
    if tail -n 1 "$OUT/cadence-$s.tsv" | grep -q NONE; then
      { echo "== $(date -u +%FT%TZ) $s http=$CODE"; head -c 300 "$OUT/last-$s.json"; echo; } >> "$OUT/errors-$s.txt"
    fi
    echo "$s $(tail -n 1 "$OUT/cadence-$s.tsv")"
  done
  sleep 300
done

for s in kubecost opencost; do
  echo "== $s cadence summary"
  python - "$OUT/cadence-$s.tsv" <<'PYEOF' | tee "$OUT/summary-$s.txt"
import csv, sys, datetime
rows = list(csv.DictReader(open(sys.argv[1]), delimiter='\t'))
ok = [r for r in rows if r['data_end_utc'] not in ('NONE', '')]
ages = sorted(float(r['age_min']) for r in ok)
ends = sorted({r['data_end_utc'] for r in ok})
steps = [(datetime.datetime.fromisoformat(b[:-1]) - datetime.datetime.fromisoformat(a[:-1])).total_seconds() / 60 for a, b in zip(ends, ends[1:])]
print('samples %d, with data %d, missing %d' % (len(rows), len(ok), len(rows) - len(ok)))
if ages:
    q = lambda p: ages[min(len(ages) - 1, int(p * len(ages)))]
    print('age of the newest data (min): min %.1f, median %.1f, p95 %.1f, max %.1f' % (ages[0], q(0.5), q(0.95), ages[-1]))
if steps:
    print('distinct data ends %d; minutes between successive ends: min %.1f, max %.1f' % (len(ends), min(steps), max(steps)))
PYEOF
done

echo "== Kubecost diagnostics"
{ $K -n kubecost get pods -o wide
  $K -n kubecost logs deploy/kubecost-cost-analyzer -c cost-model --tail=400 2>&1 | sed 's/\x1b\[[0-9;]*m//g' | grep -E " (ERR|WRN) " | cut -c32-200 | sort | uniq -c | sort -rn | head -15
} > "$OUT/kubecost-diag.txt" 2>&1; cat "$OUT/kubecost-diag.txt"

echo "== the client's live contract test against each, after $HOURS h"
RC=0
for s in kubecost opencost; do
  pf "$s"
  (cd "$HERE/operator" && KUBECOST_LIVE_URL="${URL[$s]}" KUBECOST_LIVE_NAMESPACE=shop KUBECOST_LIVE_CONTROLLER=payments-api KUBECOST_LIVE_WINDOW=1h \
    go test -tags kubecost_live -count=1 -v -run Live ./internal/kubecost/) > "$OUT/contract-$s.log" 2>&1
  if grep -q '^--- PASS' "$OUT/contract-$s.log"; then echo "PASS  contract and freshness against $s"; else echo "FAIL  contract or freshness against $s"; RC=1; fi
  grep -E "live_test" "$OUT/contract-$s.log" | cut -c1-250
done
exit $RC
