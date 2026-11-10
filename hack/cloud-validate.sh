#!/usr/bin/env bash
# READ-ONLY validation of the rightsizing pipeline on a cluster YOU choose (EKS, GKE, AKS or any other). Run it yourself with your own credentials.
# It never applies anything: it runs plan, backtest, nodes and cost, checks the prerequisites, and writes an evidence file.
#
#   CTX=my-eks-staging CONFIRM_CTX=my-eks-staging PROM=http://localhost:9090 \
#   NS=shop WORKLOAD=api CONTAINER=app [KIND=Deployment] [FOCUS=export.csv] [NODE_HOURLY_USD=0.2] bash hack/cloud-validate.sh
#
# CONFIRM_CTX must equal CTX: a typed confirmation so the wrong cluster is not inspected by accident.
# Prometheus must already be reachable at $PROM (for example kubectl port-forward). NOT RUN against any cloud by the author.
set -u
: "${CTX:?set CTX to the kubeconfig context}"; : "${NS:?set NS}"; : "${WORKLOAD:?set WORKLOAD}"; : "${CONTAINER:?set CONTAINER}"
PROM="${PROM:-http://localhost:9090}"; KIND="${KIND:-Deployment}"
[ "${CONFIRM_CTX:-}" = "$CTX" ] || { echo "set CONFIRM_CTX=$CTX to confirm this is the cluster you mean (read-only run)"; exit 2; }
HERE="$(cd "$(dirname "$0")/.." && pwd)"; OUT="$HERE/.cloud-evidence"; mkdir -p "$OUT"
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"; REPORT="$OUT/$CTX-$STAMP.md"
K="kubectl --context $CTX"
(cd "$HERE/operator" && go build -o "$OUT/rightsize" ./cmd/rightsize) || exit 2
RS="$OUT/rightsize"
PASS=0; FAILN=0
ok()  { echo "PASS  $*" | tee -a "$REPORT"; PASS=$((PASS+1)); }
bad() { echo "FAIL  $*" | tee -a "$REPORT"; FAILN=$((FAILN+1)); }
note(){ echo "$*" | tee -a "$REPORT"; }
{ echo "# Read-only validation of $CTX"; echo; echo "Run at $STAMP UTC by $(whoami). Nothing was changed in the cluster."; echo; } > "$REPORT"

note "## Cluster"
VER="$($K version -o json 2>/dev/null | python -c "import json,sys; print(json.load(sys.stdin)['serverVersion']['gitVersion'])" 2>/dev/null)"
PID="$($K get nodes -o jsonpath='{.items[0].spec.providerID}' 2>/dev/null)"
CLOUD="unknown"; case "$PID" in aws:*) CLOUD=aws;; gce:*) CLOUD=gcp;; azure:*) CLOUD=azure;; esac
note "- server version: ${VER:-unreachable}"; note "- nodes: $($K get nodes --no-headers 2>/dev/null | wc -l); provider id example: ${PID:-none}; cloud detected: $CLOUD"
[ -n "$VER" ] && ok "the cluster answers" || { bad "the cluster does not answer"; exit 1; }

