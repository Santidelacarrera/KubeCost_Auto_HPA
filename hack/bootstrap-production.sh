#!/usr/bin/env bash
set -euo pipefail

# Zero-Touch Production Bootstrap for KubeCost_Auto_HPA (Nexus Security & AI Lab)
NAMESPACE="${NAMESPACE:-kubecost-system}"
RELEASE="${RELEASE:-kubecost-auto-hpa}"
TENANT_ID="${TENANT_ID:-6f1c2a7e-0b3d-4c8e-9a51-2d7f0e4b9c10}"
OUT_DIR="${OUT_DIR:-./.prod-bootstrap}"

mkdir -p "$OUT_DIR"

ENGINE_TOKEN="$(python3 -c 'import secrets; print(secrets.token_urlsafe(32))')"
STRIPE_WEBHOOK_SECRET="whsec_$(python3 -c 'import secrets; print(secrets.token_hex(24))')"

cat > "$OUT_DIR/.env.production" <<EOF
ENGINE_TOKEN=${ENGINE_TOKEN}
STRIPE_WEBHOOK_SECRET=${STRIPE_WEBHOOK_SECRET}
NEXUS_TELEMETRY_ENABLED=true
NEXUS_TENANT_ID=${TENANT_ID}
SAAS_DB_PATH=${OUT_DIR}/kubecost_saas.db
EOF

PYTHONPATH=engine python3 -c "
from kubecost_engine.bootstrap import get_production_readiness_report, generate_nexus_rls_telemetry_ddl
from kubecost_engine.persistence import generate_timescaledb_migrations
import json

rep = get_production_readiness_report()
with open('${OUT_DIR}/readiness_report.json', 'w') as f:
    json.dump(rep, f, indent=2)
with open('${OUT_DIR}/01_timescaledb_and_nexus_rls.sql', 'w') as f:
    f.write('-- TimescaleDB + Nexus Client Portal RLS Schema\n')
    for stmt in generate_timescaledb_migrations(90) + generate_nexus_rls_telemetry_ddl():
        f.write(stmt.strip() + '\n\n')
print('Certified subsystems:', rep['certified_subsystems'], '/', rep['total_subsystems'])
"

echo "[OK] Production artifacts generated in ${OUT_DIR}"
echo "To deploy to Kubernetes cluster:"
echo "  helm upgrade --install ${RELEASE} ./deploy/helm/kubecost-auto-hpa -n ${NAMESPACE} --create-namespace -f ./deploy/helm/kubecost-auto-hpa/values-production.yaml"
