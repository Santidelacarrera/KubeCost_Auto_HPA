#!/usr/bin/env bash
# Hardening checks on a REAL lean cluster (minikube profile kubecost-hard, Kubernetes 1.33.4, no Prometheus needed):
#   A. upgrading the Helm chart from the version BEFORE this work (git commit 534c64d) to the current one, and rolling back
#   B. separation of duties enforced by the API server (ValidatingAdmissionPolicy), with real users via impersonation
#   C. 60 approved recommendations at once (observe-only): all reach DryRun, metrics agree, no reconcile errors
#   D. the API server is stopped for a while while changes are being observed: nothing is double-applied, nothing ends Failed
# REAL: the cluster, Helm upgrade/rollback, admission, RBAC, the operator image built from this checkout. HAND-MADE: the recommendations (no Prometheus here).
set -u
PROFILE="${PROFILE:-kubecost-hard}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"; LAB="$HERE/.lab"; mkdir -p "$LAB"
K="kubectl --context $PROFILE"; H="helm --kube-context $PROFILE"
REL=hard; NS=kubecost-auto-hpa-system
OLD_COMMIT="${OLD_COMMIT:-534c64d}"
PASS=0; FAILN=0
ok()  { echo "PASS  $*"; PASS=$((PASS+1)); }
bad() { echo "FAIL  $*"; FAILN=$((FAILN+1)); }
check() { if eval "$2"; then ok "$1"; else bad "$1"; fi; }
[ "$PROFILE" = "kubecost-hard" ] || [ "${ALLOW_CTX:-}" = "1" ] || { echo "refusing profile '$PROFILE'"; exit 2; }
docker info >/dev/null 2>&1 || { echo "Docker is not running"; exit 2; }
if ! minikube profile list 2>/dev/null | grep -q "$PROFILE"; then
  minikube start -p "$PROFILE" --driver=docker --kubernetes-version=v1.33.4 --cpus=2 --memory=3500 --wait=all || exit 2
fi
(cd "$HERE/operator" && go build -o "$LAB/rightsize.exe" ./cmd/rightsize) || exit 2
RS="$LAB/rightsize.exe"
docker build -q -t kubecost-auto-hpa-operator:lab --build-arg VERSION=lab "$HERE/operator" >/dev/null || exit 2
docker build -q -t kubecost-auto-hpa-engine:lab "$HERE/engine" >/dev/null || exit 2
minikube -p "$PROFILE" image load kubecost-auto-hpa-operator:lab kubecost-auto-hpa-engine:lab || exit 2

