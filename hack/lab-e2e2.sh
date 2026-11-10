#!/usr/bin/env bash
# Second real-cluster check (needs hack/lab-up.sh, hack/lab/api.yaml + hack/lab/more.yaml applied, Prometheus on :9090, optionally OpenCost on :9091).
#
# REAL here: Kubernetes 1.33.4, a real HPA, the real VerticalPodAutoscaler CRD (no VPA controller runs: the object is only SEEN), a StatefulSet,
# the API server's own SelfSubjectReview identity, kube-state-metrics/cAdvisor via Prometheus, a real OpenCost (default pricing, NOT a bill).
# SIMULATED: the Argo CD tracking annotation (no Argo CD is installed), billing data for the cost report.
set -u
CTX="${CTX:-kubecost-lab}"
PROM="${PROM:-http://localhost:9090}"
OPENCOST="${OPENCOST:-http://localhost:9091}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"
LAB="$HERE/.lab"; mkdir -p "$LAB"
PASS=0; FAILN=0
ok()  { echo "PASS  $*"; PASS=$((PASS+1)); }
bad() { echo "FAIL  $*"; FAILN=$((FAILN+1)); }
check() { if eval "$2"; then ok "$1"; else bad "$1"; fi; }
[ "$CTX" = "kubecost-lab" ] || [ "${ALLOW_CTX:-}" = "1" ] || { echo "refusing context '$CTX'"; exit 2; }
K="kubectl --context $CTX"
$K get ns lab >/dev/null 2>&1 || { echo "lab namespace missing"; exit 2; }
curl -fs "$PROM/-/ready" >/dev/null || { echo "Prometheus not reachable at $PROM"; exit 2; }
(cd "$HERE/operator" && go build -o "$LAB/rightsize.exe" ./cmd/rightsize) || exit 2
RS="$LAB/rightsize.exe"; AUDIT="$LAB/audit2.jsonl"; : > "$AUDIT"
POLICY="$HERE/hack/lab/policy.yaml"

