#!/usr/bin/env bash
# One real run of the benchmark harness (benchmarks/, docs/BENCHMARKING.md) on the local lab cluster: the same scenario in two
# namespaces at the same time, one left alone (tool "none") and one rightsized by this product through the CLI.
#   bash hack/lab-up.sh && bash hack/bench-local.sh
# SCENARIO (default step-increase, 120 minutes), APPLY_AT (minutes after the start at which the tool acts, default 25: before
# the load step at minute 40, so the run shows whether an earlier reduction hurts when load triples).
# What is real: the cluster, the pods, the load, Prometheus. What is not: one laptop node, a trivial application, a learning
# period of minutes with relaxed minimums (the product's defaults ask for 24 hours), one run. It is a run of the harness, not a
# result about any workload that matters. It creates two namespaces and deletes them at the end (KEEP=1 keeps them).
set -u
CTX="${CTX:-kubecost-lab}"; SCENARIO="${SCENARIO:-step-increase}"; APPLY_AT="${APPLY_AT:-25}"; PP="${PROM_PORT:-9092}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"; LAB="$HERE/.lab/bench"; OUT="$HERE/benchmarks/results"; mkdir -p "$LAB" "$OUT"
K="kubectl --context $CTX"; RS="$LAB/rightsize.exe"
case "$CTX" in kubecost-lab*) ;; *) echo "refusing: this script is for the disposable lab cluster"; exit 2;; esac
(cd "$HERE/operator" && go build -o "$RS" ./cmd/rightsize) || exit 2
MINUTES="$(python -c "import json,sys; print(json.load(open(sys.argv[1]))[sys.argv[2]]['minutes'])" "$HERE/benchmarks/scenarios.json" "$SCENARIO")"
VERSION="$(git -C "$HERE" rev-parse --short HEAD)"
pf() { curl -fs -m 3 "http://localhost:$PP/-/ready" >/dev/null || { ($K -n monitoring port-forward svc/prom-prometheus-server "$PP:80" >/dev/null 2>&1 &); sleep 6; }; }

for ns in bench-none bench-tool; do
  python "$HERE/benchmarks/bench.py" render "$SCENARIO" "$ns" | $K apply -f - >/dev/null || exit 1
done
$K -n bench-none rollout status deploy/app --timeout=180s >/dev/null && $K -n bench-tool rollout status deploy/app --timeout=180s >/dev/null || { echo "workloads not ready"; exit 1; }
START="$(date -u +%s)"; echo "$START" > "$LAB/start"
echo "started $SCENARIO at $(date -u -d "@$START" +%FT%TZ): $MINUTES minutes; the tool acts at minute $APPLY_AT"

sleep $((APPLY_AT * 60)); pf
cat > "$LAB/policy.yaml" <<EOF
allowNamespaces: [bench-tool]
maxChangeFraction: 0.6
minConfidence: Low
EOF
W=$((APPLY_AT - 3))
"$RS" plan --context "$CTX" --prometheus-url "http://localhost:$PP" --namespace bench-tool --workload app --container app \
  --window "${W}m" --step 15s --min-window "$((W - 4))m" --min-samples 40 > "$LAB/rec.json" 2> "$LAB/plan.err" || { cat "$LAB/plan.err"; }
python -c "import json,sys; r=json.load(open(sys.argv[1])); print('plan: abstained', r['Abstained'], 'confidence', r['Confidence']); [print(' ', k, r[k]['Action'], r[k]['Current'], '->', r[k]['Recommended']) for k in ('CPU','Memory')]" "$LAB/rec.json" | tee "$LAB/plan.txt"
"$RS" approve --recommendation "$LAB/rec.json" --context "$CTX" --audit-file "$LAB/audit.jsonl" >/dev/null 2>"$LAB/approve.err" || cat "$LAB/approve.err"
EVENT="$(date -u +%s)"
"$RS" apply --recommendation "$LAB/rec.json" --policy "$LAB/policy.yaml" --context "$CTX" --execute --observe-for 10m --poll 20s \
  --rollout-timeout 5m --audit-file "$LAB/audit.jsonl" > "$LAB/apply.out" 2> "$LAB/apply.err"
echo "apply exit $?: $(tail -c 300 "$LAB/apply.out" | tr '\n' ' ')" | tee "$LAB/apply.txt"

NOW="$(date -u +%s)"; LEFT=$((START + MINUTES * 60 + 120 - NOW)); [ "$LEFT" -gt 0 ] && sleep "$LEFT"
END="$(date -u +%s)"; pf
NODE="$($K get nodes -o jsonpath='{.items[0].status.nodeInfo.kubeletVersion}')"
for pair in "none:bench-none:" "kubecost-auto-hpa:bench-tool:$VERSION"; do
  tool="${pair%%:*}"; rest="${pair#*:}"; ns="${rest%%:*}"; ver="${rest#*:}"
  python "$HERE/benchmarks/bench.py" collect --prom "http://localhost:$PP" --namespace "$ns" --start "$START" --end "$END" --event-time "$EVENT" \
    --scenario "$SCENARIO" --tool "$tool" --tool-version "$ver" --run 1 --cluster "$CTX (minikube, one node, laptop)" --kubernetes "$NODE" \
    --provider local --node-type "docker driver, 4 CPU, 5 GB" \
    --notes "Both namespaces ran at the same time on the same node. Tool column: rightsize plan/approve/apply through the CLI at minute $APPLY_AT with relaxed minimums (window ${W}m, 40 samples), policy minConfidence Low; product defaults ask for 24 h. $(cat "$LAB/plan.txt" | tr '\n' ' ') $(cat "$LAB/apply.txt")" \
    > "$OUT/local-$SCENARIO-$tool-1.json" || echo "collect failed for $tool"
done
python "$HERE/benchmarks/bench.py" compare "$OUT/local-$SCENARIO-none-1.json" "$OUT/local-$SCENARIO-kubecost-auto-hpa-1.json" | tee "$OUT/local-$SCENARIO-1.md"
[ "${KEEP:-}" = 1 ] || $K delete ns bench-none bench-tool --wait=false >/dev/null
echo "done"
