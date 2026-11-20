# Cierre técnico local — 2026-11-19

Base: `86b6e916165af9431363e8579e78cd14143f1124`, con cambios locales.
Estado: **Beta; no Release Candidate**. No se publicaron versiones ni se
modificaron recursos de nube. GKE se consultó en modo de solo lectura.

## Cambios

- El verificador exige al menos 80% de cobertura, incluye recolector de auditoría,
  backup y benchmarks, cuenta omisiones y conserva fecha final y resultado.
  `--services` exige PostgreSQL y Redis; `--operational` activa las pruebas
  desechables Docker/Kind. En esos modos una omisión hace fallar la verificación.
- Cuatro pruebas del verificador cubren éxito, omisiones y fallos de procesos.
- Las pruebas de recuperación cubren SIGTERM, SIGKILL y pérdida de la sesión
  PostgreSQL. Verifican datos previos y nuevas escrituras tras el relevo.
- CI conserva JUnit de las pruebas operativas y ejecuta las pruebas del
  verificador. Supply-chain incorpora un escaneo del historial con Gitleaks.
- El recolector exige Go 1.27.4: el escaneo inicial encontró GO-2026-6859 al
  compilar con Go 1.27.3. El escaneo posterior no encontró vulnerabilidades
  alcanzables. Permanecen avisos en dependencias que el análisis no considera
  alcanzables; esto no equivale a un pentest.
- El ensayo de publicación acepta las dos variantes del error de referencia
  OCI de Helm y exige conexión rechazada al puerto local cerrado.
- `hack/verify_release_artifacts.py` genera SBOMs y escaneos locales, registra
  IDs inmutables de imagen y hashes de evidencia con un scanner fijado por digest.

## Verificación ejecutada

| Comprobación | Resultado |
|---|---|
| Suite integrada Python, PostgreSQL, Redis, navegador, backup y benchmarks | 168 passed, 0 skipped; cobertura 83,47% |
| Verificador final con `--browser --services` | PASS: 158 tests de motor, 8 de backup/benchmarks, 1 navegador, 1 Redis; cobertura del motor 83,14% |
| Pruebas nuevas del verificador | 4 passed |
| Operador Go | 32 paquetes con pruebas pasaron; `go vet` limpio |
| Recolector Go | Compilación mediante `go test` y `go vet` limpios; el módulo no tiene pruebas unitarias |
| Evacuación protegida por PDB en Kind, Alertmanager y relevo SIGTERM | 3 passed |
| Relevo ampliado con SIGTERM, SIGKILL y pérdida de sesión | 3 passed; estado conservado y nuevas escrituras verificadas |
| Laboratorio Kind | Upgrade `v0.1.0` → árbol de trabajo, rollback, PVC retenido, resize sin recreación y 0 reinicios |
| Helm, JavaScript y Ruff | PASS |
| Ensayo de publicación `v0.2.1` | PASS; solo ensayo, sin etiqueta ni publicación |
| `govulncheck` operador y recolector corregido | 0 vulnerabilidades alcanzables |
| SBOMs y escaneo Trivy 0.76.2 de ambas imágenes locales | PASS: 73 componentes operador, 94 motor; 0 HIGH/CRITICAL con corrección disponible |
| Gitleaks 8.24.3 | 131 commits; 2 falsos positivos revisados; segunda corrida sin hallazgos restantes |
| GKE 1.36.4 | Preflight de solo lectura completado; no certifica aceptación productiva |
| AKS | Endpoint no resuelve DNS; no validado |

Los dos falsos positivos de Gitleaks son una clave HMAC sintética del test local
de webhook y una declaración de variables Go. `.gitleaksignore` solo excluye
sus fingerprints exactos, sin excluir archivos, directorios ni reglas completas.

## Evidencia y reintentos

Los logs locales están en `.local-validation/closure-20261119/`, la corrida
final en `.local-validation/closure-final-20261119/` y el laboratorio en
`.local-validation/closure-lab-20261119/`. Los artefactos de imagen se generan
en `.local-validation/closure-artifacts-20261119/`. No se agregan credenciales,
bases de datos ni caches al repositorio.
El [resumen versionable](evidence/closure-20261119/summary.json) registra resultados,
IDs de imágenes, scanner por digest y hashes SHA-256 de los cuatro artefactos.

La primera suite registró 1 fallo de conexión HTTP en `test_bearer_token` y
10 omisiones por servicios/OpenSSL no configurados. El mismo test pasó ocho
veces de forma aislada; las corridas integrada y final pasaron con los servicios
y OpenSSL disponibles. No se estableció la causa del fallo intermitente y no
se ocultó ni se convirtió su registro en éxito. El primer ensayo de publicación
falló por el texto del error OCI de Helm; la corrida posterior a la corrección pasó.

## Pendientes concretos

1. **CI remoto:** el historial público del commit base tiene corridas exitosas
   y una fallida en `operational-smoke` (run `44408759475`). Las anotaciones
   públicas solo informan exit 1. Descargar logs y comprobar branch protection
   requiere autenticación; `gh` no tiene sesión. Las pruebas locales no cierran
   ese fallo remoto. Los cambios actuales todavía no tienen corrida remota.
2. **Infraestructura:** AKS accesible, aceptación de GKE/AKS con escritura,
   soak prolongado, carga real y recuperación ante caída del API server.
3. **HA:** particiones de red, recuperación del propio PostgreSQL y operación
   prolongada. El motor sigue limitado a un escritor; no hay active-active.
4. **Finanzas:** semanas de tráfico y exportaciones reales antes/después para
   backtesting y ahorro atribuible conciliado con factura.
5. **Stripe:** no hay claves ni catálogo de Stripe configurados en este entorno.
   Los tests existentes pasan con proveedor simulado; falta compra sandbox real,
   provisión de licencia y conciliación de facturas recurrentes.
6. **Producto:** contrato exhaustivo de rutas heredadas y recuperación de una
   evacuación interrumpida después de reclamar el plan. El ejecutor exige
   inspección manual en ese caso. La automatización productiva sigue pendiente.
7. **Release:** validación de artefactos publicados, firmas/provenance de release
   y matriz final remota. Los SBOMs locales no sustituyen esos pasos.
8. **Externos:** pentest independiente, revisión legal, soporte y aceptación
   de un cliente piloto.

Se conservaron los dos archivos sin seguimiento que ya estaban presentes:
`engine/kubecost_engine/handoff_proxy.py` y una captura de diff con nombre
accidental. El proxy no tiene referencias desde el runtime; no se activó.
Las imágenes locales incluyen el árbol de trabajo y no representan una release limpia.

## Reproducción

Configurar `KCA_TEST_POSTGRES_DSN` y `TEST_REDIS_URL` con servicios locales
aislados y poner OpenSSL en PATH. Ejecutar:

```sh
python hack/verify_enterprise.py --browser --services --operational --output .local-validation/NEW
python hack/local_enterprise_lab.py --output .local-validation/NEW-lab
bash hack/release-dry-run.sh v0.2.1
python hack/verify_release_artifacts.py --output .local-validation/NEW-artifacts
```

Los nombres `NEW*` deben ser nuevos para conservar la evidencia de cada intento.
