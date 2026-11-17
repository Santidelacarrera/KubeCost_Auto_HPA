#!/usr/bin/env bash
# Real-cluster check of the Helm chart on the disposable minikube lab: install, CRDs, observe-only behaviour,
# switching observe-only off, upgrade, rollback, uninstall.
#
# REAL here: kube-apiserver, metrics-server, the chart, the operator and engine images built from this checkout, Redis, the CRDs.
# MOCK here: KubeCost (hack/lab/mock-kubecost.py returns an invented constant cost). Nothing below validates the KubeCost contract.
# Needs: docker, minikube profile kubecost-lab, kubectl, helm. Refuses any other context unless ALLOW_CTX=1.
set -u
CTX="${CTX:-kubecost-lab}"
PROFILE="${PROFILE:-kubecost-lab}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"
REL=lab
NS=kubecost-auto-hpa-system
PASS=0; FAILN=0
ok()  { echo "PASS  $*"; PASS=$((PASS+1)); }
bad() { echo "FAIL  $*"; FAILN=$((FAILN+1)); }
check() { if eval "$2"; then ok "$1"; else bad "$1"; fi; }
[ "$CTX" = "kubecost-lab" ] || [ "${ALLOW_CTX:-}" = "1" ] || { echo "refusing context '$CTX'"; exit 2; }
K="kubectl --context $CTX"; H="helm --kube-context $CTX"
$K get ns >/dev/null 2>&1 || { echo "cluster not reachable"; exit 2; }

echo "== images"
docker build -q -t kubecost-auto-hpa-operator:lab --build-arg VERSION=lab --build-arg COMMIT="$(git -C "$HERE" rev-parse HEAD)" "$HERE/operator" >/dev/null || exit 2
docker build -q -t kubecost-auto-hpa-engine:lab "$HERE/engine" >/dev/null || exit 2
minikube -p "$PROFILE" image load kubecost-auto-hpa-operator:lab kubecost-auto-hpa-engine:lab || exit 2

echo "== mock KubeCost (NOT KubeCost) + workload"
$K create ns kubecost >/dev/null 2>&1
$K -n kubecost create configmap mock-kubecost --from-file=mock.py="$HERE/hack/lab/mock-kubecost.py" --dry-run=client -o yaml | $K apply -f - >/dev/null
cat <<'YAML' | $K apply -f - >/dev/null
apiVersion: apps/v1
kind: Deployment
metadata: {name: kubecost-cost-analyzer, namespace: kubecost}
spec:
  replicas: 1
  selector: {matchLabels: {app: mock-kubecost}}
  template:
    metadata: {labels: {app: mock-kubecost}}
    spec:
      containers:
      - name: mock
        image: python:3.12-slim
        command: ["python", "/mock/mock.py"]
        env: [{name: MOCK_NAMESPACE, value: lab}, {name: MOCK_CONTROLLER, value: scaletest}]
        volumeMounts: [{name: code, mountPath: /mock}]
        resources: {requests: {cpu: 10m, memory: 32Mi}}
      volumes: [{name: code, configMap: {name: mock-kubecost}}]
---
apiVersion: v1
kind: Service
metadata: {name: kubecost-cost-analyzer, namespace: kubecost}
spec: {selector: {app: mock-kubecost}, ports: [{port: 9090}]}
YAML
$K -n kubecost rollout status deploy/kubecost-cost-analyzer --timeout=180s >/dev/null || { echo "mock kubecost not ready"; exit 2; }
$K -n lab delete kubecostautoscaler scaletest >/dev/null 2>&1
$K get ns lab >/dev/null 2>&1 || $K create ns lab >/dev/null
$K -n lab delete deploy scaletest >/dev/null 2>&1
$K -n lab apply -f "$HERE/hack/lab/scaletest.yaml" >/dev/null 2>&1   # the CR is applied again after the CRDs exist

