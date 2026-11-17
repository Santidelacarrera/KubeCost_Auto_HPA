#!/usr/bin/env bash
# Real-cluster end-to-end check of the recommend -> policy -> approve -> apply -> observe -> rollback path.
#
# Needs: a DISPOSABLE cluster reachable through kubectl context $CTX (default kubecost-lab), Prometheus with cAdvisor and
# kube-state-metrics reachable at $PROM (default http://localhost:9090, e.g. kubectl port-forward), go, python.
# It changes ONLY namespace "lab" of that context and refuses to run against any other context name unless ALLOW_CTX=1.
# Faults are injected by the script itself (a crash file inside a container, a synthetic SLO query) and are labelled as such.
set -u
CTX="${CTX:-kubecost-lab}"
PROM="${PROM:-http://localhost:9090}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"
LAB="$HERE/.lab"
OBSERVE="${OBSERVE:-60s}"
mkdir -p "$LAB"
PASS=0; FAILN=0; SKIPPED=0
ok()   { echo "PASS  $*"; PASS=$((PASS+1)); }
bad()  { echo "FAIL  $*"; FAILN=$((FAILN+1)); }
check(){ if eval "$2"; then ok "$1"; else bad "$1"; fi; }

[ "$CTX" = "kubecost-lab" ] || [ "${ALLOW_CTX:-}" = "1" ] || { echo "refusing to run against context '$CTX' (set ALLOW_CTX=1 if it is disposable)"; exit 2; }
K="kubectl --context $CTX"
$K get ns >/dev/null 2>&1 || { echo "cluster $CTX not reachable"; exit 2; }
curl -fs "$PROM/-/ready" >/dev/null || { echo "Prometheus not reachable at $PROM"; exit 2; }

(cd "$HERE/operator" && go build -o "$LAB/rightsize.exe" ./cmd/rightsize && go build -o "$LAB/recommend.exe" ./cmd/recommend) || exit 2
RS="$LAB/rightsize.exe"; RC="$LAB/recommend.exe"
AUDIT="$LAB/audit.jsonl"; : > "$AUDIT"

$K apply -f "$HERE/hack/lab/api.yaml" >/dev/null
$K -n lab annotate deploy api autoscaling.kubecost.io/approved-recommendation- autoscaling.kubecost.io/last-applied-at- autoscaling.kubecost.io/rolled-back-recommendation- autoscaling.kubecost.io/applied-recommendation- autoscaling.kubecost.io/previous-resources- >/dev/null 2>&1
$K -n lab rollout status deploy/api --timeout=180s >/dev/null || { echo "workload not ready"; exit 2; }

