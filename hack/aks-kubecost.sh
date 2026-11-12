#!/usr/bin/env bash
# A REAL Kubecost on a REAL managed cluster (AKS, created here and deleted at the end): the client's live contract test
# (hack/lab-kubecost.sh), then Kubecost's refresh cadence measured for HOURS hours, to choose failurePolicy.maxMetricAge
# from a measurement instead of a guess (docs/ROADMAP.md P0 item 3).
#   bash hack/aks-kubecost.sh            (needs `az login`; HOURS=2 by default)
# One Standard_D4as_v4 node (the free account's regional quota is 4 vCPU). Its own kubeconfig: ~/.kube/config is not touched.
set -u
AZ="${AZ:-az}"; RG="${RG:-kubecost-kc-rg}"; NAME="${NAME:-kubecost-kc}"; LOC="${LOC:-eastus}"; SIZE="${SIZE:-Standard_D4as_v4}"
HOURS="${HOURS:-2}"; VERSION="${KUBECOST_CHART_VERSION:-2.8.7}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"; OUT="$HERE/.lab/aks-kubecost"; mkdir -p "$OUT"
export KUBECONFIG="$(cygpath -w "$OUT/kubeconfig" 2>/dev/null || echo "$OUT/kubeconfig")"   # a Windows path: kubectl.exe gets it even under MSYS_NO_PATHCONV=1
"$AZ" account show >/dev/null 2>&1 || { echo "run az login first"; exit 2; }
"$AZ" group create -n "$RG" -l "$LOC" -o none || exit 1
cleanup() { [ "${KEEP:-}" = 1 ] || "$AZ" group delete -n "$RG" --yes -o none; }; trap cleanup EXIT
"$AZ" aks create -g "$RG" -n "$NAME" --tier free --node-count 1 --node-vm-size "$SIZE" --node-osdisk-size 30 \
  --generate-ssh-keys -o none || { echo "aks create failed"; exit 1; }
"$AZ" aks get-credentials -g "$RG" -n "$NAME" --file "$KUBECONFIG" --overwrite-existing -o none
K="kubectl --context $NAME"
echo "== AKS $($K version -o json | python -c "import json,sys; print(json.load(sys.stdin)['serverVersion']['gitVersion'])"), 1 x $SIZE, Kubecost chart $VERSION"

PROFILE="$NAME" ALLOW_CTX=1 SKIP_MINIKUBE=1 KUBECOST_CHART_VERSION="$VERSION" bash "$HERE/hack/lab-kubecost.sh" 2>&1 | tee "$OUT/contract.log"
CT=${PIPESTATUS[0]}

echo "== refresh cadence: every 5 min for $HOURS h, the end of the newest data Kubecost serves for the workload"
URL=http://localhost:19090
pf() { curl -fs -m 3 -o /dev/null "$URL/healthz" || { ($K -n kubecost port-forward svc/kubecost-cost-analyzer 19090:9090 >/dev/null 2>&1 &); sleep 5; }; }
echo -e "now_utc\tdata_end_utc\tage_min\tcost_usd_1h" > "$OUT/cadence.tsv"
for _ in $(seq 1 $((HOURS * 12))); do
  pf
  CODE=$(curl -s -m 30 -o "$OUT/last.json" -w '%{http_code}' "$URL/model/allocation?window=1h&aggregate=cluster,namespace,controllerKind,controller&accumulate=true" 2>/dev/null)
  python -c "
import json, sys, datetime
now = datetime.datetime.now(datetime.timezone.utc)
try:
    d = json.load(sys.stdin)['data']
    hit = [v for s in d if s for k, v in s.items() if 'payments-api' in k]
except Exception:
    hit = []
if not hit:
    print('%s\tNONE\t\t' % now.strftime('%Y-%m-%dT%H:%M:%SZ'))
else:
    v = hit[0]; end = datetime.datetime.fromisoformat(v['end'].replace('Z', '+00:00'))
    print('%s\t%s\t%.1f\t%.6f' % (now.strftime('%Y-%m-%dT%H:%M:%SZ'), end.strftime('%Y-%m-%dT%H:%M:%SZ'), (now - end).total_seconds() / 60, v.get('totalCost', 0)))
" < "$OUT/last.json" >> "$OUT/cadence.tsv"
  # a sample without data: keep why (HTTP code, body head, Kubecost pods) instead of guessing later
  if tail -n 1 "$OUT/cadence.tsv" | grep -q NONE; then
    { echo "== $(date -u +%FT%TZ) http=$CODE"; head -c 300 "$OUT/last.json"; echo; $K -n kubecost get pods --no-headers 2>&1; } >> "$OUT/cadence-errors.txt"
  fi
  tail -n 1 "$OUT/cadence.tsv"; sleep 300
done
python - "$OUT/cadence.tsv" <<'PYEOF' | tee "$OUT/cadence-summary.txt"
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
print('maxMetricAge must sit above the max age seen, or the operator freezes (fail closed) on this Kubecost.')
PYEOF
{ echo "== Kubecost pods at the end"; $K -n kubecost get pods -o wide 2>&1
  echo "== restarts and last termination"; $K -n kubecost get pods -o jsonpath='{range .items[*]}{.metadata.name}{" "}{range .status.containerStatuses[*]}{.name}{"="}{.restartCount}{" "}{.lastState.terminated.reason}{" "}{end}{"\n"}{end}' 2>&1
  echo "== node allocation"; $K describe nodes | grep -A8 "Allocated resources"
  echo "== cost-model log tail"; $K -n kubecost logs deploy/kubecost-cost-analyzer -c cost-model --tail=60 2>&1 | cut -c1-300
} > "$OUT/kubecost-diag.txt" 2>&1
# The contract test again, now that Kubecost has hours of history: the first run (minutes after the install) is refused by the
# client's coverage check (fail closed), which is correct but is not a contract result.
pf
(cd "$HERE/operator" && KUBECOST_LIVE_URL="$URL" KUBECOST_LIVE_NAMESPACE=shop KUBECOST_LIVE_CONTROLLER=payments-api KUBECOST_LIVE_WINDOW=1h   go test -tags kubecost_live -count=1 -v -run Live ./internal/kubecost/) 2>&1 | tee "$OUT/contract-late.log"; CT2=${PIPESTATUS[0]}
pkill -f "port-forward svc/kubecost-cost-analyzer" 2>/dev/null
echo; echo "contract exit (minutes after install) $CT, contract exit (after $HOURS h) $CT2"
[ "$CT2" -eq 0 ]