note ""; note "## Prerequisites"
curl -fs "$PROM/-/ready" >/dev/null && ok "Prometheus is reachable at $PROM" || { bad "Prometheus is not reachable at $PROM"; exit 1; }
pq() { curl -s --data-urlencode "query=$1" "$PROM/api/v1/query" | python -c "import json,sys
try:
    r = json.load(sys.stdin)['data']['result']; print(len(r))
except Exception:
    print(0)"; }
SEL="namespace=\"$NS\",container=\"$CONTAINER\""
[ "$(pq "container_cpu_usage_seconds_total{$SEL}")" -gt 0 ] && ok "cAdvisor CPU series exist for the container" || bad "no container_cpu_usage_seconds_total series for $NS/$CONTAINER"
[ "$(pq "container_memory_working_set_bytes{$SEL}")" -gt 0 ] && ok "cAdvisor memory series exist" || bad "no container_memory_working_set_bytes series"
[ "$(pq "kube_pod_container_status_restarts_total{$SEL}")" -gt 0 ] && ok "kube-state-metrics series exist" || bad "no kube-state-metrics series (restarts): the planner will refuse"
HIST="$(curl -s --data-urlencode "query=count_over_time(container_memory_working_set_bytes{$SEL}[30d])" "$PROM/api/v1/query" | python -c "import json,sys
try:
    r = json.load(sys.stdin)['data']['result']; print(int(max(float(x['value'][1]) for x in r)) if r else 0)
except Exception:
    print(0)")"
note "- samples of history (30d, scrape-interval dependent): $HIST"

note ""; note "## plan (production defaults: 7 days, 24 h minimum, 200 samples)"
"$RS" plan --context "$CTX" --prometheus-url "$PROM" --namespace "$NS" --kind "$KIND" --workload "$WORKLOAD" --container "$CONTAINER" > "$OUT/rec-$CTX.json" 2> "$OUT/plan-$CTX.err" \
  && ok "plan produced a record" || { bad "plan failed: $(head -c 300 "$OUT/plan-$CTX.err")"; }
if [ -s "$OUT/rec-$CTX.json" ]; then
  python - "$OUT/rec-$CTX.json" <<'PYEOF' | tee -a "$REPORT"
import json, sys
r = json.load(open(sys.argv[1]))
print("- abstained:", r["Abstained"], "| confidence:", r["Confidence"], "| samples:", r["Samples"])
for k in ("CPU", "Memory"):
    x = r[k]
    print("- %s: %s current=%.6g recommended=%.6g reasons=%s" % (k, x["Action"], x["Current"], x["Recommended"], x["Reasons"]))
for k in ("Risks", "Conditions"):
    for item in (r.get(k) or []):
        print("- %s: %s" % (k.lower(), item))
PYEOF
fi

note ""; note "## backtest (14 days, last 24 h held out)"
"$RS" backtest --prometheus-url "$PROM" --namespace "$NS" --kind "$KIND" --workload "$WORKLOAD" --container "$CONTAINER" > "$OUT/backtest-$CTX.json" 2>"$OUT/backtest-$CTX.err" \
  && ok "backtest ran (read it: $OUT/backtest-$CTX.json)" || note "- backtest skipped: $(head -c 200 "$OUT/backtest-$CTX.err")"

note ""; note "## nodes (candidate analysis, nothing is drained)"
"$RS" nodes --context "$CTX" ${NODE_HOURLY_USD:+--node-hourly-usd "$NODE_HOURLY_USD"} > "$OUT/nodes-$CTX.json" 2>"$OUT/nodes-$CTX.err" \
  && { ok "nodes report produced"; python -c "import json,sys; r=json.load(open(sys.argv[1])); print('- removable candidates:', r['Removable']); print('- stranded cpu/mem:', round(r['StrandedCPU'],2), round(r['StrandedMem']/2**30,2),'GiB')" "$OUT/nodes-$CTX.json" | tee -a "$REPORT"; } \
  || bad "nodes failed: $(head -c 200 "$OUT/nodes-$CTX.err")"

note ""; note "## cost"
if [ -n "${FOCUS:-}" ]; then
  "$RS" cost --context "$CTX" --billing-file "$FOCUS" --billing-format focus --billing-label "$CTX-focus" --start "${COST_START:-$(date -u -d '-7 days' +%Y-%m-%dT%H:%M:%SZ)}" --end "${COST_END:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}" > "$OUT/cost-$CTX.json" 2>"$OUT/cost-$CTX.err" \
    && ok "BILLED-basis cost report produced from the FOCUS export" || bad "cost (FOCUS) failed: $(head -c 300 "$OUT/cost-$CTX.err")"
elif [ -n "${NODE_HOURLY_USD:-}" ]; then
  "$RS" cost --context "$CTX" --node-hourly-usd "$NODE_HOURLY_USD" > "$OUT/cost-$CTX.json" 2>"$OUT/cost-$CTX.err" \
    && ok "ESTIMATED-basis cost report produced (a model, not a bill)" || bad "cost failed: $(head -c 300 "$OUT/cost-$CTX.err")"
else
  note "- skipped: give FOCUS=<export.csv> for a billed basis, or NODE_HOURLY_USD for an estimate"
fi
[ -s "$OUT/cost-$CTX.json" ] && python -c "import json,sys; r=json.load(open(sys.argv[1])); print('- basis:', r['basis'], '| source:', r['source']); print('- total:', r['total'], '| reconciled:', r['reconciled']); print('- unallocated:', r['byNamespace'].get('__unallocated__','0')); print('- notes:', r['notes'])" "$OUT/cost-$CTX.json" | tee -a "$REPORT"

note ""; note "RESULT: $PASS passed, $FAILN failed. Evidence file: $REPORT"
echo "Review $REPORT before sharing it: it contains workload and node names."
[ "$FAILN" -eq 0 ]