jget() { python -c "import json,sys
d = json.load(open(sys.argv[1]))
for k in sys.argv[2].split('.'):
    d = d[k]
print(d)" "$1" "$2"; }
plan() { # plan KIND NAME -> $LAB/rec-NAME.json
  "$RS" plan --context "$CTX" --prometheus-url "$PROM" --namespace lab --kind "$1" --workload "$2" --container app --window 14m --step 15s --min-window 8m --min-samples 40 > "$LAB/rec-$2.json" 2>"$LAB/plan-$2.err"
}
ra() { local f="$1"; shift; "$RS" apply --recommendation "$LAB/rec-$f.json" --policy "$POLICY" --context "$CTX" --audit-file "$AUDIT" --actor lab-e2e2 "$@"; }

echo "== 0. wait for enough history (workloads must have run ~9 minutes)"
for _ in $(seq 1 60); do
  plan StatefulSet store && [ "$(jget "$LAB/rec-store.json" Abstained)" = False ] && break
  sleep 20
done
check "history is sufficient for a StatefulSet recommendation" "[ \"\$(jget $LAB/rec-store.json Abstained)\" = False ]"

echo "== 1. a real HPA on CPU blocks a CPU change"
plan Deployment hpa-app
check "plan sees the HPA and warns"           "grep -q 'HPA' $LAB/rec-hpa-app.json"
OUT="$(ra hpa-app 2>/dev/null)"
check "apply is denied because of the HPA"    "echo '$OUT' | grep -q 'HPA scales on CPU'"

echo "== 2. a real VerticalPodAutoscaler object in Auto mode makes the planner abstain"
plan Deployment vpa-app
check "recommendation abstained"              "[ \"\$(jget $LAB/rec-vpa-app.json Abstained)\" = True ]"
check "reason names the VPA"                  "grep -q 'VPA' $LAB/rec-vpa-app.json"

echo "== 3. Argo CD managed workload (annotation only; no Argo CD installed) is refused"
plan Deployment gitops-app
OUT="$(ra gitops-app 2>/dev/null)"
check "apply is denied: managed by argocd"    "echo '$OUT' | grep -q 'argocd'"

echo "== 4. exclusion label makes the planner abstain"
$K -n lab label deploy api autoscaling.kubecost.io/exclude=true --overwrite >/dev/null
plan Deployment api
check "excluded workload: abstained"          "[ \"\$(jget $LAB/rec-api.json Abstained)\" = True ]"
$K -n lab label deploy api autoscaling.kubecost.io/exclude- >/dev/null

echo "== 5. StatefulSet with identity-stamped approval (real SelfSubjectReview)"
WHO="$($K auth whoami -o jsonpath='{.status.userInfo.username}' 2>/dev/null)"; echo "api server says you are: $WHO"
check "kubectl auth whoami returned a username" "[ -n \"$WHO\" ]"
BEFORE="$($K -n lab get sts store -o jsonpath='{.spec.template.spec.containers[0].resources.requests.cpu}/{.spec.template.spec.containers[0].resources.requests.memory}')"
"$RS" approve --recommendation "$LAB/rec-store.json" --context "$CTX" --audit-file "$AUDIT" --actor lab-e2e2 > "$LAB/approve.out" 2>/dev/null
check "approve stamped the API server identity" "[ \"\$($K -n lab get sts store -o jsonpath='{.metadata.annotations.autoscaling\\.kubecost\\.io/approved-by}')\" = \"$WHO\" ]"
printf 'allowNamespaces: [lab]\nmaxChangeFraction: 0.6\nmaxChanges: 5\nper: 1h\napprovers: [someone-else]\n' > "$LAB/pol-wrong.yaml"
OUT="$("$RS" apply --recommendation "$LAB/rec-store.json" --policy "$LAB/pol-wrong.yaml" --context "$CTX" 2>/dev/null)"
check "a different approver list denies"      "echo '$OUT' | grep -q 'not in approvers'"
printf 'allowNamespaces: [lab]\nmaxChangeFraction: 0.6\nmaxChanges: 5\nper: 1h\napprovers: ["%s"]\n' "$WHO" > "$LAB/pol-right.yaml"
"$RS" apply --recommendation "$LAB/rec-store.json" --policy "$LAB/pol-right.yaml" --context "$CTX" --audit-file "$AUDIT" --actor lab-e2e2 --execute --observe-for 60s --poll 5s --rollout-timeout 4m > "$LAB/store.out" 2>/dev/null
tr -d '\n ' < "$LAB/store.out"; echo
AFTER="$($K -n lab get sts store -o jsonpath='{.spec.template.spec.containers[0].resources.requests.cpu}/{.spec.template.spec.containers[0].resources.requests.memory}')"
echo "StatefulSet requests: $BEFORE -> $AFTER"
check "StatefulSet change applied and healthy" "grep -q '\"healthy\": true' $LAB/store.out && [ \"$AFTER\" != \"$BEFORE\" ]"
check "pods were replaced and run with the new request" "[ \"\$($K -n lab get pod store-0 -o jsonpath='{.spec.containers[0].resources.requests.cpu}')\" = \"\${AFTER%%/*}\" ]"
"$RS" rollback --namespace lab --workload store --kind StatefulSet --context "$CTX" --audit-file "$AUDIT" --actor lab-e2e2 --wait 4m > "$LAB/store-rb.out" 2>/dev/null
check "manual rollback of the StatefulSet restored the values" "[ \"\$($K -n lab get sts store -o jsonpath='{.spec.template.spec.containers[0].resources.requests.cpu}/{.spec.template.spec.containers[0].resources.requests.memory}')\" = \"$BEFORE\" ]"

echo "== 6. GitOps proposal on a local git repository (no push, no cluster)"
G="$LAB/gitrepo"; rm -rf "$G"; mkdir -p "$G/k8s"
$K -n lab get sts store -o json | python "$HERE/hack/lab/make-manifest.py" > "$G/k8s/store.yaml"
git -C "$G" init -q -b main && git -C "$G" config user.email lab@example.com && git -C "$G" config user.name lab && git -C "$G" config commit.gpgsign false && git -C "$G" add . && git -C "$G" commit -q -m init
"$RS" propose --recommendation "$LAB/rec-store.json" --repo "$G" --file k8s/store.yaml > "$LAB/propose.out" 2>"$LAB/propose.err"
check "a branch with the change was created"  "git -C $G branch --list 'rightsize/*' | grep -q rightsize"
check "the original branch is untouched"      "[ \"\$(git -C $G rev-parse --abbrev-ref HEAD)\" = main ]"
check "the comment survived the edit"         "git -C $G show \"\$(git -C $G branch --list 'rightsize/*' | tr -d ' *'):k8s/store.yaml\" | grep -q 'keep this comment'"
check "nothing was pushed (no remote exists)" "[ -z \"\$(git -C $G remote)\" ] && grep -q '\"pushed\": false' $LAB/propose.out"

echo "== 7. outcome report from the audit trail"
"$RS" report --audit-file "$AUDIT" > "$LAB/report.out"
cat "$LAB/report.out" | tr -d '\n ' | cut -c1-300; echo
check "report counts an applied and a rolled-back change" "grep -q '\"Applied\": 1' $LAB/report.out && grep -q '\"RolledBack\": 1' $LAB/report.out"

echo "== 8. nodes and cost on the real cluster"
"$RS" nodes --context "$CTX" --node-hourly-usd 0.2 > "$LAB/nodes.out" 2>/dev/null
check "nodes report produced (single node: nothing removable)" "grep -q '\"Removable\": null' $LAB/nodes.out"
"$RS" cost --context "$CTX" --node-hourly-usd 0.2 --start "$(date -u -d '-1 hours' +%Y-%m-%dT%H:%M:%SZ)" --end "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --budget lab=0.001 > "$LAB/cost1.out" 2>/dev/null
check "cost report reconciles (ESTIMATED basis)" "grep -q '\"reconciled\": true' $LAB/cost1.out && grep -q '\"basis\": \"estimated\"' $LAB/cost1.out"
check "namespace budget exceeded is flagged and labelled estimated" "grep -q '\"over\": true' $LAB/cost1.out"
"$RS" cost --context "$CTX" --node-hourly-usd 0.4 --start "$(date -u -d '-1 hours' +%Y-%m-%dT%H:%M:%SZ)" --end "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --baseline-report "$LAB/cost1.out" --deviation-pct 20 > "$LAB/cost2.out" 2>/dev/null
check "doubling the node price shows up as a deviation" "grep -q '\"changePct\"' $LAB/cost2.out"

echo "== 9. KubeCost client against a REAL OpenCost (default pricing; the response format is real, the prices are not a bill)"
if curl -fs -m 10 "$OPENCOST/model/allocation?window=15m&aggregate=cluster,namespace,controllerKind,controller&accumulate=true" >/dev/null 2>&1; then
  (cd "$HERE/operator" && KUBECOST_LIVE_URL="$OPENCOST" KUBECOST_LIVE_NAMESPACE=lab KUBECOST_LIVE_CONTROLLER=api KUBECOST_LIVE_WINDOW=15m \
     go test -tags kubecost_live -count=1 -run Live -v ./internal/kubecost > "$LAB/live.out" 2>&1)
  tail -6 "$LAB/live.out"
  check "live contract test passes against OpenCost" "grep -q '^--- PASS' $LAB/live.out && ! grep -q '^--- FAIL' $LAB/live.out"
else
  echo "SKIP  OpenCost not reachable at $OPENCOST"
fi

echo; echo "RESULT: $PASS passed, $FAILN failed"
[ "$FAILN" -eq 0 ]
