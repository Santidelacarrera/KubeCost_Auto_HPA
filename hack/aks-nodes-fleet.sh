#!/usr/bin/env bash
# The thirteenth-pass read-only features on a REAL AKS cluster, created here and deleted at the end (free tier, two small nodes,
# about 20 minutes):
#   1. `rightsize nodes --plan`: static pods, taints, zones, and a pod with a real zonal Azure disk (is the CSI zone key read?)
#   2. `rightsize fleet` over this cluster and any others named in FLEET_CONTEXTS
#   bash hack/aks-nodes-fleet.sh        (needs `az login`; KEEP=1 keeps the cluster)
set -u
AZ="${AZ:-az}"; RG="${RG:-kubecost-p13-rg}"; NAME="${NAME:-kubecost-p13}"; LOC="${LOC:-eastus}"; ZONES="${ZONES:-}"
SIZE="${SIZE:-Standard_D2as_v4}"
# ZONES="1 2" where the subscription has zones (a free one had none in eastus2)
HERE="$(cd "$(dirname "$0")/.." && pwd)"; LAB="$HERE/.lab/aks-p13"; mkdir -p "$LAB"; RS="$LAB/rightsize.exe"
PASS=0; FAILN=0
ok()  { echo "PASS  $*"; PASS=$((PASS+1)); }
bad() { echo "FAIL  $*"; FAILN=$((FAILN+1)); }
"$AZ" account show >/dev/null 2>&1 || { echo "run az login first"; exit 2; }
(cd "$HERE/operator" && go build -o "$RS" ./cmd/rightsize) || exit 2
"$AZ" group create -n "$RG" -l "$LOC" -o none || exit 1
cleanup() { [ "${KEEP:-}" = 1 ] || "$AZ" group delete -n "$RG" --yes --no-wait -o none; }; trap cleanup EXIT
"$AZ" aks create -g "$RG" -n "$NAME" --tier free --node-count 2 ${ZONES:+--zones $ZONES} --node-vm-size "$SIZE" --node-osdisk-size 30 \
  --generate-ssh-keys -o none || { echo "aks create failed"; exit 1; }
"$AZ" aks get-credentials -g "$RG" -n "$NAME" --overwrite-existing -o none
CTX="$NAME"; K="kubectl --context $CTX"
echo "== cluster: $($K version -o json 2>/dev/null | python -c "import json,sys; print(json.load(sys.stdin)['serverVersion']['gitVersion'])")"
$K get nodes -L topology.kubernetes.io/zone --no-headers

# a workload with a real zonal managed disk
$K create ns p13 >/dev/null
cat <<EOF | $K apply -f - >/dev/null
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: data, namespace: p13}
spec: {accessModes: [ReadWriteOnce], storageClassName: managed-csi, resources: {requests: {storage: 1Gi}}}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: withdisk, namespace: p13}
spec:
  replicas: 1
  selector: {matchLabels: {app: withdisk}}
  template:
    metadata: {labels: {app: withdisk}}
    spec:
      containers:
      - name: c
        image: busybox:1.36
        command: [sh, -c, "sleep 86400"]
        resources: {requests: {cpu: 50m, memory: 32Mi}}
        volumeMounts: [{name: d, mountPath: /data}]
      volumes: [{name: d, persistentVolumeClaim: {claimName: data}}]
EOF
$K -n p13 rollout status deploy/withdisk --timeout=300s >/dev/null && ok "a pod with a managed disk is running" || bad "the pod with a disk did not start"
echo "== the volume's node affinity, as AKS wrote it:"
$K get pv -o jsonpath='{range .items[*]}{.spec.nodeAffinity.required.nodeSelectorTerms[*].matchExpressions[*].key}{" = "}{.spec.nodeAffinity.required.nodeSelectorTerms[*].matchExpressions[*].values}{"\n"}{end}'

"$RS" nodes --context "$CTX" --plan --node-hourly-usd 0.096 > "$LAB/nodes-plan.json" 2> "$LAB/nodes.err" && ok "rightsize nodes --plan ran" || { bad "rightsize nodes failed: $(head -c 300 "$LAB/nodes.err")"; }
python - "$LAB/nodes-plan.json" <<'PYEOF'
import json, sys
r = json.load(open(sys.argv[1])); a = r["Analysis"]
for n in a["Nodes"]:
    print("   node %s cpu=%.0f%% mem=%.0f%% pods=%d removable=%s | %s" % (n["Name"][-12:], n["CPUPct"] * 100, n["MemPct"] * 100, n["Pods"], n["Removable"], n["Why"][:170]))
print("   plan steps:", [s["Node"][-12:] for s in (r["Plan"]["Steps"] or [])], "blocked:", r["Plan"]["Blocked"])
why = " ".join(n["Why"] for n in a["Nodes"])
print("CHECK static-or-daemonset pods do not block:", "kube-proxy" not in why and "azure-ip-masq" not in why and "csi-azure" not in why)
print("CHECK the disk pod is not reported as 'without a known zone':", "p13/withdisk" not in why or "without a known zone" not in why)
print("CHECK every blocked node has a reason:", all(n["Removable"] or n["Why"] for n in a["Nodes"]))
PYEOF
grep -q "CHECK static-or-daemonset pods do not block: True" <(python - "$LAB/nodes-plan.json" <<'PYEOF'
import json, sys
a = json.load(open(sys.argv[1]))["Analysis"]; why = " ".join(n["Why"] for n in a["Nodes"])
print("CHECK static-or-daemonset pods do not block:", "kube-proxy" not in why and "azure-ip-masq" not in why and "csi-azure" not in why)
PYEOF
) && ok "system DaemonSet/static pods do not block a node" || bad "a system pod blocked a node"
python -c "
import json,sys
a=json.load(open(sys.argv[1]))['Analysis']; why=' '.join(n['Why'] for n in a['Nodes'])
sys.exit(1 if ('p13/withdisk' in why and 'without a known zone' in why) else 0)" "$LAB/nodes-plan.json" && ok "the pod with an Azure disk is not reported as immovable" || bad "the pod with an Azure disk is reported as immovable"

"$RS" fleet --contexts "$CTX${FLEET_CONTEXTS:+,$FLEET_CONTEXTS}" > "$LAB/fleet.json" 2> "$LAB/fleet.err" && ok "rightsize fleet ran" || bad "rightsize fleet failed"
python -c "
import json,sys
r=json.load(open(sys.argv[1]))
for c in r['Clusters']: print('   ', c['Context'][-28:], 'reachable' if c['Reachable'] else 'UNREACHABLE', c.get('Kubernetes'), c.get('Provider'), 'nodes', c['Nodes'], 'zones', c.get('Zones'), 'cpu %.0f%%' % c['CPURequestedPct'])
print('    totals', r['Totals'])
sys.exit(0 if any(c['Reachable'] and c.get('Provider')=='azure' and c['Nodes']==2 for c in r['Clusters']) else 1)" "$LAB/fleet.json" && ok "fleet reports the AKS cluster with provider azure and its two nodes" || bad "fleet did not describe the AKS cluster"
echo "== $PASS passed, $FAILN failed"
