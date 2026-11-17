#!/usr/bin/env bash
# End-to-end validation on a REAL GKE cluster (managed Kubernetes, real cluster autoscaler), created by the owner's account for this run.
#   CTX=<gke context> NODE_HOURLY_USD=<list price of one node> bash hack/gke-validate.sh
# What it does, in order:
#   0. Prometheus + kube-state-metrics, and the HTTP app with constant load, deliberately over-provisioned (REPLICAS x 500m CPU)
#      so that its REQUESTS force the cluster autoscaler to add nodes.
#   1. BEFORE window: node count sampled every 30 s for WINDOW minutes (nothing changes during it).
#   2. hack/lab-traffic.sh against GKE: recommendation from real metrics, human approval, controlled apply with an SLO on the REAL
#      latency histogram, equivalent before/after latency windows, and a forced rollback.
#   3. The autoscaler is given time to remove nodes that are no longer needed (GKE decides, not this tool).
#   4. AFTER window: the same sampling, same length. Saved node-hours x the node list price = a MEASURED infrastructure change priced
#      at LIST price (not an invoice, no discounts, no free tier).
#   5. hack/cloud-validate.sh (read-only) and rightsize backtest on the history collected during this run (hours, not weeks).
# It deletes nothing; the caller deletes the cluster.
set -u
: "${CTX:?set CTX}"; : "${NODE_HOURLY_USD:?set NODE_HOURLY_USD (list price of one node, USD/h)}"
WINDOW="${WINDOW:-20}"; REPLICAS="${REPLICAS:-6}"; SETTLE="${SETTLE:-25}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"; LAB="$HERE/.lab/${LABDIR:-gke}"; mkdir -p "$LAB"
PP="${PROM_PORT:-9090}"   # local port for the Prometheus port-forward (another run may hold 9090)
K="kubectl --context $CTX"; H="helm --kube-context $CTX"
# Managed clusters only (GKE gce://, EKS aws://, AKS azure://): the name is historical, the steps are provider-neutral.
case "$($K get nodes -o jsonpath='{.items[0].spec.providerID}' 2>/dev/null)" in gce://*|aws://*|azure://*) ;; *) echo "refusing: $CTX is not a managed GKE/EKS/AKS cluster"; exit 2;; esac
PASS=0; FAILN=0
ok()  { echo "PASS  $*"; PASS=$((PASS+1)); }
bad() { echo "FAIL  $*"; FAILN=$((FAILN+1)); }
check() { if eval "$2"; then ok "$1"; else bad "$1"; fi; }
nodes() { $K get nodes --no-headers 2>/dev/null | grep -c ' Ready'; }
sample() { # $1 = label, writes "epoch nodes" lines for WINDOW minutes
  : > "$LAB/nodes-$1.txt"
  for _ in $(seq 1 $((WINDOW * 2))); do echo "$(date +%s) $(nodes)" >> "$LAB/nodes-$1.txt"; sleep 30; done; }