req() { $K -n lab get deploy api -o jsonpath='{.spec.template.spec.containers[0].resources.requests.cpu}/{.spec.template.spec.containers[0].resources.requests.memory}'; }
# jget FILE Dotted.Path -> value (no expression evaluation)
jget() { python -c "import json,sys
d = json.load(open(sys.argv[1]))
for k in sys.argv[2].split('.'):
    d = d[k]
print(d)" "$1" "$2"; }
# newrec: wait until kube-state-metrics reflects the live request (it lags after a change), then write a recommendation
live_milli() { python -c "
import sys
v = sys.argv[1]
print(int(v[:-1]) if v.endswith('m') else int(float(v) * 1000))" "$($K -n lab get deploy api -o jsonpath='{.spec.template.spec.containers[0].resources.requests.cpu}')"; }
newrec() {
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    "$RC" --prometheus-url "$PROM" --cluster "$CTX" --namespace lab --workload api --container app --window 14m --step 15s --min-window 8m --min-samples 40 > "$LAB/rec.json" || return 1
    cur="$(python -c "import sys; print(round(float(sys.argv[1]) * 1000))" "$(jget "$LAB/rec.json" CPU.Current)")"
    [ "$cur" = "$(live_milli)" ] && return 0
    sleep 15
  done
  echo "kube-state-metrics never matched the live request"; return 1
}
approve() { $K -n lab annotate deploy api --overwrite "autoscaling.kubecost.io/approved-recommendation=$1" >/dev/null; }
POLICY="$HERE/hack/lab/policy.yaml"
# ra POLICY [extra flags]: run "rightsize apply" for the current $LAB/rec.json
ra() { local pol="$1"; shift; "$RS" apply --recommendation "$LAB/rec.json" --policy "$pol" --context "$CTX" --audit-file "$AUDIT" --actor lab-e2e "$@"; }

# restarts inside the recommender window (for example from a previous run's injected crash) veto reductions: wait until there are none
wait_quiet() {
  for _ in $(seq 1 90); do
    v="$(curl -s "$PROM/api/v1/query" --data-urlencode 'query=sum(increase(kube_pod_container_status_restarts_total{namespace="lab"}[14m]))' | python -c 'import json,sys
r = json.load(sys.stdin)["data"]["result"]
print(r[0]["value"][1] if r else 0)')"
    python -c "import sys; sys.exit(0 if float(sys.argv[1]) < 0.01 else 1)" "$v" && return 0
    echo "waiting for restarts to leave the 14m window (now $v)"; sleep 20
  done
  return 1
}
wait_quiet || { echo "restarts never left the window"; exit 2; }

echo "== 0. baseline"; BEFORE="$(req)"; echo "requests before: $BEFORE"
echo "== 1. recommendation from real Prometheus"
newrec || { bad "recommend failed"; exit 1; }
ID="$(jget "$LAB/rec.json" "ID")"
check "recommendation is not an abstention" "[ \"\$(jget $LAB/rec.json \"Abstained\")\" = False ]"
check "CPU decrease proposed"               "[ \"\$(jget $LAB/rec.json \"CPU.Action\")\" = Decrease ]"

echo "== 2. negative cases (nothing may change)"
echo "{}" > "$LAB/empty.yaml"
OUT="$(ra "$LAB/empty.yaml" 2>/dev/null)"
check "empty policy denies"                  "echo '$OUT' | grep -q 'allowNamespaces'"
OUT="$(ra "$POLICY" 2>/dev/null)"
check "no approval -> needs approval"        "echo '$OUT' | grep -q '\"needsApproval\": true'"
printf 'allowNamespaces: [lab]\nkillSwitch: true\n' > "$LAB/kill.yaml"
approve "$ID"
OUT="$(ra "$LAB/kill.yaml" 2>/dev/null)"
check "kill switch denies even when approved" "echo '$OUT' | grep -q 'kill switch'"
OUT="$(ra "$POLICY" 2>/dev/null)"
check "approved + allowed dry-run reports allowed" "echo '$OUT' | grep -q '\"allowed\": true'"
check "dry-run changed nothing"              "[ \"\$(req)\" = \"$BEFORE\" ]"
check "--execute without --context is refused" "! \"$RS\" apply --recommendation $LAB/rec.json --policy $HERE/hack/lab/policy.yaml --execute >/dev/null 2>&1"

echo "== 3. controlled apply (observe $OBSERVE)"
ra "$POLICY" --execute --observe-for "$OBSERVE" --poll 5s --rollout-timeout 3m > "$LAB/apply.out" 2>/dev/null
cat "$LAB/apply.out" | tr -d '\n ' ; echo
AFTER="$(req)"; echo "requests after: $AFTER"
check "applied and healthy"                  "grep -q '\"healthy\": true' $LAB/apply.out"
check "requests changed on the live object"  "[ \"\$AFTER\" != \"$BEFORE\" ]"
check "pods run with the new requests"       "[ \"\$($K -n lab get pods -l app=api -o jsonpath='{.items[0].spec.containers[0].resources.requests.cpu}')\" = \"\${AFTER%%/*}\" ]"
OUT="$(ra "$POLICY" 2>/dev/null)"
check "same recommendation is not applied twice" "echo '$OUT' | grep -q 'already applied'"

scenario_rec() {  # fresh recommendation that proposes a decrease, or SKIP
  newrec || { bad "recommend failed"; return 1; }
  if [ "$(jget "$LAB/rec.json" "CPU.Action")" != "Decrease" ] && [ "$(jget "$LAB/rec.json" "Memory.Action")" != "Decrease" ]; then
    echo "SKIP  recommendation proposes no decrease (workload already right-sized, or restarts in the window): scenario not run"; SKIPPED=1; return 1
  fi
  return 0
}

echo "== 4. SLO breach (synthetic signal through real Prometheus) -> automatic rollback"
if scenario_rec; then
  ID3="$(jget "$LAB/rec.json" "ID")"; STATE="$(req)"
  approve "$ID3"
  ra "$POLICY" --execute --observe-for 120s --poll 5s --rollout-timeout 3m --slo-prometheus-url "$PROM" --slo-breach-query 'vector(1)' > "$LAB/slo.out" 2>/dev/null
  tr -d '
 ' < "$LAB/slo.out"; echo
  check "SLO breach detected"                  "grep -q 'SLO breached' $LAB/slo.out"
  check "rolled back"                          "grep -q '\"rolledBack\": true' $LAB/slo.out"
  check "previous requests are back"           "[ \"\$(req)\" = \"$STATE\" ]"
  $K -n lab rollout status deploy/api --timeout=180s >/dev/null
fi

echo "== 5. injected fault: a container crashes after the change -> automatic rollback"
if scenario_rec; then
  ID2="$(jget "$LAB/rec.json" "ID")"; STATE="$(req)"
  approve "$ID2"
  # MSYS_NO_PATHCONV stops Git Bash on Windows from rewriting /tmp/crash into a Windows path
  ( sleep 45; P="$($K -n lab get pods -l app=api --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}')"; MSYS_NO_PATHCONV=1 $K -n lab exec "$P" -- touch /tmp/crash >/dev/null 2>&1 ) &
  ra "$POLICY" --execute --observe-for 120s --poll 5s --rollout-timeout 3m > "$LAB/fault.out" 2>/dev/null
  wait
  tr -d '
 ' < "$LAB/fault.out"; echo
  check "degradation detected (restart)"       "grep -q 'restart' $LAB/fault.out"
  check "rolled back automatically"            "grep -q '\"rolledBack\": true' $LAB/fault.out"
  check "previous requests are back"           "[ \"\$(req)\" = \"$STATE\" ]"
  check "rolled-back marker recorded"          "[ \"\$($K -n lab get deploy api -o jsonpath='{.metadata.annotations.autoscaling\.kubecost\.io/rolled-back-recommendation}')\" = \"$ID2\" ]"
  OUT="$(ra "$POLICY" 2>/dev/null)"
  check "rolled-back recommendation is not retried" "echo '$OUT' | grep -q 'rolled back before'"
fi

echo "== 6. audit trail"
(cd "$LAB" && python -c "
import json,collections
ev=[json.loads(l) for l in open('audit.jsonl')]
print(len(ev),'events:',collections.Counter(e['type'] for e in ev))
for e in ev:
    if e['type'] in ('applied','verified','degraded','rolled_back'): print(' ',e['time'][11:19],e['type'],e['recommendationId'],e['actor'],json.dumps(e.get('details',{}))[:140])")
check "audit has applied + verified"        "grep -q '\"type\":\"applied\"' $AUDIT && grep -q '\"type\":\"verified\"' $AUDIT"
check "audit has degraded + rolled_back"    "[ \"$SKIPPED\" = 1 ] || { grep -q '\"type\":\"degraded\"' $AUDIT && grep -q '\"type\":\"rolled_back\"' $AUDIT; }"

echo; echo "RESULT: $PASS passed, $FAILN failed"
[ "$FAILN" -eq 0 ]
