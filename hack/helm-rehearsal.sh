#!/usr/bin/env bash
# Install -> upgrade -> rollback -> uninstall of the Helm chart on a throwaway kind cluster, with locally
# built images. Publishes nothing and touches no real cluster. Needs: docker, kind, kubectl, helm.
# STATUS: NOT YET RUN WITH PODS. Validated only on a host without a kubelet (see docs/PILOT.md section 12).
# Pipes into grep -q were replaced by captured variables: with `set -o pipefail` a grep that exits early
# makes the writer fail (SIGPIPE, exit 141) even when the text matched.
set -euo pipefail
cd "$(dirname "$0")/.."
for tool in docker kind kubectl helm; do
  command -v "$tool" >/dev/null || { echo "missing required tool: $tool (see docs/PILOT.md section 12)" >&2; exit 2; }
done
CLUSTER=${CLUSTER:-kch-helm-rehearsal}
NODE_IMAGE=${NODE_IMAGE:-kindest/node:v1.33.1}
REL=rehearsal
NS=kubecost-auto-hpa-system
KEEP=${KEEP:-0}
cleanup() { [ "$KEEP" = 1 ] || kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true; }
trap 'rc=$?; [ $rc -eq 0 ] || { kubectl -n "$NS" get pods,events 2>/dev/null | tail -40; }; cleanup' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }
ok()   { echo "ok:   $*"; }

kind create cluster --name "$CLUSTER" --image "$NODE_IMAGE" --wait 120s
docker build -t kubecost-auto-hpa-operator:rehearsal --build-arg VERSION=rehearsal --build-arg COMMIT="$(git rev-parse HEAD)" operator
docker build -t kubecost-auto-hpa-engine:rehearsal engine
kind load docker-image --name "$CLUSTER" kubecost-auto-hpa-operator:rehearsal kubecost-auto-hpa-engine:rehearsal

VALUES=(--set operator.image.repository=kubecost-auto-hpa-operator --set operator.image.tag=rehearsal
        --set engine.image.repository=kubecost-auto-hpa-engine --set engine.image.tag=rehearsal
        --set operator.replicas=1 --set engine.replicas=1 --set operator.leaderElect=false
        --set operator.podDisruptionBudget.enabled=false --set engine.podDisruptionBudget.enabled=false)

# 1. install
helm install "$REL" deploy/helm/kubecost-auto-hpa -n "$NS" --create-namespace "${VALUES[@]}" --wait --timeout 4m
kubectl get crd kubecostautoscalers.autoscaling.kubecost.io kubecosthpas.autoscaling.kubecost.io >/dev/null
ok "installed (revision 1): operator, engine and Redis are ready and both CRDs exist"
logs=$(kubectl -n "$NS" logs deploy/"$REL"-kubecost-auto-hpa-operator --tail=50)
grep -q "starting manager" <<<"$logs" || fail "operator did not start"
ok "operator logged 'starting manager' with its version"

# 2. upgrade: switch an optional feature on and check that exactly it appears
helm upgrade "$REL" deploy/helm/kubecost-auto-hpa -n "$NS" "${VALUES[@]}" --set grafanaDashboard.enabled=true --wait --timeout 4m
kubectl -n "$NS" get configmap "$REL-kubecost-auto-hpa-dashboard" >/dev/null || fail "dashboard ConfigMap missing after upgrade"
[ "$(helm history "$REL" -n "$NS" -o json | python3 -c 'import sys,json;print(len(json.load(sys.stdin)))')" = 2 ] || fail "expected 2 revisions"
ok "upgraded (revision 2): dashboard ConfigMap present"

# 3. rollback
helm rollback "$REL" 1 -n "$NS" --wait --timeout 4m
kubectl -n "$NS" get configmap "$REL-kubecost-auto-hpa-dashboard" >/dev/null 2>&1 && fail "dashboard ConfigMap survived the rollback"
kubectl -n "$NS" rollout status deploy/"$REL"-kubecost-auto-hpa-operator --timeout=120s
ok "rolled back to revision 1 (now revision 3): dashboard gone, operator healthy"

# 4. uninstall: release resources go; CRDs installed from crds/ are intentionally kept by Helm
helm uninstall "$REL" -n "$NS" --wait
remaining=$(kubectl -n "$NS" get deploy 2>/dev/null || true)
grep -q "$REL" <<<"$remaining" && fail "release resources remain after uninstall"
kubectl get crd kubecostautoscalers.autoscaling.kubecost.io >/dev/null || fail "CRD unexpectedly removed (Helm keeps crds/)"
ok "uninstalled: workloads removed, CRDs kept (delete them by hand if you also want the custom resources gone)"
echo "helm rehearsal passed"
