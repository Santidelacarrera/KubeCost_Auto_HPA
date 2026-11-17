#!/usr/bin/env bash
# Proves on a real cluster that deploy/rbac/rightsize.yaml is enough for the rightsize CLI, and that without the apply role it cannot change anything.
# Uses --as impersonation of two ServiceAccounts in namespace "lab". Needs hack/lab-e2e.sh's workload "api" and Prometheus on :9090.
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
RS="$LAB/rightsize.exe"; AUDIT="$LAB/audit-rbac.jsonl"; : > "$AUDIT"
READER=system:serviceaccount:lab:rs-reader; APPLIER=system:serviceaccount:lab:rs-applier

$K apply -f "$HERE/deploy/rbac/rightsize.yaml" >/dev/null
$K -n lab create sa rs-reader --dry-run=client -o yaml | $K apply -f - >/dev/null
$K -n lab create sa rs-applier --dry-run=client -o yaml | $K apply -f - >/dev/null
# reader: read role only. applier: read role cluster-wide + apply role in namespace lab only.
$K create clusterrolebinding rs-reader --clusterrole=kubecost-rightsize-read --serviceaccount=lab:rs-reader --dry-run=client -o yaml | $K apply -f - >/dev/null
$K create clusterrolebinding rs-applier-read --clusterrole=kubecost-rightsize-read --serviceaccount=lab:rs-applier --dry-run=client -o yaml | $K apply -f - >/dev/null
$K -n lab create rolebinding rs-applier-apply --clusterrole=kubecost-rightsize-apply --serviceaccount=lab:rs-applier --dry-run=client -o yaml | $K apply -f - >/dev/null

echo "== 1. what each identity may do (kubectl auth can-i)"
can() { $K auth can-i "$@" 2>/dev/null; }
check "reader can list deployments cluster-wide"   "[ \"\$(can list deployments -A --as=$READER)\" = yes ]"
check "reader cannot update deployments"           "[ \"\$(can update deployments -n lab --as=$READER)\" = no ]"
check "reader cannot read secrets"                 "[ \"\$(can get secrets -n lab --as=$READER)\" = no ]"
check "applier can update deployments in lab"      "[ \"\$(can update deployments -n lab --as=$APPLIER)\" = yes ]"
check "applier cannot update deployments elsewhere" "[ \"\$(can update deployments -n kube-system --as=$APPLIER)\" = no ]"
check "applier cannot delete or create workloads"  "[ \"\$(can delete deployments -n lab --as=$APPLIER)\" = no ] && [ \"\$(can create deployments -n lab --as=$APPLIER)\" = no ]"

echo "== 2. the CLI works with exactly those roles"
plan() { "$RS" plan --context "$CTX" --as "$1" --prometheus-url "$PROM" --namespace lab --workload api --container app --window 14m --step 15s --min-window 8m --min-samples 40 > "$LAB/rec-rbac.json" 2>"$LAB/rbac-plan.err"; }
plan "$READER"
check "reader: plan works (read-only)"             "grep -q '\"ID\"' $LAB/rec-rbac.json"
"$RS" nodes --context "$CTX" --as "$READER" >/dev/null 2>&1; check "reader: nodes works" "[ \$? -eq 0 ]"
"$RS" cost --context "$CTX" --as "$READER" --node-hourly-usd 0.2 >/dev/null 2>&1; check "reader: cost works" "[ \$? -eq 0 ]"
POL="$HERE/hack/lab/policy.yaml"
"$RS" approve --recommendation "$LAB/rec-rbac.json" --context "$CTX" --as "$READER" >/dev/null 2>"$LAB/rbac-approve.err"
check "reader: approve is refused by RBAC (forbidden)" "[ \$? -ne 0 ] && grep -qi 'forbidden' $LAB/rbac-approve.err"
BEFORE="$($K -n lab get deploy api -o jsonpath='{.spec.template.spec.containers[0].resources.requests.cpu}')"
if [ "$(python -c "import json,sys; print(json.load(open(sys.argv[1]))['Abstained'])" "$LAB/rec-rbac.json")" = False ]; then
  "$RS" approve --recommendation "$LAB/rec-rbac.json" --context "$CTX" --as "$APPLIER" >/dev/null 2>&1; check "applier: approve works" "[ \$? -eq 0 ]"
  "$RS" apply --recommendation "$LAB/rec-rbac.json" --policy "$POL" --context "$CTX" --as "$READER" --execute --observe-for 20s --poll 5s --audit-file "$AUDIT" >"$LAB/rbac-apply-ro.out" 2>"$LAB/rbac-apply-ro.err"
  AFTER_RO="$($K -n lab get deploy api -o jsonpath='{.spec.template.spec.containers[0].resources.requests.cpu}')"
  check "reader: --execute cannot change the workload (RBAC)" "[ \"$AFTER_RO\" = \"$BEFORE\" ]"
  "$RS" apply --recommendation "$LAB/rec-rbac.json" --policy "$POL" --context "$CTX" --as "$APPLIER" --execute --observe-for 30s --poll 5s --rollout-timeout 3m --audit-file "$AUDIT" >"$LAB/rbac-apply.out" 2>"$LAB/rbac-apply.err"
  AFTER="$($K -n lab get deploy api -o jsonpath='{.spec.template.spec.containers[0].resources.requests.cpu}')"
  check "applier: --execute changes the workload with only update rights" "[ \"$AFTER\" != \"$BEFORE\" ]"
  "$RS" rollback --namespace lab --workload api --context "$CTX" --as "$APPLIER" --audit-file "$AUDIT" --wait 3m >/dev/null 2>&1
  check "applier: rollback restores the previous value" "[ \"\$($K -n lab get deploy api -o jsonpath='{.spec.template.spec.containers[0].resources.requests.cpu}')\" = \"$BEFORE\" ]"
else
  echo "SKIP  the planner abstained (not enough history): apply checks not run"
fi

$K delete clusterrolebinding rs-reader rs-applier-read --ignore-not-found >/dev/null 2>&1
$K -n lab delete rolebinding rs-applier-apply sa/rs-reader sa/rs-applier --ignore-not-found >/dev/null 2>&1
$K delete clusterrole kubecost-rightsize-read kubecost-rightsize-apply --ignore-not-found >/dev/null 2>&1
echo; echo "RESULT: $PASS passed, $FAILN failed"
[ "$FAILN" -eq 0 ]
