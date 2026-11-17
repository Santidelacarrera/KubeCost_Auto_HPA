#!/usr/bin/env bash
# REAL Argo CD (core install) and REAL Flux in a lean minikube profile, both syncing a workload from an in-cluster git server.
# Checks that the tool detects them from the marks they REALLY write, refuses to patch their workloads, that a direct patch IS reverted
# by the reconcilers (why the refusal exists), and that `rightsize propose` -> merge -> push makes the controllers apply the change.
# SIMULATED: the recommendations are hand-made (no Prometheus here). REAL: Kubernetes 1.33.4, Argo CD, Flux, the git protocol.
set -u
PROFILE="${PROFILE:-kubecost-gitops}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"; LAB="$HERE/.lab"; mkdir -p "$LAB"
K="kubectl --context $PROFILE"
PASS=0; FAILN=0
ok()  { echo "PASS  $*"; PASS=$((PASS+1)); }
bad() { echo "FAIL  $*"; FAILN=$((FAILN+1)); }
check() { if eval "$2"; then ok "$1"; else bad "$1"; fi; }
[ "$PROFILE" = "kubecost-gitops" ] || [ "${ALLOW_CTX:-}" = "1" ] || { echo "refusing profile '$PROFILE'"; exit 2; }
docker info >/dev/null 2>&1 || { echo "Docker is not running"; exit 2; }

if ! minikube profile list 2>/dev/null | grep -q "$PROFILE"; then
  minikube start -p "$PROFILE" --driver=docker --kubernetes-version=v1.33.4 --cpus=2 --memory=3500 --wait=all || exit 2
fi
if ! $K -n argocd get deploy 2>/dev/null | grep -q . && ! $K -n argocd get statefulset 2>/dev/null | grep -q .; then
  $K create ns argocd >/dev/null 2>&1
  $K apply -n argocd --server-side -f https://raw.githubusercontent.com/argoproj/argo-cd/stable/manifests/core-install.yaml >/dev/null || exit 2
  $K apply --server-side -f https://github.com/fluxcd/flux2/releases/latest/download/install.yaml >/dev/null || exit 2
  $K -n argocd wait --for=condition=Ready pod --all --timeout=300s >/dev/null
  $K -n flux-system wait --for=condition=Ready pod --all --timeout=300s >/dev/null
fi
$K -n gitops-demo get deploy web-argo >/dev/null 2>&1 || bash "$HERE/hack/lab/gitops-setup.sh" | grep -v '^warning'
(cd "$HERE/operator" && go build -o "$LAB/rightsize.exe" ./cmd/rightsize) || exit 2
RS="$LAB/rightsize.exe"; AUDIT="$LAB/audit-gitops.jsonl"; : > "$AUDIT"
WORK="$LAB/gitops/work"
POLICY="$LAB/gitops-policy.yaml"; printf 'allowNamespaces: [gitops-demo]\nrequireApproval: false\nmaxChangeFraction: 0.6\nmaxChanges: 9\nper: 1h\n' > "$POLICY"
live() { $K -n gitops-demo get deploy "$1" -o jsonpath='{.spec.template.spec.containers[0].resources.requests.cpu}/{.spec.template.spec.containers[0].resources.requests.memory}'; }
waitfor() { # waitfor SECONDS CMD...
  local t="$1"; shift; for _ in $(seq 1 $((t/5))); do eval "$*" && return 0; sleep 5; done; return 1; }

echo "== versions (recorded for COMPATIBILITY.md)"
$K -n argocd get statefulset,deploy -o custom-columns=NAME:.metadata.name,IMAGE:.spec.template.spec.containers[0].image --no-headers 2>/dev/null | grep -E "application-controller|repo-server"
$K -n flux-system get deploy -o custom-columns=NAME:.metadata.name,IMAGE:.spec.template.spec.containers[0].image --no-headers 2>/dev/null | grep -E "source-controller|kustomize-controller"
echo "== 1. both controllers manage a workload"
waitfor 240 "[ \"\$($K -n argocd get application web-argo -o jsonpath='{.status.sync.status}/{.status.health.status}' 2>/dev/null)\" = Synced/Healthy ]"
check "Argo CD: Synced/Healthy"             "[ \"\$($K -n argocd get application web-argo -o jsonpath='{.status.sync.status}/{.status.health.status}')\" = Synced/Healthy ]"
waitfor 240 "[ \"\$($K -n flux-system get kustomization web-flux -o jsonpath='{.status.conditions[0].status}' 2>/dev/null)\" = True ]"
check "Flux: Kustomization applied"         "[ \"\$($K -n flux-system get kustomization web-flux -o jsonpath='{.status.conditions[0].status}')\" = True ]"
echo "marks written by Argo CD:  $($K -n gitops-demo get deploy web-argo -o jsonpath='{.metadata.annotations.argocd\.argoproj\.io/tracking-id}')"
echo "marks written by Flux:     $($K -n gitops-demo get deploy web-flux -o jsonpath='{.metadata.labels}')"