nodehours() { awk '{s+=$2} END {printf "%.4f", s*30/3600}' "$LAB/nodes-$1.txt"; }
pf() { curl -fs -m 3 http://localhost:$PP/-/ready >/dev/null || { ($K -n monitoring port-forward svc/prom-prometheus-server $PP:80 >/dev/null 2>&1 &); sleep 5; }; }

echo "== cluster: $($K version -o json 2>/dev/null | python -c "import json,sys; print(json.load(sys.stdin)['serverVersion']['gitVersion'])"), nodes $(nodes), node price $NODE_HOURLY_USD USD/h (list)"
echo "== 0. Prometheus and the over-provisioned app"
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts >/dev/null 2>&1 || true
helm repo update prometheus-community >/dev/null 2>&1
$K create ns monitoring --dry-run=client -o yaml | $K apply -f - >/dev/null
$H upgrade --install prom prometheus-community/prometheus -n monitoring \
  --set alertmanager.enabled=false --set prometheus-pushgateway.enabled=false --set prometheus-node-exporter.enabled=false \
  --set server.persistentVolume.enabled=false --set server.global.scrape_interval=15s --set server.resources.requests.memory=256Mi \
  --wait --timeout 10m >/dev/null 2>&1
check "Prometheus installed" "$K -n monitoring rollout status deploy/prom-prometheus-server --timeout=60s >/dev/null 2>&1"
$K create ns lab >/dev/null 2>&1; $K apply -f "$HERE/hack/lab/traffic.yaml" >/dev/null; $K -n lab scale deploy/shop --replicas="$REPLICAS" >/dev/null
for _ in $(seq 1 40); do [ "$($K -n lab get deploy shop -o jsonpath='{.status.availableReplicas}')" = "$REPLICAS" ] && break; sleep 15; done
check "all $REPLICAS replicas run (the autoscaler added nodes for their requests if needed)" "[ \"\$($K -n lab get deploy shop -o jsonpath='{.status.availableReplicas}')\" = $REPLICAS ]"
echo "nodes now: $(nodes); waiting 5 min for the node count to settle"
sleep 300
echo "== 1. BEFORE window ($WINDOW min), requests $($K -n lab get deploy shop -o jsonpath='{.spec.template.spec.containers[0].resources.requests.cpu}') x $REPLICAS"
sample before; echo "node-hours before: $(nodehours before) (nodes seen: $(awk '{print $2}' "$LAB/nodes-before.txt" | sort -u | tr '\n' ' '))"

echo "== 2. hack/lab-traffic.sh on GKE (recommend, approve, apply with real-latency SLO, before/after, forced rollback)"
pf
CTX="$CTX" ALLOW_CTX=1 PROM=http://localhost:$PP REPLICAS="$REPLICAS" bash "$HERE/hack/lab-traffic.sh" > "$LAB/traffic.log" 2>&1
grep -E "^(==|PASS|FAIL|SKIP|RESULT|requests:)|metric|requests/s|p95|error ratio|throttled|usage p95|restarts|USD" "$LAB/traffic.log"
PASS=$((PASS+$(grep -c '^PASS' "$LAB/traffic.log"))); FAILN=$((FAILN+$(grep -c '^FAIL' "$LAB/traffic.log")))
echo "requests after the run: $($K -n lab get deploy shop -o jsonpath='{.spec.template.spec.containers[0].resources.requests.cpu}') x $REPLICAS"

echo "== 3. letting the GKE cluster autoscaler react (up to $SETTLE min; scale-down needs ~10 min of a node being unneeded)"
N0="$(awk 'END{print $2}' "$LAB/nodes-before.txt")"
for _ in $(seq 1 $((SETTLE * 2))); do [ "$(nodes)" -lt "$N0" ] && break; sleep 30; done
sleep 120; echo "nodes now: $(nodes) (before: $N0)"
# Why a node stayed: requests per node, what runs where, and the autoscaler's own explanation (read-only).
if [ "$(nodes)" -ge "$N0" ]; then
  { echo "== allocated requests per node"; $K describe nodes | grep -E "^Name:|^  cpu |^  memory "
    echo "== pods per node"; $K get pods -A -o wide --no-headers | awk '{print $8, $1"/"$2}' | sort
    echo "== cluster-autoscaler-status (if the provider publishes it)"; $K -n kube-system get configmap cluster-autoscaler-status -o jsonpath='{.data.status}' 2>&1
    echo; echo "== autoscaler events"; $K get events -A --field-selector source=cluster-autoscaler 2>&1 | tail -n 30
  } > "$LAB/autoscaler-diag.txt" 2>&1
  echo "no node removed; diagnostics in $LAB/autoscaler-diag.txt"; grep -A3 "^Name:" "$LAB/autoscaler-diag.txt" | head -n 12
fi

echo "== 4. AFTER window ($WINDOW min, same length)"
sample after; echo "node-hours after: $(nodehours after) (nodes seen: $(awk '{print $2}' "$LAB/nodes-after.txt" | sort -u | tr '\n' ' '))"
python - "$(nodehours before)" "$(nodehours after)" "$NODE_HOURLY_USD" "$WINDOW" <<'PYEOF' | tee "$LAB/savings.txt"
import sys
b, a, p, w = float(sys.argv[1]), float(sys.argv[2]), float(sys.argv[3]), float(sys.argv[4])
print("node-hours in %d min: before %.4f, after %.4f" % (w, b, a))
print("cost of those windows at LIST price: before %.4f USD, after %.4f USD" % (b * p, a * p))
print("measured change: %.4f USD per %d min = %.4f USD/hour (%.1f%%)" % ((b - a) * p, w, (b - a) * p * 60 / w, 100 * (b - a) / b if b else 0))
print("MEASURED from the cluster's node count, priced at list price: NOT an invoice (no discounts, credits, free tier, disks or network).")
PYEOF
check "the autoscaler removed at least one node after the change (infrastructure saving measured)" "python -c \"import sys; sys.exit(0 if float('$(nodehours after)') < float('$(nodehours before)') else 1)\""

echo "== 5. read-only cloud-validate and backtest on this run's history"
pf
CTX="$CTX" CONFIRM_CTX="$CTX" PROM=http://localhost:$PP NS=lab WORKLOAD=shop CONTAINER=app NODE_HOURLY_USD="$NODE_HOURLY_USD" bash "$HERE/hack/cloud-validate.sh" > "$LAB/cloud-validate.log" 2>&1
grep -E "^(PASS|FAIL|RESULT|- )" "$LAB/cloud-validate.log"
"$HERE/.lab/rightsize.exe" backtest --prometheus-url http://localhost:$PP --namespace lab --workload shop --container app \
  --window "${BT_WINDOW:-100m}" --holdout "${BT_HOLDOUT:-30m}" --step 15s --min-samples 40 > "$LAB/backtest.json" 2> "$LAB/backtest.err"
check "backtest ran on the real history of this run" "[ -s $LAB/backtest.json ]"
head -c 3000 "$LAB/backtest.json"; echo; head -c 300 "$LAB/backtest.err"
taskkill //F //IM kubectl.exe >/dev/null 2>&1 || true
echo; echo "RESULT: $PASS passed, $FAILN failed"
[ "$FAILN" -eq 0 ]
