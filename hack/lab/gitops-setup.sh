#!/usr/bin/env bash
# Sets up a REAL Argo CD (core install) and a REAL Flux in the lean minikube profile "kubecost-gitops", both syncing from one in-cluster
# git smart-HTTP server (hack/lab/git-http). The work repo is pushed to it through a port-forward on localhost:8081.
# Nothing leaves the machine. Argo CD and Flux must already be installed (see hack/lab-gitops.sh).
set -eu
CTX="${CTX:-kubecost-gitops}"
HERE="$(cd "$(dirname "$0")/../.." && pwd)"
G="$HERE/.lab/gitops"; WORK="$G/work"
K="kubectl --context $CTX"
rm -rf "$G"; mkdir -p "$WORK/argo" "$WORK/flux"

manifest() { # name
cat <<YAML
apiVersion: apps/v1
kind: Deployment
metadata:
  name: $1
  namespace: gitops-demo
spec:
  replicas: 1
  selector: {matchLabels: {app: $1}}
  template:
    metadata: {labels: {app: $1}}
    spec:
      containers:
      - name: app   # keep this comment
        image: busybox:1.36
        command: ["sh", "-c", "while true; do sleep 30; done"]
        resources:
          requests:
            cpu: "500m"
            memory: 128Mi
          limits:
            cpu: "1"
            memory: 256Mi
YAML
}
manifest web-argo > "$WORK/argo/web.yaml"
manifest web-flux > "$WORK/flux/web.yaml"

docker build -q -t lab-git-http:1 "$HERE/hack/lab/git-http" >/dev/null
minikube -p "$CTX" image load lab-git-http:1
$K create ns gitops >/dev/null 2>&1 || true
$K create ns gitops-demo >/dev/null 2>&1 || true
cat <<'YAML' | $K apply -f - >/dev/null
apiVersion: apps/v1
kind: Deployment
metadata: {name: git-http, namespace: gitops}
spec:
  replicas: 1
  selector: {matchLabels: {app: git-http}}
  template:
    metadata: {labels: {app: git-http}}
    spec:
      containers:
      - name: git
        image: lab-git-http:1
        imagePullPolicy: Never
        ports: [{containerPort: 80}]
---
apiVersion: v1
kind: Service
metadata: {name: git-http, namespace: gitops}
spec: {selector: {app: git-http}, ports: [{port: 80}]}
YAML
$K -n gitops rollout status deploy/git-http --timeout=180s >/dev/null

# push the work repo through a port-forward
(taskkill //F //IM kubectl.exe >/dev/null 2>&1 || true)
($K -n gitops port-forward svc/git-http 8081:80 >/dev/null 2>&1 &)
for _ in $(seq 1 20); do curl -fs http://localhost:8081/healthz >/dev/null 2>&1 && break; sleep 2; done
git -C "$WORK" init -q -b main
git -C "$WORK" config core.autocrlf false
git -C "$WORK" config user.email lab@example.com; git -C "$WORK" config user.name lab; git -C "$WORK" config commit.gpgsign false
git -C "$WORK" add . && git -C "$WORK" commit -q -m "initial manifests"
git -C "$WORK" remote add origin http://localhost:8081/app.git
git -C "$WORK" push -q origin main

cat <<'YAML' | $K apply -f - >/dev/null
apiVersion: argoproj.io/v1alpha1
kind: AppProject
metadata: {name: default, namespace: argocd}
spec:
  sourceRepos: ['*']
  destinations: [{namespace: '*', server: '*'}]
  clusterResourceWhitelist: [{group: '*', kind: '*'}]
---
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata: {name: web-argo, namespace: argocd}
spec:
  project: default
  source: {repoURL: "http://git-http.gitops.svc.cluster.local/app.git", targetRevision: main, path: argo}
  destination: {server: "https://kubernetes.default.svc", namespace: gitops-demo}
  syncPolicy: {automated: {prune: true, selfHeal: true}}
---
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata: {name: app, namespace: flux-system}
spec:
  interval: 1m
  url: http://git-http.gitops.svc.cluster.local/app.git
  ref: {branch: main}
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata: {name: web-flux, namespace: flux-system}
spec:
  interval: 1m
  path: ./flux
  prune: true
  sourceRef: {kind: GitRepository, name: app}
  targetNamespace: gitops-demo
YAML
echo "applied Application, GitRepository, Kustomization"
