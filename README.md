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

## Estado

| Componente | Estado |
|---|---|
| `engine/` Unit Economics (Python) | Implementado (núcleo + tests) |
| `operator/` (Go / Kubebuilder) | Pendiente |
| Integración Prometheus / Redis / Grafana | Pendiente |

## Arquitectura técnica

- Operator: Go + Kubebuilder + client-go.
- Analítica financiera: Python + NumPy/Pandas.
- Métricas: Prometheus + Grafana. Cache de estado de nodos/Spot: Redis.

## Desarrollo

```
cd engine && pip install -e '.[dev]' && pytest
```
