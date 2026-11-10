#!/usr/bin/env bash
# REAL cluster check of the opt-in recommendation applier (WorkloadRecommendation CRD + controller in the operator).
# Needs the full lab (hack/lab-up.sh), Prometheus on :9090, the namespace "lab" with workload "api" (hack/lab/api.yaml) and some history.
# REAL: apiserver, the CRD, the operator image built from this checkout, RBAC enforcement by impersonation, Prometheus.
# Injected: a crash file inside a container (a real restart) to trigger the rollback.
set -u
FROM="${FROM:-1}"   # FROM=8 re-runs only the last scenario (install and helpers always run)
CTX="${CTX:-kubecost-lab}"; PROM="${PROM:-http://localhost:9090}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"; LAB="$HERE/.lab"; mkdir -p "$LAB"
K="kubectl --context $CTX"; H="helm --kube-context $CTX"
REL=applier; NS=kubecost-auto-hpa-system
PASS=0; FAILN=0
ok()  { echo "PASS  $*"; PASS=$((PASS+1)); }
bad() { echo "FAIL  $*"; FAILN=$((FAILN+1)); }
check() { if eval "$2"; then ok "$1"; else bad "$1"; fi; }
[ "$CTX" = "kubecost-lab" ] || [ "${ALLOW_CTX:-}" = "1" ] || { echo "refusing context '$CTX'"; exit 2; }
curl -fs "$PROM/-/ready" >/dev/null || { echo "Prometheus not reachable"; exit 2; }
(cd "$HERE/operator" && go build -o "$LAB/rightsize.exe" ./cmd/rightsize) || exit 2
RS="$LAB/rightsize.exe"

echo "== 0. images and install (applier ON for namespace lab only, observe-only ON)"
docker build -q -t kubecost-auto-hpa-operator:lab --build-arg VERSION=lab "$HERE/operator" >/dev/null || exit 2
docker build -q -t kubecost-auto-hpa-engine:lab "$HERE/engine" >/dev/null || exit 2
minikube -p kubecost-lab image load kubecost-auto-hpa-operator:lab kubecost-auto-hpa-engine:lab || exit 2
$K create ns lab >/dev/null 2>&1; $K apply -f "$HERE/hack/lab/api.yaml" >/dev/null
POLICY=$'allowNamespaces: [lab]\nmaxChangeFraction: 0.6\nmaxChanges: 9\nper: 1h\nminConfidence: Medium\n'
VALUES=(--set operator.image.repository=kubecost-auto-hpa-operator --set operator.image.tag=lab --set operator.image.pullPolicy=Never
        --set engine.image.repository=kubecost-auto-hpa-engine --set engine.image.tag=lab --set engine.image.pullPolicy=Never
        --set operator.replicas=1 --set engine.replicas=1 --set operator.leaderElect=false
        --set operator.podDisruptionBudget.enabled=false --set engine.podDisruptionBudget.enabled=false
        --set prometheus.url=http://prom-prometheus-server.monitoring:80 --set kubecost.url=
        --set operator.recommendationApplier.enabled=true --set "operator.recommendationApplier.namespaces={lab}"
        --set operator.recommendationApplier.maxObserve=90s --set-string "operator.recommendationApplier.policy=$POLICY")