VALUES=(--set operator.image.repository=kubecost-auto-hpa-operator --set operator.image.tag=lab --set operator.image.pullPolicy=Never
        --set engine.image.repository=kubecost-auto-hpa-engine --set engine.image.tag=lab --set engine.image.pullPolicy=Never
        --set operator.replicas=1 --set engine.replicas=1 --set operator.leaderElect=false
        --set operator.podDisruptionBudget.enabled=false --set engine.podDisruptionBudget.enabled=false
        --set prometheus.url=http://prom-prometheus-server.monitoring:80)

echo "== 1. install (chart default: observe-only)"
$H install "$REL" "$HERE/deploy/helm/kubecost-auto-hpa" -n "$NS" --create-namespace "${VALUES[@]}" --wait --timeout 4m >/dev/null 2>&1
check "release deployed"                         "[ \"\$($H status $REL -n $NS -o json | python -c 'import json,sys; print(json.load(sys.stdin)[\"info\"][\"status\"])')\" = deployed ]"
check "both CRDs exist"                          "$K get crd kubecostautoscalers.autoscaling.kubecost.io kubecosthpas.autoscaling.kubecost.io >/dev/null 2>&1"
check "operator, engine, redis are ready"        "[ \"\$($K -n $NS get pods --no-headers | grep -vc ' 1/1 .*Running')\" = 0 ]"
OP="deploy/$REL-kubecost-auto-hpa-operator"
LOGS="$($K -n $NS logs $OP --tail=80 2>&1)"
check "operator started"                         "echo \"\$LOGS\" | grep -q 'starting manager'"
check "operator announces observe-only"          "echo \"\$LOGS\" | grep -q 'observe-only mode'"
check "operator arg is --observe-only=true"      "$K -n $NS get $OP -o jsonpath='{.spec.template.spec.containers[0].args}' | grep -q 'observe-only=true'"
check "Spot drain controller not started"        "! echo \"\$LOGS\" | grep -qi 'spot node controller'"

echo "== 2. observe-only: decisions are reported, replicas stay"
$K -n lab apply -f "$HERE/hack/lab/scaletest.yaml" >/dev/null 2>&1
$K -n lab rollout status deploy/scaletest --timeout=120s >/dev/null
rep() { $K -n lab get deploy scaletest -o jsonpath='{.spec.replicas}'; }
sc() { $K -n lab get kubecostautoscaler scaletest -o jsonpath="$1" 2>/dev/null; }
for _ in $(seq 1 30); do [ "$(sc '{.status.lastDecision.type}')" = ScaleDown ] && break; sleep 10; done
echo "decision message: $(sc '{.status.lastDecision.message}')"
echo "replicas=$(rep) reason=$(sc '{.status.conditions[?(@.type=="ScalingActive")].reason}') decision=$(sc '{.status.lastDecision.type}') dryRun=$(sc '{.status.lastDecision.dryRun}')"
check "replicas unchanged (4)"                   "[ \"\$(rep)\" = 4 ]"
check "ScalingActive reason is ObserveOnly"      "[ \"\$(sc '{.status.conditions[?(@.type==\"ScalingActive\")].reason}')\" = ObserveOnly ]"
check "a scale-down decision was reported"       "[ \"\$(sc '{.status.lastDecision.type}')\" = ScaleDown ]"
check "the decision is marked dry-run"           "[ \"\$(sc '{.status.lastDecision.dryRun}')\" = true ]"

echo "== 3. observe-only off (helm upgrade): the same resource now acts"
$H upgrade "$REL" "$HERE/deploy/helm/kubecost-auto-hpa" -n "$NS" "${VALUES[@]}" --set operator.observeOnly=false --wait --timeout 4m >/dev/null 2>&1
for _ in $(seq 1 24); do [ "$(rep)" -lt 4 ] && break; sleep 10; done
echo "replicas=$(rep) reason=$(sc '{.status.conditions[?(@.type=="ScalingActive")].reason}')"
check "replicas were reduced by the operator"    "[ \"\$(rep)\" -lt 4 ]"
check "scaling is active (single writer)"        "[ \"\$(sc '{.status.conditions[?(@.type==\"ScalingActive\")].status}')\" = True ]"

echo "== 4. rollback of the release, then uninstall"
$H rollback "$REL" 1 -n "$NS" --wait --timeout 4m >/dev/null 2>&1
check "rolled back to revision 1 (observe-only again)" "$K -n $NS get $OP -o jsonpath='{.spec.template.spec.containers[0].args}' | grep -q 'observe-only=true'"
$H uninstall "$REL" -n "$NS" --wait >/dev/null 2>&1
check "workloads removed by uninstall"           "[ \"\$($K -n $NS get deploy --no-headers 2>/dev/null | wc -l)\" = 0 ]"
check "CRDs kept (Helm keeps crds/)"             "$K get crd kubecostautoscalers.autoscaling.kubecost.io >/dev/null 2>&1"

echo "== cleanup (lab objects only)"
$K -n lab delete kubecostautoscaler scaletest --ignore-not-found >/dev/null 2>&1
$K -n lab delete deploy scaletest --ignore-not-found >/dev/null 2>&1
$K delete ns kubecost "$NS" --ignore-not-found >/dev/null 2>&1
echo; echo "RESULT: $PASS passed, $FAILN failed"
[ "$FAILN" -eq 0 ]
