#!/usr/bin/env bash
# Licence enforcement on a REAL cluster (the disposable minikube lab): the operator image built from this checkout, installed
# with the chart, configured to WRITE (observe-only off, applier on) and started with --enforce-license under four licences.
# What it shows: what the running operator decided, read from its own log and its kubecost_license_info metric.
#   bash hack/lab-up.sh && bash hack/lab-license.sh
set -u
CTX="${CTX:-kubecost-lab}"; PROFILE="${PROFILE:-kubecost-lab}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"; LAB="$HERE/.lab/license"; rm -rf "$LAB"; mkdir -p "$LAB"
REL=lic; NS=kubecost-license-lab; MP="${METRICS_PORT:-18080}"
PASS=0; FAILN=0
ok()  { echo "PASS  $*"; PASS=$((PASS+1)); }
bad() { echo "FAIL  $*"; FAILN=$((FAILN+1)); }
[ "$CTX" = "kubecost-lab" ] || { echo "refusing context '$CTX'"; exit 2; }
K="kubectl --context $CTX"; H="helm --kube-context $CTX"; RS="$LAB/rightsize.exe"
$K get ns >/dev/null 2>&1 || { echo "cluster not reachable"; exit 2; }
(cd "$HERE/operator" && go build -o "$RS" ./cmd/rightsize) || exit 2

echo "== images"
docker build -q -t kubecost-auto-hpa-operator:lic --build-arg VERSION=lic --build-arg COMMIT="$(git -C "$HERE" rev-parse HEAD)" "$HERE/operator" >/dev/null || exit 2
docker build -q -t kubecost-auto-hpa-engine:lic "$HERE/engine" >/dev/null || exit 2
minikube -p "$PROFILE" image load kubecost-auto-hpa-operator:lic kubecost-auto-hpa-engine:lic || exit 2

echo "== vendor key and licences"
"$RS" keygen --out "$LAB/vendor" >/dev/null || exit 2
"$RS" keygen --out "$LAB/other" >/dev/null || exit 2
"$RS" license issue --key "$LAB/vendor.key" --id L-PRO --customer lab --tier professional --max-nodes 10 --days 30 --out "$LAB/pro.license" >/dev/null || exit 2
"$RS" license issue --key "$LAB/vendor.key" --id L-ENT --customer lab --tier enterprise --days 30 --out "$LAB/ent.license" >/dev/null || exit 2
"$RS" license issue --key "$LAB/other.key" --id L-FAKE --customer lab --tier enterprise --days 30 --out "$LAB/forged.license" >/dev/null || exit 2
"$RS" license verify --file "$LAB/pro.license" --public-key "$LAB/vendor.pub" >/dev/null && ok "the CLI verifies the professional licence offline" || bad "license verify"
"$RS" license verify --file "$LAB/forged.license" --public-key "$LAB/vendor.pub" >/dev/null 2>&1 && bad "a licence signed by another key verified" || ok "the CLI refuses a licence signed by another key"

$K create ns "$NS" >/dev/null 2>&1
cat > "$LAB/values.yaml" <<EOF
operator:
  image: {repository: kubecost-auto-hpa-operator, tag: lic, pullPolicy: Never}
  replicas: 1
  leaderElect: false
  podDisruptionBudget: {enabled: false}
  observeOnly: false                 # the operator is CONFIGURED to write: only the licence may restrict it
  enableSpotDrain: false
  recommendationApplier:
    enabled: true
    namespaces: ["$NS"]
    policy: |
      allowNamespaces: []
  extraArgs:
    - --license-file=/license/license.json
    - --license-public-key-file=/license/license.pub
    - --enforce-license
  extraVolumes:
    - {name: license, secret: {secretName: kubecost-license, optional: true}}
  extraVolumeMounts:
    - {name: license, mountPath: /license, readOnly: true}
engine:
  image: {repository: kubecost-auto-hpa-engine, tag: lic, pullPolicy: Never}
  replicas: 1
  podDisruptionBudget: {enabled: false}
prometheus: {url: "http://prom-prometheus-server.monitoring:80"}
kubecost: {url: ""}
EOF
OP="deploy/$REL-kubecost-auto-hpa-operator"

