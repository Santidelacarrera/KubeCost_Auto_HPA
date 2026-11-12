#!/usr/bin/env bash
# Installs a REAL OpenCost into the lab (default pricing: the response format is real, the prices are not a bill) and makes Prometheus scrape it.
# Then: kubectl --context kubecost-lab -n opencost port-forward svc/opencost 9091:9090   and point --kubecost-url / KUBECOST_LIVE_URL at http://localhost:9091
# (the UI service proxies /model/..., which is the path the KubeCost client uses; the API port 9003 only serves /allocation/compute).
set -eu
CTX="${CTX:-kubecost-lab}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"
[ "$CTX" = "kubecost-lab" ] || [ "${ALLOW_CTX:-}" = "1" ] || { echo "refusing context '$CTX'"; exit 2; }
helm repo add opencost https://opencost.github.io/opencost-helm-chart >/dev/null 2>&1 || true
helm repo update >/dev/null
kubectl --context "$CTX" create ns opencost --dry-run=client -o yaml | kubectl --context "$CTX" apply -f -
helm --kube-context "$CTX" upgrade --install opencost opencost/opencost -n opencost \
  --set opencost.prometheus.internal.serviceName=prom-prometheus-server --set opencost.prometheus.internal.namespaceName=monitoring \
  --set opencost.prometheus.internal.port=80 --set opencost.exporter.defaultClusterId="$CTX" --set opencost.ui.enabled=true --wait --timeout 6m
helm --kube-context "$CTX" upgrade prom prometheus-community/prometheus -n monitoring --reuse-values -f "$HERE/hack/lab/prom-opencost.yaml" --wait --timeout 6m
echo "Give OpenCost ~10 minutes before the allocation covers a 15m window."
