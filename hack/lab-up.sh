#!/usr/bin/env bash
# Creates the disposable lab: a dedicated minikube profile (it never touches other profiles), metrics-server and
# Prometheus (with kube-state-metrics) in namespace "monitoring". Then run hack/lab-e2e.sh and hack/lab-helm.sh.
# Needs: docker (running), minikube, kubectl, helm, ~5 GB of memory for Docker. Verified versions: docs/COMPATIBILITY.md.
set -eu
PROFILE="${PROFILE:-kubecost-lab}"
K8S="${K8S:-v1.33.4}"
MEM="${MEM:-5000}"
docker info >/dev/null 2>&1 || { echo "the Docker daemon is not running"; exit 2; }
minikube start -p "$PROFILE" --driver=docker --kubernetes-version="$K8S" --cpus=4 --memory="$MEM" --wait=all
minikube -p "$PROFILE" addons enable metrics-server
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts >/dev/null 2>&1 || true
helm repo update >/dev/null
kubectl --context "$PROFILE" create ns monitoring --dry-run=client -o yaml | kubectl --context "$PROFILE" apply -f -
helm --kube-context "$PROFILE" upgrade --install prom prometheus-community/prometheus -n monitoring \
  --set alertmanager.enabled=false --set prometheus-pushgateway.enabled=false --set prometheus-node-exporter.enabled=false \
  --set server.persistentVolume.enabled=false --set server.global.scrape_interval=15s --set server.resources.requests.memory=256Mi \
  --wait --timeout 8m
cat <<EOF

Lab ready (context: $PROFILE). Expose Prometheus for the tools:
  kubectl --context $PROFILE -n monitoring port-forward svc/prom-prometheus-server 9090:80
Let the workloads run ~10 minutes before the recommender has enough history (the lab uses relaxed minimums: see docs/LAB.md).
  bash hack/lab-e2e.sh      # recommend -> policy -> approve -> apply -> observe -> rollback
  bash hack/lab-helm.sh     # chart install / observe-only / upgrade / rollback / uninstall
Tear down:  bash hack/lab-down.sh
EOF
