# KubeCost Auto-HPA: informe de la duodécima pasada (2026-11-15)

Continúa `2026-11-09_KUBECOST_AUTOHPA_ENGINEERING_FINAL_REPORT.md` (pasadas 1 a 11). Esta pasada respondió a la misión
"llevar el producto a nivel empresarial" con una regla: auditar contra el código, no contra los documentos.
Nada se publicó, etiquetó ni desplegó. No se creó infraestructura en la nube ni se incurrió en costos.

**Clasificación final: Beta.**

## 1. Resumen ejecutivo

El repositorio tenía dos mitades que se contradecían. Una mitad de ingeniería, medida y honesta (recomendador, política,
aplicación con rollback, evidencia en GKE y AKS reales). Y una mitad comercial que afirmaba cosas que ningún código ni
medición respaldaba: "paridad total con ScaleOps", "35% menos gasto", "certificación" de carga, garantías de p99, cobro con
Stripe, una consola con cifras inventadas presentadas como reales, y AWS como plataforma.

Esta pasada hizo tres cosas:

1. **Auditó y clasificó** cada funcionalidad (docs/STATUS.md). Encontró cuatro defectos reales, uno de seguridad.
2. **Implementó** lo que faltaba y era verificable aquí: conciliación de facturas con niveles de ahorro, tendencia y
   anomalías en el recomendador, análisis de nodos con restricciones de scheduling, arnés de benchmarking.
3. **Alineó lo que el producto dice con lo que hace**: README, landing, consola, modelo comercial y 16 borradores de ventas.

## 2. Hallazgos de la auditoría (defectos reales)

| # | Hallazgo | Gravedad | Corrección |
|---|---|---|---|
| 1 | `kubectl apply -k operator/config/default` instalaba el operador **sin** `--observe-only`: podía escribir (resize, réplicas, drenado Spot). Solo el chart tenía el valor seguro | Alta (seguridad operativa) | La base de Kustomize arranca en observe-only; CI lo comprueba |
| 2 | `KubeCostAutoscaler` aceptaba `spec.predictive`, `spec.oomProtection` y `spec.nodeBinPacking` y los ignoraba en silencio | Media (el usuario cree tener una protección que no existe) | Condición `UnsupportedFields=True` + evento Warning; descripción del CRD dice NOT IMPLEMENTED |
| 3 | Los valores `features.*` del chart no los leía ninguna plantilla | Baja | Marcados como obsoletos y sin efecto |
| 4 | Endpoints del motor (`/v1/cast/*`, GPU, GreenOps, GameDay, topología, precios, "factura") devolvían datos de ejemplo sin decirlo; la "factura" tomaba el ahorro de un parámetro de la URL y lo llamaba verificado | Media (datos de demostración presentados como reales) | Cabecera `X-KubeCost-Data-Source: sample` y campo `data_source`; la factura dice `simulation: true`, `charge_created: false` |
| 5 | `internal/castsuite`, `internal/gpu`, `internal/psi`: paquetes con pruebas que ningún binario usa | Baja (código muerto) | Documentado; decisión pendiente (backlog 8) |
| 6 | La versión v0.2.0 figuraba como publicada; solo existe la rama `release/v0.2.0`, sin etiqueta | Baja | Documentado en UPGRADE.md y status.json |

## 3. Arquitectura final

Sin cambios estructurales: operador Go (controladores `KubeCostAutoscaler`, `KubeCostHPA`, aplicador opt-in, drenado Spot),
motor Python (`POST /v1/evaluate`), CLI `rightsize`, Redis como caché. Estado en el API de Kubernetes; ninguna llamada a APIs
de nube; la facturación entra solo como archivo. Diagramas de componentes, flujo de ejecución y origen del ahorro en
docs/ARCHITECTURE.md.

## 4. Funcionalidades implementadas en esta pasada

| Funcionalidad | Archivos | Verificación |
|---|---|---|
| Niveles de ahorro (estimado, proyectado, observado, conciliado) y `rightsize reconcile` | `internal/cost/reconcile.go`, `cmd/rightsize/reconcile.go` | 11 pruebas + 1 de CLI; los totales corren sobre la exportación real de Google del repositorio |
| Tendencia de crecimiento con pronóstico, detección de picos, riesgo de OOM, señales | `internal/recommend/insights.go` | 12 pruebas |
| Análisis de nodos con taints, tolerations, nodeSelector, volúmenes zonales, anti-afinidad, PDB, zonas; pods pendientes y presión | `internal/capacity/capacity.go`, `cmd/rightsize/inventory.go` | 11 + 2 pruebas (clúster simulado) |
| Condición `UnsupportedFields` | `internal/controller/kubecostautoscaler_controller.go` | 2 pruebas |
| Etiquetado de datos de ejemplo en el motor | `engine/kubecost_engine/server.py`, `auth_billing.py` | 8 pruebas |
| Arnés de benchmarking | `benchmarks/` | 7 pruebas; manifiestos válidos como YAML; **no ejecutado en un clúster** |
| Panel de estado verificado y aviso de datos demo en la consola | `web/app.html`, `web/status.json` | Cargada en un navegador: el panel muestra 4 tarjetas desde `status.json` |

