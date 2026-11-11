$ErrorActionPreference = "Stop"

# Zero-Touch Production Bootstrap for KubeCost_Auto_HPA (Windows PowerShell)
$OutDir = Join-Path $PSScriptRoot "..\.prod-bootstrap"
New-Item -ItemType Directory -Force -Path $OutDir | Out-Null

$env:PYTHONPATH = Join-Path $PSScriptRoot "..\engine"
python -c @"
import json, os, secrets
from kubecost_engine.bootstrap import get_production_readiness_report, generate_nexus_rls_telemetry_ddl
from kubecost_engine.persistence import generate_timescaledb_migrations

out_dir = r'$OutDir'
rep = get_production_readiness_report()
with open(os.path.join(out_dir, 'readiness_report.json'), 'w', encoding='utf-8') as f:
    json.dump(rep, f, indent=2)
with open(os.path.join(out_dir, '01_timescaledb_and_nexus_rls.sql'), 'w', encoding='utf-8') as f:
    f.write('-- TimescaleDB + Nexus Client Portal RLS Schema\n')
    for stmt in generate_timescaledb_migrations(90) + generate_nexus_rls_telemetry_ddl():
        f.write(stmt.strip() + '\n\n')
print(f'Certified {rep[\"certified_subsystems\"]}/{rep[\"total_subsystems\"]} production subsystems ({rep[\"overall_status\"]})')
"@
