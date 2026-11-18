# Gate de Release Candidate

No etiquetar ni publicar v0.2.0 hasta cerrar los pendientes aplicables. Esta rama no despliega producción.

- [x] Rama independiente; baseline y diferencias registradas.
- [x] Eliminar éxito/ahorros ficticios de la consola principal y dashboard API.
- [x] Pruebas de módulos empresariales, API HTTP real y navegador local.
- [x] Correcciones del planificador, scopes, tenant admin, ACK persistente y drenado protegido.
- [x] Laboratorio Kind: resize sin reinicio y ciclo Helm, con límites documentados.
- [x] Pipeline incorpora cobertura, navegador, Redis y laboratorio con artefactos.
- [ ] Ejecución remota del pipeline y branch protection confirmadas.
- [ ] Validación de evacuación asistida en GKE y AKS, recuperación tras fallo del proceso y ejecución automática. La evacuación asistida de Deployments sin estado ya se prueba en Kind con un PDB.
- [ ] PostgreSQL/HA stateful, usuarios, retención y límites comerciales completos.
- [ ] Contrato OpenAPI exhaustivo de rutas heredadas y formularios empresariales completos.
- [ ] Validar los artefactos publicados de destino. El ensayo en Kind cubre `v0.1.0` y el chart de trabajo. La suite de persistencia cubre migración SQLite a PostgreSQL y restauración SQLite.
- [ ] Compatibilidad/race/envtest en matriz completa y artefacto final.
- [ ] GKE y AKS independientemente aprobados; soak prolongado.
- [ ] Escaneo de secretos, pentest externo y cierre de hallazgos relevantes.
- [ ] SBOM/digests finales, firmas/provenance y rehearsal de publicación sin push.
- [ ] Piloto y conciliación financiera atribuible aceptados; benchmark sin afirmaciones no probadas.
- [ ] Contratos/DPA/privacidad/SLA revisados por abogados y soporte operativo financiado.

Reproducir verificación local: `python hack/verify_enterprise.py --browser`. Laboratorio: `python hack/local_enterprise_lab.py`. Los runners devuelven un código distinto de cero ante fallos; no convierten omisiones en éxito. El informe debe conservar reintentos y distinguirlos de una pasada limpia.
