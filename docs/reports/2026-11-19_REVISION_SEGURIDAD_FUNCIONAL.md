# Revisión adicional de seguridad y funcionalidad — 2026-11-19

Seguimiento del [cierre local](2026-11-19_CIERRE_LOCAL.md). Se conservan los
cambios anteriores y los archivos ajenos sin seguimiento. Estado: **Beta**.

## Fallos reproducidos y corregidos

| Área | Antes | Ahora |
|---|---|---|
| Archivos estáticos | La comparación por prefijo permitía leer un hermano como `web-private`; también publicaba `.env`, bases o claves colocadas en `WEB_DIR` | Compara componentes de rutas resueltas, incluye symlinks, rechaza componentes ocultos y extensiones ajenas a los assets |
| Framing HTTP | Aceptaba longitudes duplicadas, `Transfer-Encoding` junto con longitud, `+longitud` y cuerpos truncados | Rechaza framing ambiguo; exige recibir la longitud declarada completa |
| Identidad | Usaba solo el primer header de credenciales y el primer valor repetido de tenant | Rechaza headers de identidad repetidos y verifica todos los valores de tenant |
| JSON | Aceptaba claves repetidas, conservando silenciosamente el último valor | Rechaza claves repetidas también en objetos anidados |
| Conexiones lentas | Cola de trabajo ilimitada; sin timeout de lectura; handshake TLS dentro del acceptor | Capacidad limitada antes de encolar, timeout de I/O y handshake en un worker con capacidad reservada |
| Configuración TLS | Una pareja incompleta de certificado/clave podía dejar HTTP plano | Falla antes de abrir el puerto; Helm también rechaza cliente mTLS sin servidor TLS |
| Recomendaciones empresariales | Podía bajar límites de CPU/memoria e ignorar señales OOM de tipos incorrectos | Conserva los límites existentes y exige booleanos cuando se aporta la señal OOM |
| Importación financiera | Aceptaba dimensiones de tipos inválidos y códigos de moneda Unicode; un exponente extremo escapaba del reporte por fila | Valida dimensiones, exige formato ASCII de moneda y rechaza importes enormes sin desbordamiento decimal |
| Recolector de auditoría | Espera del escritor sin cancelación y concurrencia sin cota | Máximo 32 eventos activos, espera cancelable y plazos de lectura/escritura/headers; retención positiva |

La reproducción inicial registró **11 fallos HTTP** y **10 fallos de seguridad
funcional** antes de aplicar las correcciones. No son hallazgos de un pentest
externo. La lectura de archivos requiere que el servidor tenga un directorio web
disponible y permisos para leer el archivo objetivo.

## Comprobaciones

- Suite del motor: **189 passed, 0 skipped; 83,37% de cobertura**.
- Verificador completo `--browser --services --operational`: **PASS**. Incluye
  12 pruebas de benchmarks/backup/verificador, navegador, Redis y las cinco
  operativas (PDB, Alertmanager y tres formas de relevo del motor), sin omisiones.
- Contrato API y límites HTTP tras actualizar OpenAPI: **30 passed**.
- Módulo empresarial: **38 passed**, incluidos los diez casos nuevos.
- Recolector: pruebas con backend HTTP local de S3 verifican headers de Object
  Lock, no confirmar antes del almacenamiento, error del almacén y cancelación
  de un evento en espera. `go test`, `go vet` y `govulncheck` pasaron.
- `go test -race -count=1 ./...` del recolector pasó en Linux con Go 1.27.4.
- Los workflows pasaron `actionlint`.
- Helm transmite los límites configurados, rechaza cuatro valores fuera de
  rango y rechaza mTLS incompleto. Los valores de producción pasan `helm lint`.
- Se reconstruyó `kubecost-auto-hpa-engine:security-20261119` después de los
  cambios. SBOMs y escaneos de ambas imágenes pasaron el filtro de
  HIGH/CRITICAL con corrección disponible. Scanner fijado por digest.

La verificación completa y sus resultados están en
`.local-validation/security-20261119/`; las pruebas previas al arreglo en
`.local-validation/http-boundaries-before.xml` y
`.local-validation/functional-safety-before.xml`. Los artefactos de imagen están
en `.local-validation/security-artifacts-20261119/`. Estos directorios están
excluidos de Git; el resumen de evidencia se conserva junto a este informe.
Resumen versionable con resultados, imágenes y hashes:
[evidence/security-20261119/summary.json](evidence/security-20261119/summary.json).

## Comportamiento y límites

Los límites se configuran con `engine.http.maxWorkers` y
`engine.http.socketTimeoutSeconds`; equivalen a `MAX_WORKERS` y
`HTTP_SOCKET_TIMEOUT_SECONDS`. Una conexión sobrante recibe 503 cuando es
posible o se cierra. El timeout limita inactividad, no tiempo total ni CPU.
El backend no acepta cuerpos chunked: el proxy/cliente debe enviar una única
longitud explícita. Los endpoints y su autenticación conservan sus contratos
de negocio, salvo el rechazo de solicitudes ambiguas o malformadas.

Las pruebas de S3 usan un backend local de contrato: no demuestran retención
WORM física en un proveedor. Esa evidencia sigue teniendo su alcance propio
en los laboratorios anteriores. La API de recomendaciones calcula propuestas;
estos cambios no habilitan escritura automática de Kubernetes.

Quedan pendientes el contrato completo de rutas heredadas, la recuperación
asistida de evacuaciones interrumpidas, CI remoto autenticado, aceptación
productiva en nube, facturas y tráfico real, pentest independiente y revisión
legal. El relevo de procesos conserva un solo escritor y no constituye HA del
servidor PostgreSQL. No se publicó ni se desplegó esta revisión en la nube.