echo "== 2. the tool detects both from the REAL marks and refuses"
python "$HERE/hack/lab/handmade-rec.py" web-argo > "$LAB/rec-web-argo.json"; python "$HERE/hack/lab/handmade-rec.py" web-flux > "$LAB/rec-web-flux.json"
OUT="$("$RS" apply --recommendation "$LAB/rec-web-argo.json" --policy "$POLICY" --context "$PROFILE" --audit-file "$AUDIT" 2>/dev/null)"
check "web-argo: denied, managed by argocd" "echo '$OUT' | grep -q 'argocd'"
OUT="$("$RS" apply --recommendation "$LAB/rec-web-flux.json" --policy "$POLICY" --context "$PROFILE" --audit-file "$AUDIT" 2>/dev/null)"
check "web-flux: denied, managed by flux"  "echo '$OUT' | grep -q 'flux'"
BEFORE_A="$(live web-argo)"; BEFORE_F="$(live web-flux)"
"$RS" apply --recommendation "$LAB/rec-web-argo.json" --policy "$POLICY" --context "$PROFILE" --execute --observe-for 5s --audit-file "$AUDIT" >/dev/null 2>&1
check "--execute on an Argo CD workload changed nothing" "[ \"\$(live web-argo)\" = \"$BEFORE_A\" ]"

echo "== 3. why the refusal exists: a direct patch is reverted by the reconcilers"
$K -n gitops-demo patch deploy web-argo --type=json -p '[{"op":"replace","path":"/spec/template/spec/containers/0/resources/requests/cpu","value":"300m"}]' >/dev/null
$K -n gitops-demo patch deploy web-flux --type=json -p '[{"op":"replace","path":"/spec/template/spec/containers/0/resources/requests/cpu","value":"300m"}]' >/dev/null
echo "patched both to 300m; waiting for the reconcilers"
waitfor 180 "[ \"\$(live web-argo)\" = \"$BEFORE_A\" ]"
check "Argo CD self-heal reverted the direct patch"  "[ \"\$(live web-argo)\" = \"$BEFORE_A\" ]"
waitfor 240 "[ \"\$(live web-flux)\" = \"$BEFORE_F\" ]"
check "Flux reverted the direct patch at its interval" "[ \"\$(live web-flux)\" = \"$BEFORE_F\" ]"

echo "== 4. the GitOps path: propose -> review/merge -> push -> the controllers apply it"
for app in argo flux; do
  W="web-$app"; F="$app/web.yaml"
  "$RS" propose --recommendation "$LAB/rec-$W.json" --repo "$WORK" --file "$F" > "$LAB/propose-$W.out" 2>"$LAB/propose-$W.err"
  BR="$(python -c "import json,sys; print(json.load(open(sys.argv[1]))['branch'])" "$LAB/propose-$W.out" 2>/dev/null)"
  check "$W: proposal branch created ($BR), working branch untouched" "[ -n \"$BR\" ] && [ \"\$(git -C $WORK rev-parse --abbrev-ref HEAD)\" = main ]"
  check "$W: the YAML comment survived"   "git -C $WORK show $BR:$F | grep -q 'keep this comment'"
  git -C "$WORK" merge -q --ff-only "$BR" && git -C "$WORK" push -q origin main   # the 'reviewed pull request' is merged here
done
$K -n argocd annotate application web-argo argocd.argoproj.io/refresh=hard --overwrite >/dev/null
$K -n flux-system annotate gitrepository app reconcile.fluxcd.io/requestedAt="$(date +%s)" --overwrite >/dev/null
waitfor 240 "[ \"\$(live web-argo)\" = \"250m/64Mi\" ]"
check "Argo CD applied the merged change (500m/128Mi -> 250m/64Mi)" "[ \"\$(live web-argo)\" = \"250m/64Mi\" ]"
waitfor 240 "[ \"\$(live web-flux)\" = \"250m/64Mi\" ]"
check "Flux applied the merged change"       "[ \"\$(live web-flux)\" = \"250m/64Mi\" ]"
sleep 20
check "the values persist (no reconciler fought them)" "[ \"\$(live web-argo)\" = \"250m/64Mi\" ] && [ \"\$(live web-flux)\" = \"250m/64Mi\" ]"
OUT="$("$RS" apply --recommendation "$LAB/rec-web-argo.json" --policy "$POLICY" --context "$PROFILE" 2>/dev/null)"
check "the old recommendation is now stale/denied" "echo '$OUT' | grep -q 'stale\|argocd'"

echo; echo "RESULT: $PASS passed, $FAILN failed"
[ "$FAILN" -eq 0 ]
