# Avance adicional — 2026-11-17

Se implementaron y probaron cambios adicionales en `enterprise-completion-v2`.
El producto sigue en desarrollo; los paquetes parciales de la matriz conservan
su estado hasta completar todos sus requisitos.

## Persistencia

`POSTGRES_DSN` selecciona el backend psycopg instalado con `engine[runtime]`.
Todas las entidades del store tienen almacenamiento PostgreSQL: telemetría,
comandos, auditoría, claves, idempotencia de eventos, contadores y objetos
empresariales. Las migraciones tienen versión y lock transaccional. Las pruebas
usan PostgreSQL 17 real en Docker y conexiones independientes: CAS concurrente,
100 incrementos sin pérdida, rollback financiero y recuperación.

El runtime conserva caches en memoria y requiere un único proceso activo.
Un advisory lock de sesión impide arrancar otro control plane sobre la misma
base. Esto protege la consistencia; no constituye HA ni failover validado.
Helm usa Recreate, admite `engine.postgres.secretName` y `key`, y exige una réplica.
El DSN debe usar TLS verificado para servidores remotos y permisos mínimos.
El esquema runtime no instala TimescaleDB ni afirma aplicar RLS.

Para migrar: detener el servicio de origen, guardar un backup verificable,
configurar POSTGRES_DSN a una base vacía y ejecutar
`python hack/migrate_sqlite_postgres.py ruta/control-plane.db`.
El migrador lee SQLite sin escritura, copia en una transacción, verifica conteos
y rechaza datos existentes. No realiza el cambio de servicio. Conservar el
origen para recuperación; después del cambio, restaurar un origen antiguo
perdería las nuevas escrituras de PostgreSQL.

## Usuarios y licencias

`GET/POST /v1/users`, `POST /v1/users/update` y `/v1/users/credentials`
administran usuarios con roles viewer/operator/approver/owner. Las credenciales
se muestran una vez. Deshabilitar un usuario invalida sus claves en cada
verificación; reducir su rol reduce permisos de claves existentes. Aumentar
un rol requiere emitir nuevas claves para obtener permisos adicionales.
La rotación preserva la asociación al usuario. Las aprobaciones usan identidad
de usuario, incluso con varias claves. Usuarios vinculados deben emitir nuevas
credenciales mediante el endpoint de usuarios. La gestión exige users:manage y
no permite delegar permisos ausentes. No incluye contraseña, SSO ni MFA.

Licencias: activación idempotente, renovación con fechas crecientes, revocación
con motivo/versionado e imposibilidad de reactivar el mismo identificador.
Activación y revocación exigen licenses:manage. REQUIRE_LICENSE=true aplica
cuotas de usuarios activos, clusters e inventario durable de nodos/workloads.
El inventario procede de telemetría suministrada: no certifica el inventario
completo del API server. Sigue pendiente el retiro de inventario y la retención
automática, por lo que recursos históricos pueden consumir cuota.

## Análisis y consola

Los endpoints de rightsizing/predicción aceptan `source: telemetry` con
cluster_id, workload, namespace, pod_name y container_name. Consumen observaciones
persistidas de ese contenedor, validan conflictos y registran hash/procedencia.
La identidad fija evita sumar réplicas incompletas. Para rightsizing se mantiene
el objeto current explícito; este origen todavía no recoge OOM.

`POST /v1/predictions/backtest` ejecuta ventanas de entrenamiento crecientes
sin incluir datos futuros. Reporta MAE, baseline ingenuo, cobertura empírica e
índices temporales; máximo 100 folds. `GET /v1/predictions/backtests` lista
evaluaciones. Los intervalos siguen siendo heurísticos.

`GET /v1/alerts` deriva alertas de heartbeat y anomalías recientes. La consola
presenta estas alertas, usuarios, evaluaciones y gráficos accesibles de CPU,
memoria, predicción/observación y costos por moneda. Los gráficos representan
la página/filtro actual. Siguen pendientes formularios completos y ciclo de
notificación/ack de alertas.

## Evidencia y pendientes

131 pruebas aprobadas, cero omitidas, cobertura 85,80 %. Helm y lint aprobados.

Resultados actuales: [JUnit](reports/evidence/enterprise-followup/python.xml),
[log](reports/evidence/enterprise-followup/python.log) y
[cobertura](reports/evidence/enterprise-followup/coverage.json).
CI incorpora un servicio PostgreSQL y pruebas de migración/concurrencia.
La prueba de navegador verifica navegación, gráficos y tamaño móvil contra
el servidor real. La política valida booleanos y mantiene spot drain apagado
cuando no se solicita explícitamente.

Faltan el ejecutor integral de consolidación, HA del runtime, retención,
contrato exhaustivo de API heredada, upgrades desde v0.1.0 y validación de fallos
completa. GKE/AKS, facturas/piloto/pagos de proveedor y revisiones externas
mantienen sus requisitos de infraestructura y terceros. No se declara 100%.

Referencia de transacciones usada para el backend:
[documentación oficial psycopg](https://www.psycopg.org/psycopg3/docs/basic/transactions.html).

## Retention and administration follow-up (2026-11-17)

[Implementation and limits](MAINTENANCE_IMPLEMENTATION.md). Latest suite: 139 passed, no skips, 85.93% coverage; real PostgreSQL and browser. Consolidation executor and runtime HA remain incomplete.