## 5. Archivos creados y modificados

Creados (código): `operator/internal/cost/reconcile.go`, `reconcile_test.go`; `operator/internal/recommend/insights.go`,
`insights_test.go`; `operator/internal/capacity/constraints_test.go`; `operator/cmd/rightsize/reconcile.go`,
`inventory_test.go`; `operator/internal/controller/kca_unsupported_test.go`; `engine/tests/test_sample_data_labelling.py`;
`benchmarks/` (bench.py, scenarios.json, test_bench.py, traces/, README, results/README); `web/status.json`.

Creados (documentación): `docs/README.md`, STATUS, INSTALL, CONFIGURATION, GKE, AKS, NODES, FINOPS, API, CRD, UPGRADE,
ROLLBACK, SUPPORT, KNOWN_LIMITATIONS, BENCHMARKING y este informe.

Modificados: `operator/internal/capacity/capacity.go`, `recommend/recommend.go`, `cmd/rightsize/inventory.go`, `main.go`,
`controller/kubecostautoscaler_controller.go`, `api/v1alpha1/kubecostautoscaler_types.go`, los dos CRD generados,
`operator/config/manager/manager.yaml`, `overlays/local/kustomization.yaml`, `deploy/rbac/rightsize.yaml`,
`deploy/helm/kubecost-auto-hpa/values.yaml`, `engine/kubecost_engine/server.py`, `auth_billing.py`, `web/app.html`,
`web/index.html`, `.github/workflows/ci.yml`, `README.md`, `CHANGELOG.md`, `docs/COMMERCIAL.md` (reescrito), ROADMAP, RBAC,
RECOMMENDER, COST, ARCHITECTURE, y 16 borradores de ventas y plantillas legales (aviso inicial).

## 6. Pruebas ejecutadas y resultados reales

Windows 11, Go 1.27.3, Python 3.12.10, Helm 3.16.3. Sin `-race` en local (no hay cgo); CI lo ejecuta.

| Comando | Resultado |
|---|---|
| Base antes de tocar nada: `gofmt`, `go vet`, `go test -count=1 -p 2 ./...` | PASS, 35 paquetes |
| Base: `pytest` (motor) | PASS, 82 |
| Final: `gofmt -l .`, `go vet ./...`, `go test -count=1 -p 2 ./...` | PASS: gofmt y vet limpios, 35 paquetes; pruebas con la etiqueta integration del paquete controller, PASS |
| Final: `pytest` (motor) | PASS, 90 |
| `pytest benchmarks` | PASS, 7 |
| `helm lint` (valores por defecto y de producción), `helm template` | PASS |
| `kubectl kustomize` de la base y los tres overlays | PASS; la base contiene `--observe-only`, los overlays de prueba no |
| `controller-gen` (CRD regenerados) | Generados; solo cambia la descripción de tres campos |
| Consola en navegador | Panel de estado con 4 tarjetas; pestañas GKE y AKS |

No ejecutado en esta pasada: nada en un clúster real (no se creó infraestructura), envtest, e2e en kind, `-race`, CI de GitHub
sobre estos cambios. Las tres pruebas nuevas que fallaron al escribirlas fallaban por supuestos del propio test (tope de
reducción del 50%) y un detalle de serialización; se corrigieron sin relajar ninguna comprobación.

## 7. Vulnerabilidades y riesgos de seguridad encontrados y corregidos

* Instalación por Kustomize con escritura habilitada por defecto (hallazgo 1). Corregido y vigilado en CI.
* Datos de ejemplo y una "factura" simulada sin etiquetar (hallazgo 4). Corregido.
* Sin cambios en RBAC del operador. El rol de lectura del CLI gana `list persistentvolumes` (documentado; opcional).
* `govulncheck` no se ejecutó en local en esta pasada; corre en CI. La nota de la pasada 11 sigue vigente: la cadena de
  herramientas local 1.27.3 tiene GO-2026-6859, corregido en 1.27.4; las imágenes se construyen con el último parche.
* No hay auditoría externa ni pruebas de penetración. El white paper de seguridad ahora lo dice.

## 8. Estado de GKE