# scenario NAME LICENSE_FILE(or "") -> restarts the operator with that licence and leaves its log in $LAB/NAME.log, its metric line in $LAB/NAME.metric
scenario() {
  local name="$1" file="$2"
  $K -n "$NS" delete secret kubecost-license >/dev/null 2>&1
  if [ -n "$file" ]; then
    $K -n "$NS" create secret generic kubecost-license --from-file=license.json="$file" --from-file=license.pub="$LAB/vendor.pub" >/dev/null || return 1
  fi
  if $H status "$REL" -n "$NS" >/dev/null 2>&1; then
    $K -n "$NS" rollout restart "$OP" >/dev/null
  else
    $H install "$REL" "$HERE/deploy/helm/kubecost-auto-hpa" -n "$NS" -f "$LAB/values.yaml" --wait --timeout 5m >/dev/null 2>"$LAB/helm.err" || { cat "$LAB/helm.err"; return 1; }
  fi
  $K -n "$NS" rollout status "$OP" --timeout=240s >/dev/null || return 1
  sleep 8
  $K -n "$NS" logs "$OP" --tail=200 > "$LAB/$name.log" 2>&1
  ($K -n "$NS" port-forward "$OP" "$MP:8080" >/dev/null 2>&1 &) ; sleep 5
  curl -s -m 5 "http://localhost:$MP/metrics" | grep '^kubecost_license_info' > "$LAB/$name.metric"
  # stop the forward (Git Bash: no pkill)
  powershell -NoProfile -Command "Get-CimInstance Win32_Process -Filter \"Name='kubectl.exe'\" | Where-Object { \$_.CommandLine -like '*port-forward*$MP:8080*' } | ForEach-Object { Stop-Process -Id \$_.ProcessId -Force }" >/dev/null 2>&1
  echo "   $name: $(cat "$LAB/$name.metric")"
}
has() { grep -q -- "$2" "$LAB/$1.log"; }
metric() { grep -q -- "$2" "$LAB/$1.metric"; }

echo "== 1. enforcement, no licence"
scenario none "" || bad "scenario none did not start"
has none "observe-only mode" && ok "no licence: the operator runs observe-only although it was configured to write" || bad "no licence: observe-only not forced"
has none "recommendation applier enabled" && bad "no licence: the applier started" || ok "no licence: the applier was not started"
metric none 'tier="starter"' && metric none 'valid="false"' && metric none 'enforced="true"' && ok "no licence: metric says starter, invalid, enforced" || bad "no licence: metric"

echo "== 2. professional licence"
scenario pro "$LAB/pro.license" || bad "scenario pro did not start"
has pro "observe-only mode" && bad "professional: observe-only was forced" || ok "professional: the operator is not forced into observe-only"
has pro "recommendation applier enabled" && ok "professional: the applier started" || bad "professional: the applier did not start"
metric pro 'tier="professional"' && metric pro 'valid="true"' && ok "professional: metric says professional, valid" || bad "professional: metric"

echo "== 3. a licence signed by another key (forged)"
scenario forged "$LAB/forged.license" || bad "scenario forged did not start"
has forged "observe-only mode" && ok "forged licence: observe-only" || bad "forged licence: observe-only not forced"
has forged "does not verify" && ok "forged licence: the log says the signature does not verify" || bad "forged licence: reason missing from the log"
metric forged 'tier="starter"' && metric forged 'valid="false"' && ok "forged licence: metric says starter, invalid" || bad "forged licence: metric"

echo "== 4. enterprise licence"
scenario ent "$LAB/ent.license" || bad "scenario ent did not start"
has ent "recommendation applier enabled" && ! has ent "observe-only mode" && ok "enterprise: the applier started and nothing was restricted" || bad "enterprise: restricted"
metric ent 'tier="enterprise"' && ok "enterprise: metric says enterprise" || bad "enterprise: metric"

echo "== 5. the pods stay healthy through all four"
[ "$($K -n "$NS" get pods --no-headers | grep -vc ' 1/1 .*Running')" = 0 ] && ok "operator, engine and redis are running" || { bad "a pod is not running"; $K -n "$NS" get pods; }

[ "${KEEP:-}" = 1 ] || { $H uninstall "$REL" -n "$NS" >/dev/null 2>&1; $K delete ns "$NS" --wait=false >/dev/null 2>&1; }
echo "== $PASS passed, $FAILN failed"
