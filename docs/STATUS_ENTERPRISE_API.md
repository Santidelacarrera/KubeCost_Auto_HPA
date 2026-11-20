# Estado técnico — 2026-11-16

Rama: `enterprise-completion-v2`. Base: `716e0d7`. Única etiqueta disponible al iniciar: `v0.1.0`.
**Estado: desarrollo empresarial en curso; no Release Candidate.** AWS está fuera de esta fase.

## Seguimiento 2026-11-19

La [revisión posterior de seguridad y funcionalidad](reports/2026-11-19_REVISION_SEGURIDAD_FUNCIONAL.md)
corrige escapes del directorio web, parsing ambiguo de HTTP/JSON, espera y
concurrencia sin cota, TLS incompleto, límites de recomendaciones y validación
de filas financieras. El motor pasó 189 pruebas sin omisiones; conserva estado Beta.

La [verificación local](reports/2026-11-19_CIERRE_LOCAL.md) pasó con PostgreSQL,
Redis y navegador reales, sin omisiones. Kind verificó actualización desde
`v0.1.0`, rollback con PVC conservado y resize sin reinicio. La recuperación del
motor se probó ante SIGTERM, SIGKILL y pérdida de sesión PostgreSQL, conservando
estado y admitiendo nuevas escrituras. Sigue habiendo un solo escritor activo;
esto no valida HA productiva ni recuperación del servidor PostgreSQL.
La evacuación protegida por PDB y Alertmanager pasaron localmente.
GKE pasó un preflight de solo lectura; AKS no resolvió su endpoint DNS.
Los límites históricos que siguen abajo deben leerse con este seguimiento.

Este documento, el [informe de cierre](ENTERPRISE_COMPLETION_REPORT.md) y los logs actuales sustituyen las afirmaciones históricas de “paridad total”, “10/10” y “100% production ready”. Los documentos STATUS, MVP y ROADMAP no existían en la base: se crearon en esta intervención.

## Avance 2026-11-17

PostgreSQL real, usuarios y roles, cuotas de inventario, backtesting y telemetria persistida: [implementacion y limites](FOLLOWUP_IMPLEMENTATION.md). Este seguimiento sustituye las limitaciones de PostgreSQL y usuarios descritas en la linea base de abajo. El runtime mantiene un solo proceso activo. PostgreSQL admite réplicas en espera.

## Implementación disponible

- Operadores Go para KubeCostHPA y KubeCostAutoscaler, métricas, límites de seguridad, resize y arbitraje de propiedad existentes.
- Recomendaciones CPU/memoria con historial, percentiles, margen configurable, límites de reducción, picos y señal OOM; API persistente `/v1/rightsizing`.
- Predicciones con timestamps, ventanas, rechazo de muestras insuficientes/irregulares, evidencia de ciclos y registro de observación posterior. Los intervalos son heurísticos, no calibrados estadísticamente.
- Planificador de consolidación que respeta CPU/memoria reservadas, exclusiones, readiness, taints/tolerations, selector y zona; bloquea PVC, afinidades complejas y StatefulSets en vez de simular soporte del scheduler.
- Planes persistentes, caducidad y aprobación por otro principal. El ejecutor asistido puede cordonar un candidato vacío o evacuar hasta cinco pods de Deployments sin estado con un PDB compatible. El ensayo en Kind verificó el rechazo del PDB y la reubicación real de dos pods.
- Importadores GCP, Azure y subconjunto FOCUS con Decimal, errores por fila, deduplicación transaccional, monedas separadas, asignación y conciliación de importe facturado.
- Registro/heartbeat multi-clúster, recuperación SQLite de agentes/políticas/telemetría/comandos, aislamiento de organización y estado offline.
- Consola real en `web/app.html`; la interfaz histórica está en `web/demo.html`, rotulada como simulación.
- Licencias HMAC verificadas en servidor, expiración, planes, límite de clústeres y control de funcionalidades optativos con `REQUIRE_LICENSE=true`. Faltan usuarios, límites integrales y retención automática.
- Suscripciones de prueba locales: activación, renovación, cancelación e idempotencia. No otorgan derechos productivos ni realizan cobros.

## Validación

Ver [resultados reproducibles](reports/evidence/enterprise-v2/) y [laboratorio Kind](reports/evidence/enterprise-lab-final/acceptance.json).
El laboratorio observó resize sin recreación/reinicio, instalación/upgrade/rollback de Helm y PVC enlazado. Usó Prometheus de fixture: no prueba exactitud de métricas productivas ni ahorro financiero. Redis y navegador se probaron separadamente contra servicios reales locales.

## Límites materiales

SQLite y PostgreSQL implementan persistencia. PostgreSQL usa migraciones versionadas y transacciones probadas con una base real. El runtime requiere un solo proceso activo por sus caches en memoria; un lock de sesion impide un segundo escritor. Helm permite réplicas en espera cuando se configura PostgreSQL. Ver OPERATIONS.md.

No hay validación GKE/AKS, soak de varios días, pentest, certificación SOC 2/ISO 27001, cliente piloto, cobros sandbox de proveedor ni ahorro atribuible conciliado. La etiqueta v0.2.0 existe en el repositorio y apunta a una revisión anterior a estos cambios. El upgrade observado fue del chart de trabajo, no v0.1.0 → v0.2.0.

## Retention and administration follow-up (2026-11-17)

[Implementation and limits](MAINTENANCE_IMPLEMENTATION.md). Latest suite: 139 passed, no skips, 85.93% coverage; real PostgreSQL and browser. Consolidation executor and runtime HA remain incomplete.

## Scheduled retention follow-up

See [LOCAL_COMPLETION_V3](LOCAL_COMPLETION_V3.md) for server-owned retention of idle tenants and payment boundary hardening. Remaining HA and consolidation limits still apply.
