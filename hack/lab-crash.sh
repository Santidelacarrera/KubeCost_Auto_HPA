#!/usr/bin/env bash
# Interruption test on the real lab: the tool is killed while it is watching a change. The previous state lives on the object,
# so a new process can still roll back, and the same recommendation can not be applied twice.
set -u
CTX="${CTX:-kubecost-lab}"; PROM="${PROM:-http://localhost:9090}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"; LAB="$HERE/.lab"; mkdir -p "$LAB"
PASS=0; FAILN=0
ok()  { echo "PASS  $*"; PASS=$((PASS+1)); }
bad() { echo "FAIL  $*"; FAILN=$((FAILN+1)); }
check() { if eval "$2"; then ok "$1"; else bad "$1"; fi; }
[ "$CTX" = "kubecost-lab" ] || [ "${ALLOW_CTX:-}" = "1" ] || { echo "refusing context '$CTX'"; exit 2; }
K="kubectl --context $CTX"
(cd "$HERE/operator" && go build -o "$LAB/rightsize.exe" ./cmd/rightsize) || exit 2
RS="$LAB/rightsize.exe"; AUDIT="$LAB/audit-crash.jsonl"; : > "$AUDIT"
POL="$HERE/hack/lab/policy.yaml"
req() { $K -n lab get deploy api -o jsonpath='{.spec.template.spec.containers[0].resources.requests.cpu}/{.spec.template.spec.containers[0].resources.requests.memory}'; }
ann() { $K -n lab get deploy api -o jsonpath="{.metadata.annotations.autoscaling\\.kubecost\\.io/$1}"; }

$K apply -f "$HERE/hack/lab/api.yaml" >/dev/null
$K -n lab annotate deploy api autoscaling.kubecost.io/approved-recommendation- autoscaling.kubecost.io/approved-by- autoscaling.kubecost.io/approved-at- autoscaling.kubecost.io/last-applied-at- autoscaling.kubecost.io/rolled-back-recommendation- autoscaling.kubecost.io/applied-recommendation- autoscaling.kubecost.io/previous-resources- >/dev/null 2>&1
$K -n lab rollout status deploy/api --timeout=180s >/dev/null
BEFORE="$(req)"; echo "requests before: $BEFORE"
"$RS" plan --context "$CTX" --prometheus-url "$PROM" --namespace lab --workload api --container app --window 14m --step 15s --min-window 8m --min-samples 40 > "$LAB/rec-crash.json" 2>"$LAB/plan-crash.err" || { cat "$LAB/plan-crash.err"; exit 2; }
ID="$(python -c "import json,sys; print(json.load(open(sys.argv[1]))['ID'])" "$LAB/rec-crash.json")"
"$RS" approve --recommendation "$LAB/rec-crash.json" --context "$CTX" --audit-file "$AUDIT" >/dev/null 2>&1

echo "== apply with a long observation, then kill the process"
"$RS" apply --recommendation "$LAB/rec-crash.json" --policy "$POL" --context "$CTX" --execute --observe-for 10m --poll 5s --rollout-timeout 4m --audit-file "$AUDIT" >"$LAB/crash.out" 2>/dev/null &
PID=$!
for _ in $(seq 1 30); do [ -n "$(ann applied-recommendation)" ] && break; sleep 2; done
check "the change was applied before the interruption" "[ \"\$(ann applied-recommendation)\" = \"$ID\" ]"
taskkill //F //PID "$PID" >/dev/null 2>&1 || kill -9 "$PID" 2>/dev/null
sleep 2
AFTER="$(req)"; echo "requests after the kill: $AFTER"
check "the new values are in place (the tool died mid-watch)" "[ \"$AFTER\" != \"$BEFORE\" ]"
check "the previous state survived on the object"             "[ -n \"\$(ann previous-resources)\" ]"
check "a second process refuses to apply the same ID again"   "\"$RS\" apply --recommendation $LAB/rec-crash.json --policy $POL --context $CTX 2>/dev/null | grep -q 'already applied'"

echo "== a NEW process rolls back from the saved state alone"
"$RS" rollback --namespace lab --workload api --context "$CTX" --audit-file "$AUDIT" --actor recovery --wait 4m >"$LAB/crash-rb.out" 2>&1
check "requests are back to the original"                     "[ \"\$(req)\" = \"$BEFORE\" ]"
check "the rolled-back marker is set and the saved state cleared" "[ \"\$(ann rolled-back-recommendation)\" = \"$ID\" ] && [ -z \"\$(ann previous-resources)\" ]"
check "audit has applied and rolled_back by the recovery actor" "grep -q '\"type\":\"applied\"' $AUDIT && grep -q '\"type\":\"rolled_back\".*\"actor\":\"recovery\"\\|\"actor\":\"recovery\".*\"type\":\"rolled_back\"' $AUDIT"
echo; echo "RESULT: $PASS passed, $FAILN failed"
[ "$FAILN" -eq 0 ]
