# Retención, alertas y administración — 2026-11-17

`POST /v1/maintenance/retention` recibe `days` (1..3650) y `dry_run`
(booleano, true por defecto). Exige `maintenance:write`. La vista previa cuenta
los eventos elegibles; la ejecución elimina solo telemetría de la organización
autenticada con timestamp anterior al corte. Conserva IDs de deduplicación en
una tabla separada, registros financieros, comandos, licencias y auditoría.
La eliminación y conservación de IDs ocurren en una transacción. El cache de
telemetría se actualiza y se registra una entrada de mantenimiento.

`TELEMETRY_RETENTION_DAYS` habilita limpieza durante la ingesta, como máximo
una vez por hora por organización. Con licencia obligatoria se aplica el menor
plazo entre configuración y plan. Sin ingesta no hay worker periódico; puede
invocarse el endpoint desde un scheduler autenticado. La variable está apagada
por defecto. Los IDs conservados consumen espacio y requieren una política de
archivo independiente antes de poder acotar su crecimiento sin perder dedup.
La eliminación libera inventario derivado de esos eventos y sus cuotas.

`POST /v1/alerts/ack` exige `alerts:write`, id de alerta y versionado opcional
(0 para el primer reconocimiento). Se asocia a la observación que originó la
alerta. Una nueva observación vuelve a presentarse activa. La consola permite
reconocer alertas. Aún no hay envío externo ni ciclo completo de incidentes.

La consola agrega formularios con campos específicos para crear/actualizar
usuarios, emitir credenciales, configurar políticas y gestionar retención.
La limpieza abre con vista previa marcada. Desconectar limpia los resultados
que puedan contener credenciales. Las pruebas de navegador usan el backend
real para crear usuarios y verificar esa vista previa.

La migración SQLite → PostgreSQL incluye los IDs conservados. El esquema
PostgreSQL registra versión 2. Las pruebas cubren ambos motores, conservación
del límite temporal, aislamiento de organizaciones y dedup tras reinicio.

Evidencia de esta revisión: `reports/evidence/enterprise-maintenance/`.
Estos cambios no completan consolidación, HA, API heredada ni las pruebas de
actualización desde v0.1.0. Esos paquetes siguen abiertos en la matriz general.
