# Gate de Release Candidate

No etiquetar ni publicar v0.2.0 hasta cerrar los pendientes aplicables. Esta rama no despliega producción.

Seguimiento local del 2026-11-19: [resultados y pendientes](reports/2026-11-19_CIERRE_LOCAL.md).
El resultado local no sustituye CI remoto, pruebas prolongadas ni aceptación productiva.

La [revisión adicional de seguridad](reports/2026-11-19_REVISION_SEGURIDAD_FUNCIONAL.md)
registra los fallos reproducidos, sus correcciones y los nuevos controles HTTP,
TLS, de recomendaciones, de importación financiera y del recolector.

- [x] Rama independiente; baseline y diferencias registradas.
- [x] Eliminar éxito/ahorros ficticios de la consola principal y dashboard API.
- [x] Pruebas de módulos empresariales, API HTTP real y navegador local.
- [x] Correcciones del planificador, scopes, tenant admin, ACK persistente y drenado protegido.
- [x] Laboratorio Kind: resize sin reinicio y ciclo Helm, con límites documentados.
- [x] Pipeline incorpora cobertura, navegador, Redis y laboratorio con artefactos.
- [ ] Pipeline remoto de los cambios actuales y branch protection confirmados. El historial público del commit base contiene una corrida fallida en `operational-smoke`; sus logs requieren autenticación.
- [ ] Validación de evacuación asistida en GKE y AKS, recuperación tras fallo del proceso y ejecución automática. La evacuación asistida de Deployments sin estado ya se prueba en Kind con un PDB.
- [x] PostgreSQL real, usuarios, retención y cuotas existentes verificados localmente sin omisiones.
- [x] Failover local con estado persistido y nuevas escrituras tras SIGTERM, SIGKILL y pérdida de la sesión PostgreSQL.
- [ ] HA productiva, recuperación de PostgreSQL y límites comerciales completos. El runtime sigue siendo de un solo escritor activo.
- [ ] Contrato OpenAPI exhaustivo de rutas heredadas y formularios empresariales completos.
- [ ] Validar los artefactos publicados de destino. El ensayo en Kind cubre `v0.1.0` y el chart de trabajo. La suite de persistencia cubre migración SQLite a PostgreSQL y restauración SQLite.
- [x] Repetir upgrade desde `v0.1.0`, rollback, conservación del PVC y resize sin reinicio con el árbol de trabajo local (2026-11-19).
- [ ] Compatibilidad/race/envtest en matriz completa y artefacto final.
- [ ] GKE y AKS independientemente aprobados; soak prolongado.
- [x] Escaneo del historial local con Gitleaks: 131 commits; dos falsos positivos revisados y excluidos por fingerprint exacto.
- [x] `govulncheck` del operador y recolector: ninguna vulnerabilidad alcanzable tras exigir Go 1.27.4 en el recolector.
- [ ] Pentest externo y cierre de hallazgos relevantes.
- [ ] SBOM/digests finales, firmas/provenance y rehearsal de publicación sin push.
- [x] SBOMs, IDs inmutables, hashes y filtro HIGH/CRITICAL corregible de las dos imágenes locales; ensayo sin publicación `v0.2.1` (2026-11-19).
- [ ] Piloto y conciliación financiera atribuible aceptados; benchmark sin afirmaciones no probadas.
- [ ] Contratos/DPA/privacidad/SLA revisados por abogados y soporte operativo financiado.

Reproducir verificación local: `python hack/verify_enterprise.py --browser`. Laboratorio: `python hack/local_enterprise_lab.py`. Los runners devuelven un código distinto de cero ante fallos; no convierten omisiones en éxito. El informe debe conservar reintentos y distinguirlos de una pasada limpia.
