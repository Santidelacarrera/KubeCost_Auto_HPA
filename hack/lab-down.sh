#!/usr/bin/env bash
# Deletes ONLY the lab minikube profile created by hack/lab-up.sh (default "kubecost-lab"). Other profiles are never touched.
set -eu
PROFILE="${PROFILE:-kubecost-lab}"
[ "$PROFILE" = "kubecost-lab" ] || [ "${ALLOW_PROFILE:-}" = "1" ] || { echo "refusing to delete profile '$PROFILE' (set ALLOW_PROFILE=1 if it is yours to delete)"; exit 2; }
minikube delete -p "$PROFILE"
