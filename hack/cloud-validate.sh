#!/usr/bin/env bash
# Read-only preflight wrapper. No rightsize/recommend CLI is assumed to exist.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
: "${CTX:?set CTX to the explicitly selected cluster context}"
: "${PROVIDER:?set PROVIDER to gke or aks}"
ARGS=(--context "$CTX" --provider "$PROVIDER")
if [ "${CONFIRM_CTX:-}" = "$CTX" ]; then ARGS+=(--authorized-context "$CTX"); fi
exec "${PYTHON:-python3}" "$ROOT/hack/cloud_preflight.py" "${ARGS[@]}" "$@"
