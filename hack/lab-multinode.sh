#!/usr/bin/env bash
# REAL two-node minikube cluster (profile kubecost-multi): checks that `rightsize nodes` predictions match what the scheduler really does
# when a node is drained. The cluster is disposable: the script drains and uncordons the worker. No Prometheus is needed.
set -u
PROFILE="${PROFILE:-kubecost-multi}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"; LAB="$HERE/.lab"; mkdir -p "$LAB"
K="kubectl --context $PROFILE"
PASS=0; FAILN=0
ok()  { echo "PASS  $*"; PASS=$((PASS+1)); }
bad() { echo "FAIL  $*"; FAILN=$((FAILN+1)); }
check() { if eval "$2"; then ok "$1"; else bad "$1"; fi; }
[ "$PROFILE" = "kubecost-multi" ] || [ "${ALLOW_CTX:-}" = "1" ] || { echo "refusing profile '$PROFILE'"; exit 2; }
if ! minikube profile list 2>/dev/null | grep -q "$PROFILE"; then
  minikube start -p "$PROFILE" --driver=docker --kubernetes-version=v1.33.4 --nodes=2 --cpus=2 --memory=2400 --wait=all || exit 2
fi
(cd "$HERE/operator" && go build -o "$LAB/rightsize.exe" ./cmd/rightsize) || exit 2
RS="$LAB/rightsize.exe"
W=kubecost-multi-m02; CP=kubecost-multi
$K uncordon "$W" >/dev/null 2>&1
$K create ns mn >/dev/null 2>&1

deploy() { # name replicas cpu
cat <<YAML | $K apply -f - >/dev/null
apiVersion: apps/v1
kind: Deployment
metadata: {name: $1, namespace: mn}
spec:
  replicas: $2
  selector: {matchLabels: {app: $1}}
  template:
    metadata: {labels: {app: $1}}
    spec:
      containers:
      - name: app
        image: busybox:1.36
        command: ["sh", "-c", "while true; do sleep 30; done"]
        resources: {requests: {cpu: $3, memory: 100Mi}}
YAML
}
removable() { "$RS" nodes --context "$PROFILE" --node-hourly-usd 0.2 > "$LAB/nodes-mn.json" 2>/dev/null; (cd "$LAB" && python -c "import json; r=json.load(open('nodes-mn.json')); print(','.join(r['Removable'] or []))"); }
placement() { $K -n mn get pods -o wide --no-headers | awk '{print $7}' | sort | uniq -c | tr '\n' ' '; }

echo "== 1. two real nodes"
check "two Ready nodes" "[ \"\$($K get nodes --no-headers | grep -c ' Ready')\" = 2 ]"

echo "== 2. a workload that fits on one node"
deploy spread 4 100m
cat <<YAML | $K apply -f - >/dev/null
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata: {name: spread, namespace: mn}
spec: {minAvailable: 2, selector: {matchLabels: {app: spread}}}
YAML
$K -n mn rollout status deploy/spread --timeout=180s >/dev/null
echo "placement: $(placement)"
R="$(removable)"; echo "tool says removable: [$R]"
check "the tool reports a removable candidate"        "[ -n \"$R\" ]"
check "the report flags the PodDisruptionBudget"      "grep -q PodDisruptionBudget $LAB/nodes-mn.json"
check "the report says a candidate is not a saving"   "grep -q CANDIDATE $LAB/nodes-mn.json"
CAND="$(echo "$R" | cut -d, -f1)"
if [ -n "$CAND" ]; then
  echo "draining $CAND to test the prediction"
  $K drain "$CAND" --ignore-daemonsets --delete-emptydir-data --timeout=150s >/dev/null 2>&1
  sleep 10
  OTHER="$($K get nodes --no-headers | awk '{print $1}' | grep -v "^$CAND$" | head -1)"
  check "after the drain all workload pods run on the other node ($OTHER)" "[ \"\$($K -n mn get pods --no-headers -o wide | awk '{print \$3,\$7}' | grep -vc \"Running $OTHER\")\" = 0 ]"
  check "no workload pod is Pending"                   "[ \"\$($K -n mn get pods --no-headers | grep -c Pending)\" = 0 ]"
  $K uncordon "$CAND" >/dev/null
fi

echo "== 3. a workload that does NOT fit on one node"
$K -n mn delete deploy spread >/dev/null 2>&1; $K -n mn delete pdb spread >/dev/null 2>&1
sleep 20   # let the previous pods finish terminating
deploy big 3 4500m   # minikube nodes advertise the host CPU count (12), so the requests must be large to not fit
$K -n mn rollout status deploy/big --timeout=180s >/dev/null
PENDING_BEFORE="$($K -n mn get pods --no-headers | grep -c Pending)"
echo "placement: $(placement)"
R2="$(removable)"; echo "tool says removable: [$R2]"
check "the tool reports nothing removable"            "[ -z \"$R2\" ]"
check "the report explains why (does not fit)"        "grep -q 'does not fit' $LAB/nodes-mn.json"
$K drain "$W" --ignore-daemonsets --delete-emptydir-data --timeout=60s --disable-eviction=false >/dev/null 2>&1
sleep 15
check "draining anyway leaves pods Pending (the prediction was right; none were Pending before)" "[ \"$PENDING_BEFORE\" = 0 ] && [ \"\$($K -n mn get pods --no-headers | grep -c Pending)\" -ge 1 ]"
$K uncordon "$W" >/dev/null

$K delete ns mn --ignore-not-found >/dev/null 2>&1
echo; echo "RESULT: $PASS passed, $FAILN failed"
[ "$FAILN" -eq 0 ]
