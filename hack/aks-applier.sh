#!/usr/bin/env bash
# The opt-in applier (hack/lab-applier.sh, 8 scenarios) and the Helm lifecycle (hack/helm-lifecycle.sh) on a REAL AKS cluster,
# created here with its own container registry and deleted at the end (the whole resource group).
#   bash hack/aks-applier.sh            (needs `az login` with a subscription and a running Docker)
# One Standard_D4as_v4 node (4 vCPU, 16 GiB): the free account's regional quota is 4 vCPU, and the lab workload asks 2 x 1 CPU.
# Its own kubeconfig file (.lab/aks-applier/kubeconfig): the operator's ~/.kube/config is not touched.
set -u
AZ="${AZ:-az}"; RG="${RG:-kubecost-applier-rg}"; NAME="${NAME:-kubecost-applier}"; LOC="${LOC:-eastus}"; SIZE="${SIZE:-Standard_D4as_v4}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"; OUT="$HERE/.lab/aks-applier"; mkdir -p "$OUT"
export KUBECONFIG="$(cygpath -w "$OUT/kubeconfig" 2>/dev/null || echo "$OUT/kubeconfig")"; PP="${PROM_PORT:-9093}"   # KUBECONFIG as a Windows path: kubectl.exe gets it even under MSYS_NO_PATHCONV=1
"$AZ" account show >/dev/null 2>&1 || { echo "run az login first"; exit 2; }
docker info >/dev/null 2>&1 || { echo "Docker is not running"; exit 2; }
for ns in Microsoft.ContainerService Microsoft.ContainerRegistry Microsoft.Compute Microsoft.Network Microsoft.Storage; do
  "$AZ" provider register --namespace $ns --wait >/dev/null; done
"$AZ" group create -n "$RG" -l "$LOC" -o none || exit 1
cleanup() { pkill -f "port-forward svc/prom-prometheus-server $PP" 2>/dev/null; [ "${KEEP:-}" = 1 ] || "$AZ" group delete -n "$RG" --yes -o none; }
trap cleanup EXIT
ACR="kubecostlab$(date +%s | tail -c 7)"
"$AZ" acr create -g "$RG" -n "$ACR" --sku Basic -o none || { echo "acr create failed"; exit 1; }
"$AZ" aks create -g "$RG" -n "$NAME" --tier free --node-count 1 --node-vm-size "$SIZE" --node-osdisk-size 30 \
  --attach-acr "$ACR" --generate-ssh-keys -o none || { echo "aks create failed"; exit 1; }
"$AZ" aks get-credentials -g "$RG" -n "$NAME" --file "$KUBECONFIG" --overwrite-existing -o none
"$AZ" acr login -n "$ACR" >/dev/null || exit 1
K="kubectl --context $NAME"
echo "== AKS $($K version -o json | python -c "import json,sys; print(json.load(sys.stdin)['serverVersion']['gitVersion'])"), 1 x $SIZE"

echo "== Helm lifecycle (install, upgrade, rollback, uninstall; apiserver acceptance)"
bash "$HERE/hack/helm-lifecycle.sh" 2>&1 | tee "$OUT/helm-lifecycle.log"; HL=${PIPESTATUS[0]}

echo "== Prometheus, the lab workload and its history"
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts >/dev/null 2>&1 || true
helm repo update prometheus-community >/dev/null 2>&1
helm --kube-context "$NAME" upgrade --install prom prometheus-community/prometheus -n monitoring --create-namespace \
  --set alertmanager.enabled=false --set prometheus-pushgateway.enabled=false --set prometheus-node-exporter.enabled=false \
  --set server.persistentVolume.enabled=false --set server.global.scrape_interval=15s --wait --timeout 10m >/dev/null 2>&1 || { echo "prometheus failed"; exit 1; }
$K create ns lab >/dev/null 2>&1
sed -e 's/cpu: "1", memory: 1Gi}/cpu: 500m, memory: 256Mi}/' -e 's/cpu: "2", memory: 1Gi}/cpu: "1", memory: 256Mi}/' "$HERE/hack/lab/api.yaml" | $K apply -f - >/dev/null   # the same as LAB_SMALL=1 below
($K -n monitoring port-forward svc/prom-prometheus-server $PP:80 >/dev/null 2>&1 &); sleep 5
echo "waiting 15 min of history for the lab workload"; sleep 900
curl -fs "http://localhost:$PP/-/ready" >/dev/null || { ($K -n monitoring port-forward svc/prom-prometheus-server $PP:80 >/dev/null 2>&1 &); sleep 5; }

echo "== applier scenarios (hack/lab-applier.sh)"
CTX="$NAME" ALLOW_CTX=1 LAB_SMALL=1 PF=1 PROM="http://localhost:$PP" REGISTRY="$ACR.azurecr.io" bash "$HERE/hack/lab-applier.sh" 2>&1 | tee "$OUT/applier.log"
AP=${PIPESTATUS[0]}
echo; echo "helm lifecycle exit $HL, applier exit $AP"
[ "$HL" -eq 0 ] && [ "$AP" -eq 0 ]
