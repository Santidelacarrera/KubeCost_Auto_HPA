#!/usr/bin/env bash
# Offline rehearsal of what a v* tag would produce. It publishes NOTHING and creates no tag: it
# builds in a temp dir and uses a closed local port to check OCI reference syntax.
# Needs: helm, go, python3. Usage: hack/release-dry-run.sh [vX.Y.Z]   (default v0.1.0)
set -euo pipefail
cd "$(dirname "$0")/.."
TAG=${1:-v0.1.0}
OWNER=${OWNER:-Santidelacarrera}   # as GitHub reports it (mixed case on purpose)
fail() { echo "FAIL: $*" >&2; exit 1; }
ok()   { echo "ok:   $*"; }

[[ $TAG =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || fail "tag $TAG is not vMAJOR.MINOR.PATCH[-pre]"
VERSION=${TAG#v}
OWNER_LC=$(echo "$OWNER" | tr '[:upper:]' '[:lower:]')
WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT
ok "tag $TAG -> version $VERSION, registry owner $OWNER_LC"

# 1. chart packaging carries the tag's version
helm package deploy/helm/kubecost-auto-hpa --version "$VERSION" --app-version "$VERSION" --destination "$WORK" >/dev/null
PKG="$WORK/kubecost-auto-hpa-$VERSION.tgz"; [[ -f $PKG ]] || fail "chart package $PKG not produced"
helm show chart "$PKG" | grep -q "^version: $VERSION$"       || fail "chart version is not $VERSION"
helm show chart "$PKG" | grep -q "^appVersion: \"\?$VERSION\"\?$" || fail "chart appVersion is not $VERSION"
ok "chart packaged as kubecost-auto-hpa-$VERSION.tgz with version and appVersion $VERSION"

# 2. the packaged chart installs the images the release publishes (default tag = appVersion)
helm lint "$PKG" >/dev/null
RENDER=$(helm template r "$PKG")
for c in operator engine; do
  echo "$RENDER" | grep -q "image: \"\?ghcr.io/$OWNER_LC/kubecost-auto-hpa-$c:$VERSION\"\?$" \
    || fail "chart does not reference ghcr.io/$OWNER_LC/kubecost-auto-hpa-$c:$VERSION (release.yml pushes exactly that tag)"
done
ok "rendered manifests reference ghcr.io/$OWNER_LC/kubecost-auto-hpa-{operator,engine}:$VERSION"

# 3. the OCI reference the chart job pushes to is syntactically valid (lower-case) and the mixed-case one is not
MIXED=$(helm push "$PKG" "oci://localhost:1/$OWNER/charts" 2>&1 || true)
LOWER=$(helm push "$PKG" "oci://localhost:1/$OWNER_LC/charts" 2>&1 || true)
if [[ $OWNER != "$OWNER_LC" ]]; then
  [[ $MIXED == *invalid_reference* ]] || fail "expected helm to reject the mixed-case OCI repository, got: $MIXED"
  ok "mixed-case owner is rejected by helm (release.yml lower-cases it, as required)"
fi
[[ $LOWER != *invalid_reference* ]] || fail "the lower-case OCI reference was rejected: $LOWER"
ok "lower-case OCI reference is accepted by helm (no registry contacted: connection refused is the expected outcome)"

# 4. version and commit reach the operator binary (the Dockerfile uses the same -ldflags)
COMMIT=$(git rev-parse HEAD)
( cd operator && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$VERSION -X main.commit=$COMMIT" -o "$WORK/manager" ./cmd )
OUT=$("$WORK/manager" --version)
[[ $OUT == *"$VERSION"* && $OUT == *"$COMMIT"* ]] || fail "binary --version is '$OUT'"
ok "operator binary reports: $OUT"

# 5. the Dockerfile passes the same build args the workflow sets
grep -q 'ARG VERSION' operator/Dockerfile && grep -q 'ARG COMMIT' operator/Dockerfile && grep -q 'build-args' .github/workflows/release.yml \
  || fail "Dockerfile/workflow build-arg wiring is missing"
ok "Dockerfile ARG VERSION/COMMIT and release.yml build-args are present"

echo "release dry-run passed for $TAG (nothing was built as an image, signed or published)"
