#!/usr/bin/env bash
# The managed-cluster run of hack/gke-validate.sh on a REAL AKS cluster, created here and deleted at the end.
#   bash hack/aks-validate.sh            (needs `az login` with a subscription; uses the free AKS tier)
# Node: Standard_D2as_v4 (2 vCPU, 8 GiB), Linux list price 0.096 USD/h in eastus from the Azure Retail Prices API (2026-11-10).
# (A new free-account subscription refused Standard_B2s: "not allowed in your subscription"; at most 2 nodes keeps it inside the
# free account's small vCPU quota.)
set -u
AZ="${AZ:-az}"; RG="${RG:-kubecost-validate-rg}"; NAME="${NAME:-kubecost-validate}"; LOC="${LOC:-eastus}"
SIZE="${SIZE:-Standard_D2as_v4}"; PRICE="${NODE_HOURLY_USD:-0.096}"; MAXN="${MAXN:-2}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"
"$AZ" account show >/dev/null 2>&1 || { echo "run az login first (an account with a subscription)"; exit 2; }
for ns in Microsoft.ContainerService Microsoft.Compute Microsoft.Network Microsoft.Storage; do "$AZ" provider register --namespace $ns --wait >/dev/null; done
"$AZ" group create -n "$RG" -l "$LOC" -o none || exit 1
cleanup() { [ "${KEEP:-}" = 1 ] || "$AZ" group delete -n "$RG" --yes -o none; }; trap cleanup EXIT
# CA_PROFILE: AKS cluster-autoscaler profile. AKS defaults to skip-nodes-with-system-pods=true, so a node holding any kube-system
# pod that is not a DaemonSet (AKS places konnectivity-agent replicas on any node) is never removed. CA_PROFILE="" keeps the default.
CA_PROFILE="${CA_PROFILE-skip-nodes-with-system-pods=false}"
"$AZ" aks create -g "$RG" -n "$NAME" --tier free --node-count 1 --node-vm-size "$SIZE" --enable-cluster-autoscaler \
  --min-count 1 --max-count "$MAXN" --node-osdisk-size 30 --generate-ssh-keys ${CA_PROFILE:+--cluster-autoscaler-profile $CA_PROFILE} -o none || { echo "aks create failed"; exit 1; }
echo "== cluster-autoscaler profile: ${CA_PROFILE:-AKS default}"
"$AZ" aks get-credentials -g "$RG" -n "$NAME" --overwrite-existing -o none
CTX="$NAME"
echo "== AKS $(kubectl --context "$CTX" version -o json | python -c "import json,sys; print(json.load(sys.stdin)['serverVersion']['gitVersion'])")"
# REPLICAS x 500m on 2-vCPU nodes (about 1.9 allocatable CPU each) forces a second node; after the change the pods should fit on
# the first one. With 4 replicas they did not (result 14: the first node also holds AKS system pods and Prometheus); 2 is the default now.
CTX="$CTX" NODE_HOURLY_USD="$PRICE" REPLICAS="${REPLICAS:-2}" LABDIR=aks PROM_PORT=9092 bash "$HERE/hack/gke-validate.sh"; RC=$?
exit $RC