**Validado parcialmente; pendiente de validación para producción.** Ejecutado en GKE 1.36.4 real (pasada 6): comprobaciones
de solo lectura 9/9; recomendar, aprobar, aplicar, observar y revertir 11/11; un nodo liberado por el autoescalador
(0,067 USD/h a precio de lista); emparejamiento de nodos con filas FOCUS reales de un día. Pendiente: informe del soak de 7
días (2026-11-17), chart y aplicador en GKE, conciliación con factura (la cuenta está en créditos), Autopilot (no soportado).
En esta pasada no se ejecutó nada en GKE. Detalle y permisos: docs/GKE.md.

## 9. Estado de AKS

**Validado parcialmente; pendiente de validación para producción.** Ejecutado en AKS 1.36.5 real (pasadas 8 y 9): mecánica
14/15 dos veces, aplicador 27/27, ciclo de vida de Helm 4/4, un nodo liberado (0,096 USD/h a precio de lista), OpenCost fresco
2 h. Pendiente: exportación FOCUS real de Azure (el adaptador solo vio fixtures), corrida de varios días, conciliación con
factura. En esta pasada no se ejecutó nada en AKS. Detalle y permisos: docs/AKS.md.

AWS/EKS: fuera de alcance. No se implementó nada. Lo que quedaba de trabajo anterior (etiquetas de Spot de EKS/Karpenter,
filas de precios) se dejó, sin soporte, y se explica por qué en docs/STATUS.md.

## 10. Estado del motor FinOps

Disponible: asignación por namespace y workload, costo por petición, presupuestos y desviaciones (base estimada salvo que se
aporte facturación), adaptador FOCUS de Google (un día real). Nuevo: conciliación antes/después y niveles de ahorro.
Pendiente de validación: Azure FOCUS; **ninguna pareja real de exportaciones se ha conciliado**. No implementado: pronóstico
de gasto, llamadas directas a APIs de facturación (decisión deliberada).

Ahorros a la fecha: estimado y proyectado, de rutina; observado, dos mediciones a precio de lista en cargas de prueba;
**conciliado con factura, ninguno**.

## 11. Resultados de benchmarking

Frente a CAST AI, Kubecost y ScaleOps: **no ejecutado**. Requiere cuenta o licencia de cada proveedor y un clúster propio.
Se entrega el arnés (escenarios, trazas con semilla fija, recolector, tabla comparativa) y la metodología
(docs/BENCHMARKING.md). El arnés no se ha ejecutado en un clúster. La única comparación existente es simulada
(`KubeCostAutoscaler` frente a HPA nativo, docs/BENCHMARKS.md) y su conclusión es que la configuración segura no ahorra
frente a un HPA con las mismas ventanas. No hay base para afirmar superioridad ni paridad.

## 12. Limitaciones pendientes

docs/KNOWN_LIMITATIONS.md. Las principales: valores por defecto del recomendador sin afinar; ninguna corrida larga; ningún
ahorro conciliado; modo automatizado sin validar bajo carga real; sin consolidación automática de nodos; sin multi-clúster;
sin cobro ni licencias; consola de demostración.

## 13. Riesgos operativos

Tabla completa en docs/KNOWN_LIMITATIONS.md. Los que más pesan: una reducción de memoria que provoca OOM después de la ventana
de observación; una reducción de CPU bajo carga creciente; un overlay de Kustomize que quita `--observe-only` sin que nadie
lo note; precios unitarios por defecto que vuelven inútiles las estimaciones.

## 14. Instalación y actualización

docs/INSTALL.md y docs/UPGRADE.md. En corto: `helm upgrade --install` del chart (observe-only por defecto); para actualizar,
aplicar primero `crds/` y después `helm upgrade`.

## 15. Backlog priorizado

docs/ROADMAP.md, sección "Prioritised backlog after the twelfth pass". Los tres primeros: leer el informe del soak de GKE;
backtest sobre semanas de tráfico real; conciliar un ahorro con una factura.

## 16. Evaluación honesta de preparación comercial

**Beta. No es comercializable todavía.**

A favor: el núcleo de seguridad es sólido y está probado en clústeres reales (política fail-closed, aprobación con
separación de funciones, rollback verificado, auditoría); el modo observe-only es utilizable hoy; la documentación ahora dice
lo que hay.

En contra: no existe un solo ahorro confirmado contra una factura; el recomendador no se ha validado con tráfico real de
semanas; no hay comparación con ningún competidor, y los tres cubren áreas que este producto no toca (gestión de compromisos,
automatización de Spot, aprovisionamiento de nodos); no hay multi-clúster, ni cobro, ni licencias, ni organización de soporte,
ni auditoría de seguridad externa.

Para Release Candidate: backlog 1 a 5 y 7. Para "Producción validada": además, operación prolongada con clientes reales,
recuperación probada y conciliación de costos rutinaria. Nada de eso se puede producir desde este entorno: necesita clústeres
con tráfico real, exportaciones de facturación y tiempo.
