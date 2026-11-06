#!/usr/bin/env bash
# End-to-end test on a real cluster: kind (Kubernetes >= 1.33) + the real operator,
# engine and Redis images. Prometheus is replaced by a deterministic stub.
# Requires: docker, kind, kubectl. Run from the repo root.
set -euo pipefail

CLUSTER=${CLUSTER:-kch-e2e}
NODE_IMAGE=${NODE_IMAGE:-kindest/node:v1.33.4}
KEEP=${KEEP:-0}

cleanup() { [ "$KEEP" = 1 ] || kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true; }
dump() {
  echo "--- diagnostics ---"
  kubectl get nodes,deploy,pods -A -o wide || true
  kubectl -n kubecost-auto-hpa-system describe pod -l app.kubernetes.io/component=operator || true
  kubectl -n kubecost-auto-hpa-system logs deploy/kubecost-auto-hpa-operator --tail=80 || true
  kubectl -n kubecost-auto-hpa-system logs deploy/kubecost-auto-hpa-engine --tail=80 || true
}
trap 'rc=$?; [ $rc -eq 0 ] || dump; cleanup' EXIT

kind create cluster --name "$CLUSTER" --image "$NODE_IMAGE" --wait 120s
docker build -t kubecost-auto-hpa-operator:dev operator
docker build -t kubecost-auto-hpa-engine:dev engine
kind load docker-image --name "$CLUSTER" kubecost-auto-hpa-operator:dev kubecost-auto-hpa-engine:dev

# Establish CRDs first so the overlay applies cleanly on the first pass.
kubectl apply -k operator/config/crd
kubectl wait --for=condition=Established --timeout=60s \
  crd/kubecosthpas.autoscaling.kubecost.io \
  crd/kubecostautoscalers.autoscaling.kubecost.io
kubectl apply -k operator/config/overlays/e2e || { sleep 5; kubectl apply -k operator/config/overlays/e2e; }
for d in kubecost-auto-hpa-operator kubecost-auto-hpa-engine kubecost-auto-hpa-redis fake-prometheus; do
  kubectl -n kubecost-auto-hpa-system rollout status "deploy/$d" --timeout=180s
done
kubectl -n e2e rollout status deploy/web --timeout=180s

uids_before=$(kubectl -n e2e get pods -l app=web -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' | sort)

echo "waiting for in-place shrink (500m -> below 500m) ..."
for i in $(seq 1 60); do
  cpu=$(kubectl -n e2e get pods -l app=web -o jsonpath='{.items[0].spec.containers[0].resources.requests.cpu}')
  case "$cpu" in *m) [ "${cpu%m}" -lt 500 ] && break ;; esac
  sleep 5
done
echo "pod cpu request is now: $cpu"
case "$cpu" in *m) [ "${cpu%m}" -lt 500 ] || { echo "FAIL: not resized"; exit 1; } ;; *) echo "FAIL: not resized"; exit 1 ;; esac

uids_after=$(kubectl -n e2e get pods -l app=web -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' | sort)
[ "$uids_before" = "$uids_after" ] || { echo "FAIL: pods were recreated"; exit 1; }
restarts=$(kubectl -n e2e get pods -l app=web -o jsonpath='{range .items[*]}{.status.containerStatuses[0].restartCount}{"\n"}{end}' | sort -u)
[ "$restarts" = "0" ] || { echo "FAIL: containers restarted ($restarts)"; exit 1; }

kubectl -n e2e get kubecosthpa web -o wide
echo "PASS: in-place resize without restart on $NODE_IMAGE"