OBS=true; [ "$FROM" -ge 4 ] && OBS=false   # scenarios 4 and later need the operator out of observe-only
$H upgrade --install "$REL" "$HERE/deploy/helm/kubecost-auto-hpa" -n "$NS" --create-namespace "${VALUES[@]}" --set operator.observeOnly=$OBS --wait --timeout 5m >/dev/null 2>&1
OP="deploy/$REL-kubecost-auto-hpa-operator"
check "the CRD is installed"                       "$K get crd workloadrecommendations.autoscaling.kubecost.io >/dev/null 2>&1"
check "the operator runs the applier"              "$K -n $NS logs $OP --tail=60 2>&1 | grep -q 'recommendation applier enabled'"
[ "$FROM" -lt 4 ] && check "the operator is in observe-only mode"       "$K -n $NS get $OP -o jsonpath='{.spec.template.spec.containers[0].args}' | grep -q 'observe-only=true'"
$K apply -f "$HERE/deploy/rbac/rightsize.yaml" >/dev/null
$K -n lab create sa rs-pub --dry-run=client -o yaml | $K apply -f - >/dev/null; $K -n lab create sa rs-appr --dry-run=client -o yaml | $K apply -f - >/dev/null
$K create clusterrolebinding rs-pub-read --clusterrole=kubecost-rightsize-read --serviceaccount=lab:rs-pub --dry-run=client -o yaml | $K apply -f - >/dev/null
$K -n lab create rolebinding rs-pub --clusterrole=kubecost-rightsize-publish --serviceaccount=lab:rs-pub --dry-run=client -o yaml | $K apply -f - >/dev/null
$K create clusterrolebinding rs-appr-read --clusterrole=kubecost-rightsize-read --serviceaccount=lab:rs-appr --dry-run=client -o yaml | $K apply -f - >/dev/null
$K -n lab create rolebinding rs-appr --clusterrole=kubecost-rightsize-approve --serviceaccount=lab:rs-appr --dry-run=client -o yaml | $K apply -f - >/dev/null
WL=api
use() { WL="$1"; }
req() { $K -n lab get deploy "$WL" -o jsonpath='{.spec.template.spec.containers[0].resources.requests.cpu}/{.spec.template.spec.containers[0].resources.requests.memory}'; }
phase() { [ -n "${1:-}" ] && $K -n lab get wrec "$1" -o jsonpath='{.status.phase}' 2>/dev/null; }
waitphase() { for _ in $(seq 1 "$3"); do [ "$(phase "$1")" = "$2" ] && return 0; sleep 5; done; return 1; }
plan() { "$RS" plan --context "$CTX" --prometheus-url "$PROM" --namespace lab --workload "$WL" --container app --window 14m --step 15s --min-window 8m --min-samples 40 --publish > "$LAB/rec-applier.json" 2>"$LAB/plan-applier.err"; }
planok() { for _ in $(seq 1 "${1:-40}"); do plan && [ "$(python -c "import json,sys; print(json.load(open(sys.argv[1]))['Abstained'])" "$LAB/rec-applier.json")" = False ] && return 0; sleep 20; done; return 1; }
wname() { python -c "import json,sys; r=json.load(open(sys.argv[1])); print(r['Workload'].lower() + '-' + r['ID'].lower())" "$LAB/rec-applier.json"; }
approve_as() { "$RS" approve --cr --recommendation "$LAB/rec-applier.json" --context "$CTX" --as system:serviceaccount:lab:rs-appr >/dev/null 2>"$LAB/approve-last.err"; }
killswitch() { # killswitch on|off
  $K -n "$NS" get cm "$REL-kubecost-auto-hpa-applier-policy" -o json | python -c "
import json, sys
d = json.load(sys.stdin)
p = d['data']['policy.yaml'].replace('killSwitch: true\n', '')
if sys.argv[1] == 'on':
    p += 'killSwitch: true\n'
d['data']['policy.yaml'] = p
print(json.dumps(d))" "$1" | $K apply -f - >/dev/null 2>&1
}
# three more over-provisioned copies of api, one per later scenario, created now so they all gather history together
python "$HERE/hack/lab/make-copies.py" api2 api3 api4 | $K apply -f - >/dev/null

if [ "$FROM" -le 1 ]; then
echo "== 1. publish: creating the object changes nothing, and nobody has approved yet"
BEFORE="$(req)"
for _ in $(seq 1 40); do plan && [ "$(python -c "import json,sys; print(json.load(open(sys.argv[1]))['Abstained'])" "$LAB/rec-applier.json")" = False ] && break; sleep 20; done
W="$(wname)"
sleep 10
check "a WorkloadRecommendation exists"            "$K -n lab get wrec $W >/dev/null 2>&1"
check "it is Pending (waiting for approval)"       "[ \"\$(phase $W)\" = Pending ]"
check "the workload is untouched"                  "[ \"\$(req)\" = \"$BEFORE\" ]"
fi

if [ "$FROM" -le 2 ]; then
echo "== 2. approval needs its own permission: a publisher cannot approve, an approver can (RBAC by impersonation)"
"$RS" approve --cr --recommendation "$LAB/rec-applier.json" --context "$CTX" --as system:serviceaccount:lab:rs-pub >/dev/null 2>"$LAB/appr-pub.err"
check "the publisher is refused (Forbidden) when approving" "[ \$? -ne 0 ] && grep -qi forbidden $LAB/appr-pub.err"
check "and the phase is still Pending"             "[ \"\$(phase $W)\" = Pending ]"
fi

