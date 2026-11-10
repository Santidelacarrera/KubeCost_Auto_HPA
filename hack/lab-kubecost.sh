#!/usr/bin/env bash
# Installs a REAL Kubecost (free tier, chart kubecost/cost-analyzer, its own bundled Prometheus) on a local minikube profile
# and runs the live contract test of the KubeCost client (operator/internal/kubecost/live_test.go) against it.
# REAL: Kubecost's binary, its API (/model/allocation) and its response format. NOT REAL: the prices (Kubecost's default
# on-prem price list for a laptop VM); nothing here is a bill.
# Usage: bash hack/lab-kubecost.sh          (KEEP=1 keeps the cluster; DELETE=1 deletes the profile at the end)
set -u
PROFILE="${PROFILE:-kubecost-real}"
VERSION="${KUBECOST_CHART_VERSION:-2.8.7}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"; LAB="$HERE/.lab"; mkdir -p "$LAB"
K="kubectl --context $PROFILE"; H="helm --kube-context $PROFILE"
PASS=0; FAILN=0
ok()  { echo "PASS  $*"; PASS=$((PASS+1)); }
bad() { echo "FAIL  $*"; FAILN=$((FAILN+1)); }
check() { if eval "$2"; then ok "$1"; else bad "$1"; fi; }
[ "$PROFILE" = "kubecost-real" ] || [ "${ALLOW_CTX:-}" = "1" ] || { echo "refusing profile '$PROFILE'"; exit 2; }
docker info >/dev/null 2>&1 || { echo "Docker is not running"; exit 2; }
if ! minikube profile list 2>/dev/null | grep -q "$PROFILE"; then
  minikube start -p "$PROFILE" --driver=docker --kubernetes-version=v1.33.4 --cpus=3 --memory=5000 --wait=all || exit 2
fi

echo "== a workload to be priced"
$K create ns shop --dry-run=client -o yaml | $K apply -f - >/dev/null
$K -n shop create deployment payments-api --image=nginx:1.27-alpine --replicas=2 --dry-run=client -o yaml | $K apply -f - >/dev/null
$K -n shop set resources deployment payments-api --requests=cpu=100m,memory=64Mi >/dev/null
$K -n shop rollout status deployment payments-api --timeout=180s >/dev/null
check "the workload runs" "[ \"\$($K -n shop get deploy payments-api -o jsonpath='{.status.availableReplicas}')\" = 2 ]"

echo "== Kubecost $VERSION (free tier; forecasting, Grafana and network costs off; 2.9.x refuses to install without a federated object store, it only prepares the move to 3.0 to fit the laptop VM)"
helm repo add kubecost https://kubecost.github.io/cost-analyzer/ >/dev/null 2>&1 || true
helm repo update kubecost >/dev/null 2>&1
$H upgrade --install kubecost kubecost/cost-analyzer --version "$VERSION" -n kubecost --create-namespace \
  --set global.clusterId="$PROFILE" --set prometheus.server.global.external_labels.cluster_id="$PROFILE" --set forecasting.enabled=false \
  --set global.grafana.enabled=false --set global.grafana.proxy=false --set networkCosts.enabled=false \
  --wait --timeout 15m > "$LAB/kubecost-install.log" 2>&1
check "helm install of the real Kubecost chart succeeded" "grep -q 'STATUS: deployed' $LAB/kubecost-install.log" || true
grep -q 'STATUS: deployed' "$LAB/kubecost-install.log" || { tail -5 "$LAB/kubecost-install.log"; $K -n kubecost get pods 2>&1; echo "RESULT: $PASS passed, $FAILN failed"; exit 1; }
$K -n kubecost get pods -o wide > "$LAB/kubecost-pods.txt" 2>&1; cat "$LAB/kubecost-pods.txt"
echo "Kubecost image: $($K -n kubecost get deploy -l app=cost-analyzer -o jsonpath='{.items[0].spec.template.spec.containers[*].image}' 2>/dev/null)"

echo "== waiting until Kubecost has priced the workload (it needs a few scrapes and an allocation build)"
($K -n kubecost port-forward svc/kubecost-cost-analyzer 19090:9090 >/dev/null 2>&1 &)
sleep 5
URL=http://localhost:19090
got=0
for i in $(seq 1 60); do
  curl -s "$URL/model/allocation?window=1h&aggregate=cluster,namespace,controllerKind,controller&accumulate=true" > "$LAB/kubecost-alloc.json" 2>/dev/null
  if grep -q 'payments-api' "$LAB/kubecost-alloc.json" && grep -q '"totalCost"' "$LAB/kubecost-alloc.json"; then got=1; break; fi
  # the port-forward dies when a pod restarts; re-open it
  curl -s -o /dev/null "$URL/healthz" || { ($K -n kubecost port-forward svc/kubecost-cost-analyzer 19090:9090 >/dev/null 2>&1 &); sleep 5; }
  sleep 30
done
echo "allocation answer (first 600 chars): $(head -c 600 "$LAB/kubecost-alloc.json")"
check "Kubecost's /model/allocation prices the workload (after $((i*30/60)) min)" "[ $got = 1 ]"

echo "== the client's live contract test against it"
(cd "$HERE/operator" && KUBECOST_LIVE_URL="$URL" KUBECOST_LIVE_NAMESPACE=shop KUBECOST_LIVE_CONTROLLER=payments-api \
  KUBECOST_LIVE_WINDOW=1h go test -tags kubecost_live -count=1 -v -run Live ./internal/kubecost/) > "$LAB/kubecost-live-test.log" 2>&1
cat "$LAB/kubecost-live-test.log" | tail -20
check "live contract test PASSES against a real Kubecost" "grep -q '^--- PASS' $LAB/kubecost-live-test.log && ! grep -q -- '--- SKIP' $LAB/kubecost-live-test.log"

taskkill //F //IM kubectl.exe >/dev/null 2>&1 || true
if [ "${DELETE:-}" = "1" ]; then minikube delete -p "$PROFILE" >/dev/null 2>&1; fi
echo; echo "RESULT: $PASS passed, $FAILN failed"
[ "$FAILN" -eq 0 ]
