# Expediente de infraestructura — 2026-11-16

## Evidencia local

`python hack/local_enterprise_lab.py` crea un Kind con nombre aleatorio y kubeconfig propio, construye imágenes locales, instala los CRD, ejecuta el overlay E2E y verifica resize sin recreación de pods ni reinicios. Después detiene el primer operador para evitar dos escritores, instala/actualiza/revierte Helm y verifica el PVC. El cleanup elimina exclusivamente el clúster creado por esa ejecución.

Resultados y duración por comando: [results.json](reports/evidence/enterprise-lab-final/results.json). Logs: [lab.log](reports/evidence/enterprise-lab-final/lab.log). Aceptación: [acceptance.json](reports/evidence/enterprise-lab-final/acceptance.json). Kubernetes 1.33.4, Docker Desktop/Kind local; Prometheus de fixture, nginx de prueba, sin facturas ni carga de clientes. Este resultado no cuenta como GKE/AKS ni como upgrade entre versiones publicadas.

Las correcciones posteriores de compilador/dependencias requieren rebuild y nueva verificación; los logs conservan qué imagen se utilizó en cada ejecución.

## Preparación GKE y AKS

No se buscaron ni adquirieron credenciales cloud. `hack/cloud_preflight.py` no consulta ningún clúster sin `--authorized-context` coincidente. Ejemplo que solo imprime el plan:

```text
python hack/cloud_preflight.py --context CONTEXTO_GKE --provider gke
python hack/cloud_preflight.py --context CONTEXTO_AKS --provider aks
```

Tras autorización explícita del propietario, añadir `--authorized-context CONTEXTO` para recoger versión, nodos, estado/reinicios de pods, PDB y metrics-server. No se guardan entornos de containers, secretos ni identificadores de cuenta. Revisar los nombres de recursos antes de compartir el informe.

Renderizar sin instalar:

```text
helm template pilot deploy/helm/kubecost-auto-hpa -f deploy/helm/kubecost-auto-hpa/values-production.yaml -f deploy/validation/gke.yaml
helm template pilot deploy/helm/kubecost-auto-hpa -f deploy/helm/kubecost-auto-hpa/values-production.yaml -f deploy/validation/aks.yaml
```

Antes de instalar: acordar namespace/ventana, secretos TLS y bearer ya creados, fuente Prometheus y Kubecost, StorageClass autorizada, respaldo SQLite consistente y procedimiento de restauración. Mantener `enableSpotDrain=false` y CRs `dryRun: true`. Nunca usar snapshots SQLite con WAL abierto mediante una simple copia del archivo: usar la API backup de SQLite o detener la instancia y copiar todos los archivos necesarios.

| Prueba | GKE | AKS | Aceptación |
|---|---|---|---|
| Versión/subrecurso resize | Verificar versión y disponibilidad del API | Verificar versión y disponibilidad del API | Resize soportado o rechazo explícito |
| Métricas | Validar series/labels y frescura | Validar series/labels y frescura | Sin downsizing ante muestras insuficientes |
| Autoscaler de nodos | Identificar dueño de pools y restricciones GKE | Identificar dueño de pools y restricciones AKS | No escritores concurrentes ni cambio directo de VM |
| PDB/afinidad/taints | Stateful, PVC zonales, DS y cargas de sistema | CSI, node pools de sistema, PVC zonales y DS | Plan inseguro rechazado; sin bypass de eviction |
| Reinicio API/operador | Registrar decisiones y estado antes/después | Ejecutar independientemente | Sin duplicación de efectos ni pérdida de aprobaciones |
| Upgrade/rollback | v0.1.0 → candidato construido y firmado | Mismo protocolo independiente | Datos y CRD conservados; smoke y restauración exitosos |

GKE documenta bloqueos de scale-down por workloads y PDB; AKS tiene su propio conjunto de restricciones y perfiles. No se asume equivalencia de proveedor. Fuentes: [GKE](https://docs.cloud.google.com/kubernetes-engine/docs/troubleshooting/cluster-autoscaler-scale-down), [AKS](https://learn.microsoft.com/azure/aks/cluster-autoscaler-overview), [Kubernetes drain](https://kubernetes.io/docs/tasks/administer-cluster/safely-drain-node/).

## Soak de 72 horas a 7 días

Usar `cloud_preflight.py` con `--duration-seconds 259200 --interval-seconds 60` después de autorizar el contexto. Es un recolector de salud, no un generador de carga. Registrar adicionalmente RPS, error rate, p99, RSS, CPU del operador/motor, memoria del proceso, pendientes, decisiones, rechazos, recargas y recuperación en Prometheus.

Carga acordada: estable, rampa, picos repetidos, caída y recuperación de métricas; fallos de red/API y reinicios únicamente en staging autorizado. Objetivos propuestos: cero violaciones de PDB, cero pérdidas de datos, cero efectos duplicados, cero writes en Observe-only, p99 y errores dentro del SLO acordado, memoria sin crecimiento sostenido después de estabilizar cardinalidad. Medir falsos positivos con etiquetado humano, no inferirlos de disponibilidad.

Cada ejecución debe registrar commit/digest, proveedor/contexto, horarios, carga, flags/políticas, resultados crudos, fallos, corrección y repetición. No ejecutado: GKE, AKS, soak prolongado, Automated real, restauración de desastre ni upgrade v0.1.0 → v0.2.0 (esta última versión no existe como release).

Los scripts históricos `hack/cloud-validate.sh` y `hack/lab-e2e.sh` referencian CLIs ausentes (`cmd/rightsize`, `cmd/recommend`); no son evidencia vigente. Usar los runners Python nuevos para sus alcances documentados. No se presentan como sustitutos del aplicador faltante.
