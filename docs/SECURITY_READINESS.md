# Seguridad y preparación externa

Este documento complementa [THREAT_MODEL](THREAT_MODEL.md), [RBAC](RBAC.md), [SUPPLY_CHAIN](SUPPLY_CHAIN.md) y SECURITY.md. No es un pentest ni una certificación.

## Correcciones verificables

- Tenant-bound API keys no adquieren acceso global por poseer `admin:*`; la delegación de scopes no puede exceder al emisor.
- Escrituras de política, comandos, agentes, telemetría y análisis exigen scopes; las aprobaciones requieren un principal distinto y versión vigente del plan.
- Sin ejecución simulada desde la UI; ausencia de evidencia financiera se representa con null.
- El drenado consulta protecciones antes de cordon, bloquea almacenamiento/stateful no aprobado y liga la evicción al UID del pod. PDB se respeta mediante Eviction API; no se utiliza delete para evadirlo.
- Modo desconocido no activa escrituras; dryRun prevalece sobre Canary. El modo vacío conserva semántica heredada del flag dryRun: configurar explícitamente Observe/DRY_RUN al instalar workloads.
- Spot drain desactivado por defecto; la cuenta del motor no monta token Kubernetes. SQLite persistente admite una réplica y Recreate; no se afirma HA stateful.
- El perfil productivo deja deshabilitado el webhook HTTP heredado hasta implementar/validar TLS de admisión.
- Persistencia del ACK y protección contra reutilizar command_id; CAS en aprobaciones y transacción de registros financieros.

## Dependencias e imágenes

Se detectó GO-2026-6859 en Go 1.27.3 y se cambió el mínimo/Dockerfile a 1.27.4. El reescaneo no encontró vulnerabilidades alcanzables; informó además dos vulnerabilidades en módulos requeridos sin llamadas detectadas. Esto no equivale a ausencia universal de riesgo. Fuente: [aviso Go](https://pkg.go.dev/vuln/GO-2026-6859), [releases](https://go.dev/doc/devel/release).

Trivy detectó inicialmente CVE-2026-85091 en zlib de la imagen Python. Se actualizaron paquetes de sistema al construir; consultar logs `image-*-scan-fixed.log`. El criterio de esos scans es HIGH/CRITICAL con corrección disponible; no incluye automáticamente todas las severidades o vulnerabilidades sin parche. Repetir al cambiar dependencias/base y conservar digest/SBOM. No ocultar hallazgos iniciales.

## Riesgos y gates abiertos

| Superficie | Riesgo | Gate |
|---|---|---|
| Consolidación | Snapshot incompleto, scheduler, PDB dinámico, reemplazos no Ready | Implementar y validar ejecutor fail-closed antes de Automated |
| HA API | Caché en proceso y SQLite | Un solo proceso; PostgreSQL real antes de varias réplicas |
| Auditoría | Base local editable por administrador | Retención y exportación append-only externa con control de acceso |
| Licencias | Firma HMAC compartida solo en servidor | Secret manager, rotación/revocación y límites completos antes de comercializar |
| Webhook | Servidor heredado sin TLS de admisión | Mantener deshabilitado |
| Billing | Metadata suministrada no autentica factura ni atribución | Evidencia financiera y revisión humana |
| Históricos | Crecimiento de datos y recuperación de eventos legacy | Migración/retención con pruebas y backup |
| Secretos | Necesidad de revisión del repositorio y artefactos | Escaneo dedicado y revisión externa pendientes |

## Paquete para pentest/SOC 2/ISO 27001

Entregar al especialista: arquitectura y flujos de confianza, inventario de endpoints y scopes, amenazas STRIDE, RBAC renderizado, imágenes/digests/SBOM, logs de pruebas, gestión de secretos, evidencia de aislamiento y casos negativos, backups y política de cambios. Acordar alcance, horario, entorno sintético y reglas de detención. Probar BOLA/IDOR incluyendo admin tenant, escalamiento de scopes, SSRF/proxies, agotamiento de recursos, replay, carreras de aprobación, expiración y recuperación.

Preparar evidencia organizacional aún ausente: revisiones de acceso, onboarding/offboarding, proveedores, formación, inventario de activos, riesgo, continuidad y revisión directiva. Los marcos no se satisfacen únicamente con código.

Incidentes: contener escrituras (pausa/emergency-stop), preservar logs/digests, revocar credenciales comprometidas, evaluar impacto, informar al contacto contractual, restaurar desde evidencia validada y documentar causa/acciones. Plazos legales de notificación y obligaciones de DPA deben definirlos los responsables legales.
