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
# REGISTRY=<host> (a managed cluster): push the images there instead of loading them into minikube; the cluster must be able to pull.
IMG=""; PULL=Never
if [ -n "${REGISTRY:-}" ]; then
  IMG="$REGISTRY/"; PULL=Always
  for i in operator engine; do docker tag kubecost-auto-hpa-$i:lab "$IMG"kubecost-auto-hpa-$i:lab && docker push -q "$IMG"kubecost-auto-hpa-$i:lab >/dev/null || exit 2; done
else
  minikube -p kubecost-lab image load kubecost-auto-hpa-operator:lab kubecost-auto-hpa-engine:lab || exit 2
fi
# LAB_SMALL=1 (a managed node with little quota): api asks 500m/256Mi instead of 1/1Gi; it still uses ~0.23 CPU, so a decrease is still due.
apiyaml() { if [ -n "${LAB_SMALL:-}" ]; then sed -e 's/cpu: "1", memory: 1Gi}/cpu: 500m, memory: 256Mi}/' -e 's/cpu: "2", memory: 1Gi}/cpu: "1", memory: 256Mi}/' "$HERE/hack/lab/api.yaml"; else cat "$HERE/hack/lab/api.yaml"; fi; }
$K create ns lab >/dev/null 2>&1; apiyaml | $K apply -f - >/dev/null
POLICY=$'allowNamespaces: [lab]\nmaxChangeFraction: 0.6\nmaxChanges: 9\nper: 1h\nminConfidence: Medium\n'
VALUES=(--set operator.image.repository="$IMG"kubecost-auto-hpa-operator --set operator.image.tag=lab --set operator.image.pullPolicy=$PULL
        --set engine.image.repository="$IMG"kubecost-auto-hpa-engine --set engine.image.tag=lab --set engine.image.pullPolicy=$PULL
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
# PF=1: Prometheus is behind a kubectl port-forward that can drop (a managed cluster); re-open it before each plan.
pfup() { [ -n "${PF:-}" ] || return 0; curl -fs -m 3 "$PROM/-/ready" >/dev/null && return 0
  ($K -n monitoring port-forward svc/prom-prometheus-server "${PROM##*:}":80 >/dev/null 2>&1 &); sleep 5; }
plan() { pfup; "$RS" plan --context "$CTX" --prometheus-url "$PROM" --namespace lab --workload "$WL" --container app --window 14m --step 15s --min-window 8m --min-samples 40 --publish > "$LAB/rec-applier.json" 2>"$LAB/plan-applier.err"; }
# usable = not abstained AND at least the policy's minConfidence (Medium): a Low one is refused by the policy, which is right but not what a scenario tests
planok() { for _ in $(seq 1 "${1:-40}"); do plan && [ "$(python -c "import json,sys; r=json.load(open(sys.argv[1])); print(not r['Abstained'] and r['Confidence'] in ('Medium','High'))" "$LAB/rec-applier.json")" = True ] && return 0; sleep 20; done; return 1; }
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
# The chart's ValidatingAdmissionPolicy (enforceSeparationOfDuties, the default) refuses any object without an honest published-by
# stamp, so a bare copy never reaches the operator. Check that first, then give the copy a valid stamp and a valid approval so that
# it is the OPERATOR's namespace check that is tested: publisher = the admin running this lab, approver = rs-appr (bound in "other").
ME="$($K auth whoami -o jsonpath='{.status.userInfo.username}' 2>/dev/null)"
python - "$LAB/rec-applier.json" "" > "$LAB/cross-bare.yaml" <<'PYEOF'
import json, sys
rec = json.load(open(sys.argv[1]))          # a record that targets namespace "lab"
meta = {"name": "cross", "namespace": "other"}
if sys.argv[2]:
    meta["annotations"] = {"autoscaling.kubecost.io/published-by": sys.argv[2]}
print(json.dumps({"apiVersion": "autoscaling.kubecost.io/v1alpha1", "kind": "WorkloadRecommendation", "metadata": meta,
  "spec": {"recommendationID": rec["ID"], "target": {"kind": "Deployment", "name": rec["Workload"], "container": "app"}, "record": json.dumps(rec)}}))
PYEOF
if $K get validatingadmissionpolicy 2>/dev/null | grep -q rec-published-by; then
  $K apply -f "$LAB/cross-bare.yaml" > "$LAB/cross-bare.out" 2>&1
  check "an object without an honest published-by stamp is refused by the API server" "grep -qi 'published-by must be present' $LAB/cross-bare.out"
fi
python - "$LAB/rec-applier.json" "$ME" > "$LAB/cross.yaml" <<'PYEOF'
import json, sys
rec = json.load(open(sys.argv[1]))
meta = {"name": "cross", "namespace": "other", "annotations": {"autoscaling.kubecost.io/published-by": sys.argv[2]}}
print(json.dumps({"apiVersion": "autoscaling.kubecost.io/v1alpha1", "kind": "WorkloadRecommendation", "metadata": meta,
  "spec": {"recommendationID": rec["ID"], "target": {"kind": "Deployment", "name": rec["Workload"], "container": "app"}, "record": json.dumps(rec)}}))
PYEOF
$K apply -f "$LAB/cross.yaml" > "$LAB/cross.out" 2>&1
$K -n other create rolebinding rs-appr --clusterrole=kubecost-rightsize-approve --serviceaccount=lab:rs-appr --dry-run=client -o yaml | $K apply -f - >/dev/null
APPR=system:serviceaccount:lab:rs-appr
for _ in $(seq 1 10); do $K -n other auth can-i patch workloadrecommendations/status --as "$APPR" >/dev/null 2>&1 && break; sleep 2; done   # RBAC propagation
$K -n other patch wrec cross --subresource=status --type=merge --as "$APPR" -p "{\"status\":{\"approval\":{\"recommendationID\":\"$(python -c "import json,sys; print(json.load(open(sys.argv[1]))['ID'])" "$LAB/rec-applier.json")\",\"by\":\"$APPR\",\"at\":\"$(date -u +%FT%TZ)\"}}}" >> "$LAB/cross.out" 2>&1
for _ in $(seq 1 12); do [ -n "$($K -n other get wrec cross -o jsonpath='{.status.phase}' 2>/dev/null)" ] && break; sleep 5; done
[ "$($K -n other get wrec cross -o jsonpath='{.status.phase}' 2>/dev/null)" = Denied ] || head -c 600 "$LAB/cross.out"
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
# Crash the changed pods while the operator observes them. Retry the injection until a restart is really seen: a managed cluster
# can take longer to roll out than a laptop, and a single exec may land on a pod that is about to be replaced.
: > "$LAB/crash-diag.txt"
( for _ in $(seq 1 60); do [ "$(phase $W4)" = Applying ] && break; sleep 2; done
  for _ in $(seq 1 30); do
    [ "$(phase $W4)" = Applying ] || break
    R="$($K -n lab get pods -l app=api4 -o jsonpath='{range .items[*]}{.status.containerStatuses[0].restartCount}{" "}{end}')"
    echo "$(date -u +%TZ) phase=$(phase $W4) restarts=[$R]" >> "$LAB/crash-diag.txt"
    echo "$R" | grep -q '[1-9]' && break
    for P in $($K -n lab get pods -l app=api4 --field-selector=status.phase=Running -o jsonpath='{.items[*].metadata.name}'); do
      MSYS_NO_PATHCONV=1 $K -n lab exec "$P" -- touch /tmp/crash >>"$LAB/crash-diag.txt" 2>&1; done
    sleep 5
  done ) &
waitphase "$W4" RolledBack 60
wait
if [ "$(phase $W4)" != RolledBack ]; then
  { echo "== final phase $(phase $W4): $($K -n lab get wrec $W4 -o jsonpath='{.status.message} {.status.reasons}')"
    $K -n lab get pods -l app=api4 -o wide; $K -n "$NS" logs "$OP" --tail=200 2>&1 | grep -i "api4" | tail -n 20 | cut -c1-300; } >> "$LAB/crash-diag.txt" 2>&1
  tail -n 25 "$LAB/crash-diag.txt"
fi
check "phase RolledBack"                           "[ \"\$(phase $W4)\" = RolledBack ]"
check "the previous requests are back"             "[ \"\$(req)\" = \"$STATE4\" ]"
check "the reason names the restart"               "$K -n lab get wrec $W4 -o jsonpath='{.status.reasons}' | grep -qi restart"
check "a Warning event was recorded on the object" "$K -n lab get events --field-selector involvedObject.name=$W4 2>/dev/null | grep -q Warning"

echo "== diagnostics (final phase of every object; last approve error, if any)"
[ -s "$LAB/approve-last.err" ] && head -c 400 "$LAB/approve-last.err"
$K -n lab get wrec -o jsonpath='{range .items[*]}{.metadata.name}{" phase="}{.status.phase}{" msg="}{.status.message}{" reasons="}{.status.reasons}{"\n"}{end}' 2>/dev/null | cut -c1-300
echo "== cleanup"
if [ "${KEEP:-0}" = 1 ]; then echo "KEEP=1: leaving the objects and the release"; echo; echo "RESULT: $PASS passed, $FAILN failed"; [ "$FAILN" -eq 0 ]; exit; fi
$K delete wrec --all -n lab >/dev/null 2>&1; $K delete wrec --all -n other >/dev/null 2>&1; $K delete ns other --ignore-not-found >/dev/null 2>&1
$K delete clusterrolebinding rs-pub-read rs-appr-read --ignore-not-found >/dev/null 2>&1
$K -n lab delete rolebinding rs-pub rs-appr sa/rs-pub sa/rs-appr --ignore-not-found >/dev/null 2>&1
$H uninstall "$REL" -n "$NS" >/dev/null 2>&1; $K delete ns "$NS" --ignore-not-found >/dev/null 2>&1
echo; echo "RESULT: $PASS passed, $FAILN failed"
[ "$FAILN" -eq 0 ]
