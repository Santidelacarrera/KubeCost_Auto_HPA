#!/usr/bin/env bash
# Isolated local resize + Helm lifecycle lab; does not operate on a preexisting CTX.
# The historical rightsize/recommend CLIs are absent; this wrapper states its actual scope.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
exec "${PYTHON:-python3}" "$ROOT/hack/local_enterprise_lab.py" "$@"