if [ "$FROM" -le 3 ]; then
echo "== 3. observe-only: an approval produces a DryRun, never a write"
"$RS" approve --cr --recommendation "$LAB/rec-applier.json" --context "$CTX" --as system:serviceaccount:lab:rs-appr > "$LAB/appr.out" 2>"$LAB/appr.err"
check "the approver succeeds and the identity is the server's" "grep -q 'system:serviceaccount:lab:rs-appr' $LAB/appr.out"
waitphase "$W" DryRun 24
check "phase DryRun (observe-only)"                "[ \"\$(phase $W)\" = DryRun ]"
check "the workload is still untouched"            "[ \"\$(req)\" = \"$BEFORE\" ]"
fi

if [ "$FROM" -le 4 ]; then
echo "== 4. observe-only OFF: the same approved object is applied, observed and verified"
$H upgrade "$REL" "$HERE/deploy/helm/kubecost-auto-hpa" -n "$NS" "${VALUES[@]}" --set operator.observeOnly=false --wait --timeout 5m >/dev/null 2>&1
waitphase "$W" Verified 60
check "phase Verified"                             "[ \"\$(phase $W)\" = Verified ]"
AFTER="$(req)"; echo "requests: $BEFORE -> $AFTER"
check "the workload really changed"                "[ \"$AFTER\" != \"$BEFORE\" ]"
check "the previous state is on the workload"      "[ -n \"\$($K -n lab get deploy api -o jsonpath='{.metadata.annotations.autoscaling\\.kubecost\\.io/previous-resources}')\" ]"
check "the object carries the approver and times"  "$K -n lab get wrec $W -o jsonpath='{.status.approval.by}{.status.appliedAt}{.status.finishedAt}' | grep -q 'rs-appr'"
fi

if [ "$FROM" -le 5 ]; then
echo "== 5. namespace isolation: an object in another namespace cannot act on lab"
$K create ns other >/dev/null 2>&1
python - "$LAB/rec-applier.json" > "$LAB/cross.yaml" <<'PYEOF'
import json, sys
rec = json.load(open(sys.argv[1]))          # a record that targets namespace "lab"
print(json.dumps({"apiVersion": "autoscaling.kubecost.io/v1alpha1", "kind": "WorkloadRecommendation",
  "metadata": {"name": "cross", "namespace": "other"},
  "spec": {"recommendationID": rec["ID"], "target": {"kind": "Deployment", "name": rec["Workload"], "container": "app"}, "record": json.dumps(rec)}}))
PYEOF
$K apply -f "$LAB/cross.yaml" >/dev/null 2>&1
$K -n other patch wrec cross --subresource=status --type=merge -p "{\"status\":{\"approval\":{\"recommendationID\":\"$(python -c "import json,sys; print(json.load(open(sys.argv[1]))['ID'])" "$LAB/rec-applier.json")\",\"by\":\"mallory\",\"at\":\"2026-11-09T00:00:00Z\"}}}" >/dev/null 2>&1
for _ in $(seq 1 12); do [ -n "$($K -n other get wrec cross -o jsonpath='{.status.phase}' 2>/dev/null)" ] && break; sleep 5; done
check "the cross-namespace object is Denied"       "[ \"\$($K -n other get wrec cross -o jsonpath='{.status.phase}')\" = Denied ]"
check "with the reason: own namespace"             "$K -n other get wrec cross -o jsonpath='{.status.message}' | grep -q 'own namespace'"
fi

if [ "$FROM" -le 6 ]; then
echo "== 6. the kill switch in the policy ConfigMap acts without a restart, and lifting it lets the same object proceed"
use api2
planok 40 || echo "no usable recommendation for api2"
W2="$(wname)"; STATE2="$(req)"
killswitch on; sleep 5
approve_as   # approved AFTER the switch is on
sleep 25
check "with the kill switch on: Denied"            "[ \"\$(phase $W2)\" = Denied ] && $K -n lab get wrec $W2 -o jsonpath='{.status.reasons}' | grep -q 'kill switch'"
check "and nothing was written"                    "[ \"\$(req)\" = \"$STATE2\" ]"
killswitch off
$K -n lab annotate wrec "$W2" touch="$(date +%s)" --overwrite >/dev/null
waitphase "$W2" Verified 60
check "after lifting the switch the same object is applied and Verified (policy re-read, no restart)" "[ \"\$(phase $W2)\" = Verified ] && [ \"\$(req)\" != \"$STATE2\" ]"
fi

