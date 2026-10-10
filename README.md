# KubeCost Auto-HPA

Operador nativo de Kubernetes que reemplaza el escalado por CPU/RAM con **Unit Economics Scaling**: ajusta
el tamaño de los Pods y migra cargas tolerantes a fallos a instancias Spot (AWS/GCP) evaluando la latencia
p99 y el costo financiero por petición servida.

```
 Prometheus / KubeCost / Spot API
              |
   Operator (Go, Kubebuilder)  -- evalúa SLO latencia vs costo
              |
   Unit Economics Engine (Python) -- costo / RPS
              |
   Control Plane & Guardrails  --> Spot Migration (cordon/drain)
              |
   Kube-API Resource Adjuster  -- in-place resize de requests/limits
```

## Propuesta de valor

`C_pod = Costo Instancia / RPS`. Escala según rentabilidad y SLO, no según picos de CPU/RAM.

## Funcionalidades

1. **Escalado por rentabilidad y SLOs** – reduce límites cuando la latencia p99 está holgada frente al SLA.
2. **Orquestador Spot** – detecta cargas tolerantes a fallos y gestiona cordon/drain.
3. **Hot-resizing in-place** – actualiza `requests`/`limits` sin rolling update.
4. **Guardrails de presupuesto** – congela el auto-escalado al alcanzar el límite mensual (`FinOps Agent`).

## Componentes

| Ruta | Qué es |
|---|---|
| `engine/` | Motor Unit Economics (Python, stdlib). Núcleo + API HTTP `POST /v1/evaluate`, `/healthz`, `/metrics`. |
| `operator/` | Operador Go (controller-runtime): CRD `KubeCostHPA`, reconciler, actuadores, cliente Prometheus/KubeCost, caché Redis. |
| `operator/config/` | Kustomize: `default` (CRD, RBAC, operador, motor, Redis) y `overlays/local` (+ workload demo). |
| `deploy/helm/kubecost-auto-hpa/` | Chart de Helm equivalente. |
| `docker-compose.yml` | Stack local: Redis, motor, Prometheus, Grafana (+ operador con `--profile operator`). |

## Flujo de reconciliación

1. Lee `KubeCostHPA` y el Deployment objetivo (CPU/memoria solicitadas, pods en ejecución).
2. Métricas: RPS, p99 y uso de CPU desde Prometheus (consultas PromQL configurables en `internal/metrics`),
   con caché en Redis (30 s) y respaldo a la última lectura (15 min) si Prometheus cae.
3. Gasto mensual del namespace desde la API de asignación de KubeCost (si hay `budget`).
4. Llama al motor: decide `shrink`/`grow`/`hold`, calcula `$/request`, congelación por presupuesto y elegibilidad Spot.
5. Actúa: resize **in-place** vía subrecurso `pods/resize` (sin reinicio) y converge también los pods nuevos.
   El estado temporal (cooldown, última decisión) vive en Redis; `status` del CR es la fuente de verdad.
6. Publica estado/condiciones/eventos y métricas Prometheus (`kubecost_hpa_*`).

Un segundo controlador (`--enable-spot-drain`) hace **cordon + drain** de nodos Spot con señal de interrupción
(taint `aws-node-termination-handler/spot-itn`, `cloud.google.com/impending-node-termination` o la anotación
`autoscaling.kubecost.io/spot-interruption=true`), usando la Eviction API (respeta PDBs).

### Reglas de seguridad

- Nunca reduce por debajo de `uso_real / 0.7` ni fuera de `cpu.min`/`cpu.max`.
- Solo cambia CPU; memoria no se toca (reducirla in-place es inseguro).
- Rechaza el resize si `resizePolicy` de CPU es `RestartContainer`; espera si hay un resize en curso.
- Sin métricas o con el motor caído no se hace nada. Si KubeCost no responde, el presupuesto no se aplica en ese ciclo (fail-open, para no sacrificar el SLO).
- El drain se niega si no queda otro nodo listo, si hay pods sin controlador, y reintenta evicciones bloqueadas por PDB hasta `--drain-timeout`.
- El presupuesto congelado solo bloquea crecer; reducir siempre se permite.

## Uso

```yaml
apiVersion: autoscaling.kubecost.io/v1alpha1
kind: KubeCostHPA
metadata: {name: checkout, namespace: shop}
spec:
  targetRef: {name: checkout}
  slo: {p99LatencyMs: 250}
  cpu: {min: 100m, max: "2"}
  unitEconomics: {maxCostPerRequest: "0.000002"}
  budget: {monthlyLimitUSD: "5000"}
  spot: {enabled: true, faultTolerant: true}
  dryRun: true      # empezar en dry-run
```

Requisitos del clúster: Kubernetes ≥ 1.33 (`InPlacePodVerticalScaling` GA/beta con subrecurso `pods/resize`),
Prometheus con métricas `http_requests_total` / `http_request_duration_seconds`, y KubeCost (opcional).

El resize in-place cambia el Pod, no el template del Deployment: el operador lo reaplica a pods nuevos,
pero un cambio manual del template vuelve a ser la base.

## Despliegue

```
# Local (stack de observabilidad + motor + Redis)
docker compose up -d --build
# Operador contra un clúster kind/minikube
docker compose --profile operator up -d --build

# Kustomize
kubectl apply -k operator/config/default
kubectl apply -k operator/config/overlays/local      # imágenes locales + workload demo

# Helm
helm install kch deploy/helm/kubecost-auto-hpa -n kubecost-auto-hpa-system --create-namespace \
  --set prometheus.url=http://prometheus-server.monitoring:80
```

## Desarrollo y pruebas

```
cd engine && pip install -e '.[dev]' && pytest
cd operator && make test           # unitarias (clientes fake, miniredis, httptest)
cd operator && make integration    # E2E con el motor Python real + Redis + Prometheus/KubeCost simulados
cd operator && make manifests      # regenera deepcopy, CRD, RBAC y copia el CRD al chart (requiere controller-gen)
```

## Estado y límites conocidos

- Verificado en este repo: tests Python y Go (unitarios + E2E con motor real), `go vet`, `helm lint/template`,
  `kustomize build`, `docker compose config`.
- **No verificado contra un clúster real**: la API `pods/resize`, Eviction y el contenedor de las imágenes se probaron con
  clientes fake; falta una ejecución en kind/minikube ≥ 1.33.
- El E2E usa el `client` fake de controller-runtime, no `envtest` (necesita binarios de kube-apiserver/etcd).
- Las imágenes no se publican aún (CI solo prueba/valida); el stack de Compose las construye localmente.
- Las consultas PromQL por defecto asumen nombres de métrica convencionales; sobreescribir en `metrics.Queries` si difieren.
- Pendiente: HPA horizontal (réplicas), memoria, autenticación a Prometheus/KubeCost, TLS entre componentes.
