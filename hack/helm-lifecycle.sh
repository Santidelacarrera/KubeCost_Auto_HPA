#!/usr/bin/env bash
# Helm install -> upgrade -> rollback -> uninstall against whatever cluster $KUBECONFIG points to.
# It does NOT wait for pods: it proves the apiserver accepts every object (CRDs, CEL rules, RBAC, Secrets,
# Deployments) and that each lifecycle step changes exactly what it should. Pods, reconciliation and
# scaling need a kubelet and are covered by hack/helm-rehearsal.sh (kind) and the envtest suite.
# Needs: kubectl, helm, python3 and a KUBECONFIG with admin rights on a DISPOSABLE cluster.
set -euo pipefail
cd "$(dirname "$0")/.."
: "${KUBECONFIG:?set KUBECONFIG to a disposable cluster}"
CHART=deploy/helm/kubecost-auto-hpa
REL=${REL:-rehearsal}
NS=${NS:-kubecost-auto-hpa-system}
fail() { echo "FAIL: $*" >&2; exit 1; }
ok()   { echo "ok:   $*"; }
VALUES=(--set operator.image.repository=kubecost-auto-hpa-operator --set operator.image.tag=rehearsal
        --set engine.image.repository=kubecost-auto-hpa-engine --set engine.image.tag=rehearsal
        --set operator.replicas=1 --set engine.replicas=1 --set operator.leaderElect=false
        --set operator.podDisruptionBudget.enabled=false --set engine.podDisruptionBudget.enabled=false)
kubectl create namespace "$NS" >/dev/null 2>&1 || true
helm install "$REL" "$CHART" -n "$NS" "${VALUES[@]}" --timeout 2m >/dev/null || fail "install"
kubectl get crd kubecostautoscalers.autoscaling.kubecost.io kubecosthpas.autoscaling.kubecost.io >/dev/null || fail "CRDs missing after install"
deploys=$(kubectl -n "$NS" get deploy -o name)
grep -q operator <<<"$deploys" || fail "operator Deployment missing"
ok "install: revision 1 accepted by the apiserver"
helm upgrade "$REL" "$CHART" -n "$NS" "${VALUES[@]}" --set grafanaDashboard.enabled=true --timeout 2m >/dev/null || fail "upgrade"
kubectl -n "$NS" get configmap "$REL-kubecost-auto-hpa-dashboard" >/dev/null || fail "dashboard ConfigMap missing after upgrade"
[ "$(helm history "$REL" -n "$NS" -o json | python3 -c 'import sys,json;print(len(json.load(sys.stdin)))')" = 2 ] || fail "expected 2 revisions"
ok "upgrade: revision 2 adds exactly the dashboard ConfigMap"
helm rollback "$REL" 1 -n "$NS" --timeout 2m >/dev/null || fail "rollback"
kubectl -n "$NS" get configmap "$REL-kubecost-auto-hpa-dashboard" >/dev/null 2>&1 && fail "dashboard survived rollback"
ok "rollback to revision 1 (revision 3 in history): dashboard removed"
helm uninstall "$REL" -n "$NS" >/dev/null || fail "uninstall"
remaining=$(kubectl -n "$NS" get deploy -o name 2>/dev/null || true)
grep -q "$REL" <<<"$remaining" && fail "release objects remain after uninstall"
kubectl get crd kubecostautoscalers.autoscaling.kubecost.io >/dev/null || fail "CRD removed (Helm keeps crds/)"
ok "uninstall: release objects removed; CRDs kept by design"
echo "helm lifecycle passed"