COMMON=(--set operator.image.repository=kubecost-auto-hpa-operator --set operator.image.tag=lab --set operator.image.pullPolicy=Never
        --set engine.image.repository=kubecost-auto-hpa-engine --set engine.image.tag=lab --set engine.image.pullPolicy=Never
        --set operator.replicas=1 --set engine.replicas=1 --set operator.leaderElect=false
        --set operator.podDisruptionBudget.enabled=false --set engine.podDisruptionBudget.enabled=false
        --set prometheus.url=http://prom.invalid:9090 --set kubecost.url=)
APPLIER=(--set operator.recommendationApplier.enabled=true --set "operator.recommendationApplier.namespaces={lab}" --set operator.recommendationApplier.maxObserve=60s
         --set-string "operator.recommendationApplier.policy=$(printf 'allowNamespaces: [lab]\nmaxChangeFraction: 0.6\nmaxChanges: 500\nper: 1h\nminConfidence: Medium\n')")
CRDS="$HERE/deploy/helm/kubecost-auto-hpa/crds"
OPPOD() { $K -n "$NS" get pods -l app.kubernetes.io/component=operator --sort-by=.metadata.creationTimestamp -o name | tail -1; }   # the newest, not a terminating old one
op_ready() { for _ in $(seq 1 40); do [ "$($K -n "$NS" get pods -l app.kubernetes.io/component=operator --field-selector=status.phase=Running -o jsonpath='{.items[0].status.containerStatuses[0].ready}' 2>/dev/null)" = true ] && return 0; sleep 5; done; return 1; }

echo "== A. chart upgrade from the previous version ($OLD_COMMIT) and back"
$K create ns lab >/dev/null 2>&1
rm -rf "$LAB/oldchart"; mkdir -p "$LAB/oldchart"
git -C "$HERE" archive "$OLD_COMMIT" deploy/helm/kubecost-auto-hpa | tar -x -C "$LAB/oldchart" || { echo "cannot extract the old chart"; exit 2; }
OLDCHART="$LAB/oldchart/deploy/helm/kubecost-auto-hpa"
$H uninstall "$REL" -n "$NS" >/dev/null 2>&1
$K delete crd workloadrecommendations.autoscaling.kubecost.io --ignore-not-found >/dev/null 2>&1   # start from a cluster that never saw the new CRD
$H install "$REL" "$OLDCHART" -n "$NS" --create-namespace "${COMMON[@]}" --wait --timeout 5m >/dev/null 2>&1
check "the OLD chart installs and its operator is ready"      "op_ready"
check "the new CRD does not exist yet"                        "! $K get crd workloadrecommendations.autoscaling.kubecost.io >/dev/null 2>&1"
$H upgrade "$REL" "$HERE/deploy/helm/kubecost-auto-hpa" -n "$NS" "${COMMON[@]}" "${APPLIER[@]}" --wait --timeout 3m >/dev/null 2>&1
check "helm upgrade completed (revision 2)"                   "[ \"\$($H history $REL -n $NS -o json | python -c 'import json,sys; print(len(json.load(sys.stdin)))')\" = 2 ]"
check "KNOWN LIMITATION: helm upgrade does NOT install the new CRD from crds/" "! $K get crd workloadrecommendations.autoscaling.kubecost.io >/dev/null 2>&1"
sleep 150   # long enough for the manager to give up waiting for a cache of a kind that does not exist
echo "operator after the upgrade WITHOUT the new CRD: $($K -n "$NS" get pods -l app.kubernetes.io/component=operator -o jsonpath='{range .items[*]}{.metadata.name}{" ready="}{.status.containerStatuses[0].ready}{" restarts="}{.status.containerStatuses[0].restartCount}{"\n"}{end}')"
echo "its log says: $($K -n "$NS" logs "$(OPPOD)" --tail=30 2>&1 | grep -iE 'no matches|not found|WorkloadRecommendation|unable|exited' | head -3 | cut -c1-240)"
$K apply -f "$CRDS" >/dev/null
OLDPOD="$(OPPOD)"
$K -n "$NS" delete "$OLDPOD" --wait=false >/dev/null 2>&1
for _ in $(seq 1 40); do [ "$(OPPOD)" != "$OLDPOD" ] && break; sleep 3; done
check "after kubectl apply -f crds/ and a restart the operator is ready with the applier" "op_ready && $K -n $NS logs \$(OPPOD) --tail=50 2>&1 | grep -q 'recommendation applier enabled'"
check "the pre-existing CRDs were not touched (KubeCostAutoscaler still served)" "$K get crd kubecostautoscalers.autoscaling.kubecost.io >/dev/null 2>&1"
$H rollback "$REL" 1 -n "$NS" --wait --timeout 3m >/dev/null 2>&1
check "helm rollback to the old chart works and its operator is ready"  "op_ready"
check "the new CRD is kept by the rollback (Helm never deletes CRDs)"   "$K get crd workloadrecommendations.autoscaling.kubecost.io >/dev/null 2>&1"
$H upgrade "$REL" "$HERE/deploy/helm/kubecost-auto-hpa" -n "$NS" "${COMMON[@]}" "${APPLIER[@]}" --wait --timeout 3m >/dev/null 2>&1
check "upgrading again lands on the current chart, applier on"      "op_ready && $K -n $NS logs \$(OPPOD) --tail=50 2>&1 | grep -q 'recommendation applier enabled'"

echo "== workloads and users"
mk() { cat <<YAML | $K apply -f - >/dev/null
apiVersion: apps/v1
kind: Deployment
metadata: {name: $1, namespace: lab}
spec:
  replicas: 1
  selector: {matchLabels: {app: $1}}
  template:
    metadata: {labels: {app: $1}}
    spec:
      containers:
      - name: app
        image: busybox:1.36
        command: ["sh", "-c", "while true; do sleep 30; done"]
        resources: {requests: {cpu: 500m, memory: 128Mi}, limits: {cpu: "1", memory: 256Mi}}
YAML
}
for w in web w1 w2 w3; do mk $w; done
for w in web w1 w2 w3; do $K -n lab rollout status deploy/$w --timeout=120s >/dev/null; done
$K apply -f "$HERE/deploy/rbac/rightsize.yaml" >/dev/null
for u in alice carol; do $K -n lab create rolebinding "$u-pub" --clusterrole=kubecost-rightsize-publish --user=$u --dry-run=client -o yaml | $K apply -f - >/dev/null
  $K create clusterrolebinding "$u-read" --clusterrole=kubecost-rightsize-read --user=$u --dry-run=client -o yaml | $K apply -f - >/dev/null; done
for u in bob alice; do $K -n lab create rolebinding "$u-appr" --clusterrole=kubecost-rightsize-approve --user=$u --dry-run=client -o yaml | $K apply -f - >/dev/null; done   # alice can publish AND approve
$K create clusterrolebinding bob-read --clusterrole=kubecost-rightsize-read --user=bob --dry-run=client -o yaml | $K apply -f - >/dev/null
mkrec() { python "$HERE/hack/lab/handmade-rec.py" "$1" "$2" lab > "$LAB/rec-$2.json"; }
as() { local u="$1"; shift; "$RS" "$@" --context "$PROFILE" --as "$u"; }
wrec() { $K -n lab get wrec "$1" -o jsonpath="$2" 2>/dev/null; }
name_of() { echo "$1-$2" | tr 'A-Z_' 'a-z-'; }

echo "== B. separation of duties enforced by the API server (alice publishes, bob approves)"
mkrec web adm-1
as alice publish --recommendation "$LAB/rec-adm-1.json" > "$LAB/pub1.out" 2>"$LAB/pub1.err"
N1="$(name_of web adm-1)"
check "alice publishes; published-by is stamped with her identity" "[ \"\$(wrec $N1 '{.metadata.annotations.autoscaling\\.kubecost\\.io/published-by}')\" = alice ]"
cat <<YAML > "$LAB/forged.yaml"
apiVersion: autoscaling.kubecost.io/v1alpha1
kind: WorkloadRecommendation
metadata: {name: forged, namespace: lab, annotations: {autoscaling.kubecost.io/published-by: bob}}
spec: {recommendationID: x, target: {kind: Deployment, name: web, container: app}, record: "{}"}
YAML
$K --as alice apply -f "$LAB/forged.yaml" > "$LAB/forged.out" 2>&1
check "alice cannot create an object claiming bob published it"    "grep -qi 'published-by must be present and equal' $LAB/forged.out"
as alice approve --cr --recommendation "$LAB/rec-adm-1.json" > "$LAB/self.out" 2>"$LAB/self.err"
check "alice cannot approve her own recommendation (admission)"    "grep -qi 'separation of duties' $LAB/self.err"
check "so it is still Pending"                                      "[ \"\$(wrec $N1 '{.status.phase}')\" = Pending ]"
$K -n lab get wrec "$N1" -o json | python -c "
import json, sys
o = json.load(sys.stdin)
o.setdefault('status', {})['approval'] = {'recommendationID': 'adm-1', 'by': 'alice', 'at': '2026-11-09T00:00:00Z'}
print(json.dumps(o))" | $K --as bob replace --subresource=status -f - > "$LAB/impers.out" 2>&1
check "bob cannot write an approval in alice's name (admission)"    "grep -qi 'cannot be written in somebody else' $LAB/impers.out"
as bob approve --cr --recommendation "$LAB/rec-adm-1.json" > "$LAB/bob.out" 2>"$LAB/bob.err"
check "bob approves properly"                                       "grep -q '\"approvedBy\": \"bob\"' $LAB/bob.out"
for _ in $(seq 1 24); do [ "$(wrec $N1 '{.status.phase}')" = DryRun ] && break; sleep 5; done
check "the operator's own status writes still pass admission: phase DryRun (observe-only)" "[ \"\$(wrec $N1 '{.status.phase}')\" = DryRun ]"
$K annotate wrec "$N1" -n lab autoscaling.kubecost.io/published-by=bob --overwrite > "$LAB/imm.out" 2>&1
check "published-by cannot be rewritten afterwards, even by a cluster admin" "grep -qi 'cannot be changed or removed' $LAB/imm.out"
$H upgrade "$REL" "$HERE/deploy/helm/kubecost-auto-hpa" -n "$NS" "${COMMON[@]}" "${APPLIER[@]}" --set operator.recommendationApplier.enforceSeparationOfDuties=false --wait --timeout 3m >/dev/null 2>&1
mkrec web adm-2
as alice publish --recommendation "$LAB/rec-adm-2.json" >/dev/null 2>&1
# the API server drops a deleted admission policy a few seconds after Helm removes it: retry for up to 60 s
for _ in $(seq 1 12); do
  as alice approve --cr --recommendation "$LAB/rec-adm-2.json" > "$LAB/self2.out" 2>"$LAB/self2.err"
  grep -q '"approvedBy": "alice"' "$LAB/self2.out" && break; sleep 5
done
check "with enforceSeparationOfDuties=false alice CAN approve her own (the switch works)" "grep -q '\"approvedBy\": \"alice\"' $LAB/self2.out"
$H upgrade "$REL" "$HERE/deploy/helm/kubecost-auto-hpa" -n "$NS" "${COMMON[@]}" "${APPLIER[@]}" --wait --timeout 3m >/dev/null 2>&1

echo "== C. 60 approved recommendations at once (observe-only on)"
START=$(date +%s)
for i in $(seq 1 60); do mkrec web "load-$i"; as alice publish --recommendation "$LAB/rec-load-$i.json" >/dev/null 2>&1; as bob approve --cr --recommendation "$LAB/rec-load-$i.json" >/dev/null 2>&1; done
echo "published and approved 60 objects in $(( $(date +%s) - START )) s"
for _ in $(seq 1 60); do [ "$($K -n lab get wrec -o jsonpath='{range .items[*]}{.status.phase}{"\n"}{end}' | grep -c '^DryRun$')" -ge 62 ] && break; sleep 5; done
DRY="$($K -n lab get wrec -o jsonpath='{range .items[*]}{.status.phase}{"\n"}{end}' | grep -c '^DryRun$')"
echo "objects in DryRun: $DRY"
check "all 60 load objects (plus the earlier ones) reach DryRun"   "[ \"$DRY\" -ge 62 ]"
check "the web workload was never written (observe-only)"           "[ \"\$($K -n lab get deploy web -o jsonpath='{.spec.template.spec.containers[0].resources.requests.cpu}')\" = 500m ]"
($K -n "$NS" port-forward "$(OPPOD)" 18080:8080 >/dev/null 2>&1 &); sleep 4
curl -s localhost:18080/metrics > "$LAB/op-metrics.txt"
check "the metric counted the DryRun transitions (>= 60)"           "python -c \"import re,sys; m=re.search(r'kubecost_recommendation_phase_transitions_total\\{phase=\\\"DryRun\\\"\\} (\\S+)', open(sys.argv[1]).read()); sys.exit(0 if m and float(m.group(1))>=60 else 1)\" $LAB/op-metrics.txt"
check "no reconcile errors of the applier"                          "! grep -E 'controller_runtime_reconcile_errors_total\\{controller=\"recommendation-applier\"\\} [1-9]' $LAB/op-metrics.txt"
echo "operator resident memory: $(grep '^process_resident_memory_bytes' $LAB/op-metrics.txt | awk '{printf "%.0f MiB", $2/1048576}')"
taskkill //F //IM kubectl.exe >/dev/null 2>&1
$K -n lab delete wrec --all >/dev/null 2>&1

echo "== D. the API server is stopped while changes are being observed (operator out of observe-only, 3 workloads)"
$H upgrade "$REL" "$HERE/deploy/helm/kubecost-auto-hpa" -n "$NS" "${COMMON[@]}" "${APPLIER[@]}" --set operator.observeOnly=false --wait --timeout 3m >/dev/null 2>&1
op_ready
for w in w1 w2; do mkrec $w "out-$w"; as alice publish --recommendation "$LAB/rec-out-$w.json" >/dev/null 2>&1; as bob approve --cr --recommendation "$LAB/rec-out-$w.json" >/dev/null 2>&1; done
for _ in $(seq 1 60); do [ "$($K -n lab get wrec -o jsonpath='{range .items[*]}{.status.phase}{"\n"}{end}' | grep -c '^Applying$')" -ge 1 ] && break; sleep 1; done
echo "phases at the moment of the outage: $($K -n lab get wrec -o jsonpath='{range .items[*]}{.status.phase}{" "}{end}')"
AT_BEFORE="$(for w in w1 w2; do $K -n lab get deploy $w -o jsonpath='{.metadata.annotations.autoscaling\.kubecost\.io/last-applied-at}'; done)"
echo "stopping kube-apiserver"
minikube -p "$PROFILE" ssh -- 'sudo crictl stop $(sudo crictl ps -q --name kube-apiserver)' >/dev/null 2>&1
DOWN="$(date +%s)"
sleep 40
for _ in $(seq 1 60); do $K get ns >/dev/null 2>&1 && break; sleep 3; done
echo "the API server was unreachable for about $(( $(date +%s) - DOWN )) s (kubelet restarts the static pod)"
check "the API server came back"                                    "$K get ns >/dev/null 2>&1"
for _ in $(seq 1 90); do [ "$($K -n lab get wrec -o jsonpath='{range .items[*]}{.status.phase}{"\n"}{end}' | grep -Ec '^(Verified|RolledBack|Failed)$')" -ge 2 ] && break; sleep 5; done
echo "phases after the outage: $($K -n lab get wrec -o jsonpath='{range .items[*]}{.metadata.name}{"="}{.status.phase}{" "}{end}')"
echo "why (status.reasons / message of each object):"
$K -n lab get wrec -o jsonpath='{range .items[*]}{.metadata.name}{": "}{.status.reasons}{" | "}{.status.message}{"\n"}{end}'
echo "operator log lines about the applier (last 40):"
echo "operator restarts: $($K -n "$NS" get "$(OPPOD)" -o jsonpath='{.status.containerStatuses[0].restartCount}')"
{ $K -n "$NS" logs "$(OPPOD)" --previous --tail=300 2>&1; $K -n "$NS" logs "$(OPPOD)" --tail=400 2>&1; } | grep -iE 'applier|recommendation|rollback|degrad|observe|w1|w2' | cut -c1-400 | tail -40
check "both finished Verified (none Failed, none stuck)"            "[ \"\$($K -n lab get wrec -o jsonpath='{range .items[*]}{.status.phase}{\"\\n\"}{end}' | grep -c '^Verified$')\" = 2 ]"
AT_AFTER="$(for w in w1 w2; do $K -n lab get deploy $w -o jsonpath='{.metadata.annotations.autoscaling\.kubecost\.io/last-applied-at}'; done)"
check "each change was applied exactly once (last-applied-at unchanged)" "[ \"$AT_BEFORE\" = \"$AT_AFTER\" ]"
check "the workloads carry the new requests"                         "[ \"\$($K -n lab get deploy w1 -o jsonpath='{.spec.template.spec.containers[0].resources.requests.cpu}')\" = 250m ]"

echo "== cleanup"
$K -n lab delete wrec --all >/dev/null 2>&1; $K -n lab delete deploy web w1 w2 w3 --ignore-not-found >/dev/null 2>&1
$H uninstall "$REL" -n "$NS" >/dev/null 2>&1
echo; echo "RESULT: $PASS passed, $FAILN failed"
[ "$FAILN" -eq 0 ]