if [ "$FROM" -le 7 ]; then
echo "== 7. restart the operator while a change is being observed: it resumes, it does not re-apply"
use api3
planok 40 || echo "no usable recommendation for api3"
W3="$(wname)"
approve_as
for _ in $(seq 1 60); do [ "$(phase $W3)" = Applying ] && break; sleep 2; done
sleep 10
AT_BEFORE="$($K -n lab get deploy api3 -o jsonpath='{.metadata.annotations.autoscaling\.kubecost\.io/last-applied-at}')"
OPPOD="$($K -n "$NS" get pods -o name | grep operator | head -1)"
$K -n "$NS" delete "$OPPOD" --wait=false >/dev/null 2>&1
waitphase "$W3" Verified 80
check "after the operator restart the change is Verified" "[ \"\$(phase $W3)\" = Verified ]"
check "it was applied exactly once (last-applied-at is unchanged)" "[ -n \"$AT_BEFORE\" ] && [ \"\$($K -n lab get deploy api3 -o jsonpath='{.metadata.annotations.autoscaling\\.kubecost\\.io/last-applied-at}')\" = \"$AT_BEFORE\" ]"
fi

echo "== 8. a crash after the change: automatic rollback by the controller (last, because it leaves restarts in the window)"
use api4
planok 40 || echo "no usable recommendation for api4"
W4="$(wname)"; STATE4="$(req)"
approve_as
( for _ in $(seq 1 60); do [ "$(phase $W4)" = Applying ] && break; sleep 2; done
  $K -n lab rollout status deploy/api4 --timeout=80s >/dev/null 2>&1; sleep 5
  # the NEWEST pod, not the first listed (that one may be the old pod still terminating)
  P="$($K -n lab get pods -l app=api4 --sort-by=.metadata.creationTimestamp -o jsonpath='{.items[-1:].metadata.name}')"
  MSYS_NO_PATHCONV=1 $K -n lab exec "$P" -- touch /tmp/crash >/dev/null 2>&1 ) &
waitphase "$W4" RolledBack 60
wait
check "phase RolledBack"                           "[ \"\$(phase $W4)\" = RolledBack ]"
check "the previous requests are back"             "[ \"\$(req)\" = \"$STATE4\" ]"
check "the reason names the restart"               "$K -n lab get wrec $W4 -o jsonpath='{.status.reasons}' | grep -qi restart"
check "a Warning event was recorded on the object" "$K -n lab get events --field-selector involvedObject.name=$W4 2>/dev/null | grep -q Warning"

echo "== diagnostics (final phase of every object; last approve error, if any)"
[ -s "$LAB/approve-last.err" ] && head -c 400 "$LAB/approve-last.err"
$K -n lab get wrec -o jsonpath='{range .items[*]}{.metadata.name}{" phase="}{.status.phase}{" msg="}{.status.message}{" reasons="}{.status.reasons}{"\n"}{end}' 2>/dev/null | cut -c1-300
echo "== cleanup"
if [ "${KEEP:-0}" = 1 ]; then echo "KEEP=1: leaving the objects and the release"; echo; echo "RESULT: $PASS passed, $FAILN failed"; [ "$FAILN" -eq 0 ]; exit; fi
$K delete wrec --all -n lab >/dev/null 2>&1; $K delete ns other --ignore-not-found >/dev/null 2>&1
$K delete clusterrolebinding rs-pub-read rs-appr-read --ignore-not-found >/dev/null 2>&1
$K -n lab delete rolebinding rs-pub rs-appr sa/rs-pub sa/rs-appr --ignore-not-found >/dev/null 2>&1
$H uninstall "$REL" -n "$NS" >/dev/null 2>&1; $K delete ns "$NS" --ignore-not-found >/dev/null 2>&1
echo; echo "RESULT: $PASS passed, $FAILN failed"
[ "$FAILN" -eq 0 ]
