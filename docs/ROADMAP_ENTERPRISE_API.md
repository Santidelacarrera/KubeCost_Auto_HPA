# Roadmap basado en brechas verificadas

## Antes de declarar Release Candidate

1. Completar el ejecutor de consolidación: inventario obtenido del API server, huella y UID del snapshot, aprobación ligada al snapshot, revalidación, PDB real, verificación de reemplazos Ready, lease por clúster, recuperación de intención y uncordon solo cuando se pueda demostrar propiedad. Una evicción no puede deshacerse: rollback significa recuperar capacidad y disponibilidad, no resucitar el mismo Pod.
2. Implementar PostgreSQL como backend real de todas las entidades; migraciones versionadas, transacciones y lecturas consistentes entre réplicas. El DDL heredado no es una integración.
3. Integrar muestras históricas de métricas con los nuevos analizadores, backtesting temporal independiente, evaluación de intervalos y tolerancia documentada a falsos positivos.
4. Completar usuarios/roles, cuotas de nodos/workloads/usuarios, retención, revocación de licencias y adaptador de pagos de proveedor en sandbox autorizado.
5. Completar dashboard de alertas/políticas con formularios específicos, gráficos de métricas/costos, paginación uniforme y contrato OpenAPI de rutas heredadas.
6. Ejecutar envtest/race/compatibilidad, actualización desde el artefacto v0.1.0 y restauración real de datos; mantener checks obligatorios en el repositorio remoto.

## Validación autorizada posterior

GKE y AKS se validan por separado con [INFRASTRUCTURE_VALIDATION](INFRASTRUCTURE_VALIDATION.md). Secuencia: Observe-only → Assisted → Automated en laboratorio → staging → carga controlada → recuperación → soak. Se detiene el avance al incumplir disponibilidad, PDB, integridad o límites.

Piloto Observe-only de dos semanas, conciliación de costos reales, benchmark controlado y revisión externa de seguridad/legal conforme a [COMMERCIAL_READINESS](COMMERCIAL_READINESS.md).

AWS no se añade al alcance. No se publica v0.2.0 ni se cambia un entorno productivo como parte de este trabajo.

## Seguimiento 2026-11-17

PostgreSQL implementado y probado; falta HA del runtime. Usuarios/roles y cuotas implementados; falta retencion. Backtesting temporal y origen de telemetria persistida disponibles. Ver [detalle](FOLLOWUP_IMPLEMENTATION.md).
