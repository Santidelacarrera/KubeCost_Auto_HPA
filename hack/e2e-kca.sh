#!/usr/bin/env bash
# End-to-end test of the KubeCostAutoscaler on a real cluster: kind + real metrics-server +
# the real operator. KubeCost is replaced by a deterministic stub. The CPU-bound workload
# runs far above its utilisation target, so the controller must scale it out through the
# /scale subresource and publish a decision. Requires: docker, kind, kubectl. Run from the repo root.
set -euo pipefail

CLUSTER=${CLUSTER:-kca-e2e}
NODE_IMAGE=${NODE_IMAGE:-kindest/node:v1.33.1}
METRICS_SERVER=${METRICS_SERVER:-https://github.com/kubernetes-sigs/metrics-server/releases/download/v0.7.2/components.yaml}
KEEP=${KEEP:-0}

cleanup() { [ "$KEEP" = 1 ] || kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true; }
dump() {
  echo "--- diagnostics ---"
  kubectl -n kca get deploy,pods,kubecostautoscaler -o wide || true
  kubectl -n kca get kubecostautoscaler load -o yaml || true
  kubectl -n kubecost-auto-hpa-system logs deploy/kubecost-auto-hpa-operator --tail=80 || true
  kubectl top pods -n kca || true
}
trap 'rc=$?; [ $rc -eq 0 ] || dump; cleanup' EXIT

kind create cluster --name "$CLUSTER" --image "$NODE_IMAGE" --wait 120s
docker build -t kubecost-auto-hpa-operator:dev operator
docker build -t kubecost-auto-hpa-engine:dev engine
kind load docker-image --name "$CLUSTER" kubecost-auto-hpa-operator:dev kubecost-auto-hpa-engine:dev

# metrics-server needs --kubelet-insecure-tls: kind kubelets use self-signed certificates.
kubectl apply -f "$METRICS_SERVER"
kubectl -n kube-system patch deploy metrics-server --type=json \
  -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]'
kubectl -n kube-system rollout status deploy/metrics-server --timeout=180s

kubectl apply -k operator/config/overlays/e2e-kca || { sleep 5; kubectl apply -k operator/config/overlays/e2e-kca; }
for d in kubecost-auto-hpa-operator kubecost-auto-hpa-engine kubecost-auto-hpa-redis fake-kubecost; do
  kubectl -n kubecost-auto-hpa-system rollout status "deploy/$d" --timeout=180s
done
kubectl -n kca rollout status deploy/load --timeout=180s

echo "waiting for scale-out (1 -> >1 replicas) ..."
replicas=1
for i in $(seq 1 60); do
  replicas=$(kubectl -n kca get deploy load -o jsonpath='{.spec.replicas}')
  [ "$replicas" -gt 1 ] && break
  sleep 5
done
echo "deployment replicas: $replicas"
[ "$replicas" -gt 1 ] || { echo "FAIL: not scaled out"; exit 1; }
[ "$replicas" -le 4 ] || { echo "FAIL: exceeded maxReplicas"; exit 1; }

owner=$(kubectl -n kca get kubecostautoscaler load -o jsonpath='{.status.lastDecision}')
[ -n "$owner" ] || { echo "FAIL: no decision published in status"; exit 1; }
kubectl -n kca get kubecostautoscaler load -o wide
echo "PASS: KubeCostAutoscaler scaled out through /scale on $NODE_IMAGE"
