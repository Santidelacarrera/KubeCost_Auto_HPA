# Preparación comercial y externa

Estado: paquete de preparación; no cliente, ingreso, certificación ni ahorro demostrado. No se contactó a terceros.

## Oferta y costos

Los planes Starter, Professional, Business y Enterprise están definidos en `engine/kubecost_engine/enterprise.py` y publicados mediante `/v1/plans`. Límites de clústeres: 1/3/10/100; nodos: 10/50/250/10000; workloads: 50/250/2000/50000; usuarios: 3/10/50/1000; retención propuesta: 14/30/90/365 días. Solo el límite de clústeres y algunas funcionalidades se aplican actualmente con licencia obligatoria; usuarios/retención y cuotas integrales son pendientes de código. No ofrecerlos como controles ya implementados.

Modelo de precio propuesto, sin tarifa de mercado inventada:
`precio mínimo mensual = (infraestructura + almacenamiento + observabilidad + horas de soporte × costo/hora + provisión de incidentes) / (1 - margen objetivo)`.
Completar las variables con costos observados del piloto, por nodo y por organización. Publicar sensibilidad a cardinalidad, retención y horas de soporte. No calcular comisión sobre ahorros proyectados: usar solo ahorro atribuible validado y aceptado por ambas partes. Las cifras del catálogo heredado son estimaciones estáticas, no cotizaciones actuales.

`/v1/subscriptions/sandbox` implementa activación, renovación y cancelación en proveedor de pruebas local, con event_id y CAS. Cada resultado dice `local_sandbox`, `live_transaction=false` y no otorga entitlements productivos. El protocolo PaymentProvider permite implementar posteriormente un gateway. Los tests HMAC de Stripe existentes no equivalen a probar cobros en el sandbox externo.

## Programa design partner: dos semanas Observe-only

Cliente ideal: equipo con owner SRE y FinOps, GKE o AKS staging representativo, métricas históricas, trazabilidad de costos y margen para validar sin comprometer producción. Requisitos: versión/API compatibles, fuente de métricas, etiquetas de ownership, acceso de lectura acordado, capacidad de exportar factura anonimizada y responsable de seguridad.

Consentimiento previo: describir datos/campos recopilados, finalidades, retención, ubicación, personal autorizado, revocación de acceso, alcance exacto sin writes, canal de incidentes y prohibición de publicación sin autorización. No ingresar datos de clientes en fixtures del repositorio.

| Días | Actividad | Responsable | Evidencia |
|---|---|---|---|
| 1–2 | Consentimiento, inventario, RBAC, baseline y carga representativa | Cliente + SRE | Checklist firmado, métricas de partida |
| 3–5 | Instalación Observe-only y revisión diaria de recomendaciones | SRE | Cero writes, muestras, decisiones explicadas |
| 6–7 | Clasificación de falsos positivos y cargas excluidas | App owners | Justificación aceptada/rechazada por recomendación |
| 8–10 | Variaciones de demanda y desconexión autorizada | Cliente | Disponibilidad, reconexión y estado persistido |
| 11–12 | Exportación de costos y comparación bajo mismo periodo | FinOps | Archivos/hash, moneda, asignación y diferencias |
| 13–14 | Revisión de riesgos, valor potencial e informe final | Ambas partes | Resultados, límites, decisión de siguiente fase |

Registro diario: fecha UTC/local, commit/digest, clúster anonimizado, ventana, muestras, cobertura, p99/error rate, recomendaciones, falsos positivos, acciones realmente aplicadas por el cliente, costo base/actual, incidentes, aprobación y próximos pasos. Éxito propuesto: recomendaciones trazables, cero writes no autorizados, ausencia de degradación por el agente y disposición del cliente a continuar. En Observe-only solo hay ahorro potencial.

## Demostración financiera

1. Fijar cuentas/recursos, periodo, moneda y baseline; registrar precios, descuentos, compromisos, créditos y demanda.
2. Importar exportaciones con record_id estable y hashes. Rechazar filas faltantes; no sumar monedas.
3. Conciliar BilledCost con total y alcance de factura; investigar impuestos, créditos o líneas fuera del dataset. EffectiveCost no debe sustituir automáticamente BilledCost en conciliación de caja ([FOCUS BilledCost](https://focus.finops.org/docs/specification/v1-2/columns/cost-and-usage/billed-cost/)).
4. Comparar antes/después normalizado por demanda y exposición; separar variación de tarifa y descuentos de cambios de recursos.
5. Atribuir solo cambios respaldados por decisiones y acciones comprobadas, con aprobación financiera. Conservar un residual no atribuible.

`estimated`: modelo de tarifas; `projected`: escenario recomendado; `observed`: diferencia medida con condiciones documentadas; `invoice-reconciled`: resultado financiero con baseline y atribución aprobados. El código nuevo nunca produce el último a partir de una simulación o un simple match de factura.

## Benchmark reproducible

Comparar CAST AI, Kubecost y ScaleOps en clústeres equivalentes o periodos cruzados, con misma región, versión, imágenes, trazas de carga, límites, PDB, SLO, moneda, descuentos y ventana. Cada producto debe tener un único escritor; no enfrentarlos simultáneamente sobre el mismo workload. Registrar versión/plan/precio cotizado y configuración. CAST AI requiere cotización según entorno; no se inventó un precio ([fuente oficial](https://cast.ai/pricing/)).

Métricas: costo total incluido software/operación, error de recomendación, p99, errores, pending time, OOM/restarts, capacidad liberada, recursos del controlador, tiempo de instalación, fallos/recuperación y horas humanas. Ejecutar varias repeticiones, publicar distribución y condiciones. No se dispone de acceso a competidores: resultados y superioridad permanecen sin determinar.

## Soporte y borradores contractuales

Propuesta operativa, no compromiso contractual: P1 caída o riesgo de datos, primera respuesta objetivo 1 h dentro de cobertura pactada; P2 degradación 4 h; P3 defecto sin indisponibilidad 1 día hábil; P4 consulta 2 días hábiles. Atención 24×7 solo después de financiar y probar la guardia. Escalamiento: soporte → SRE responsable → seguridad/ingeniería → contacto del cliente. RTO/RPO/SLA se fijan después de ejercicios, no se prometen como capacidades existentes.

Los borradores MSA/orden de servicio, DPA y privacidad ya están en [MSA](ENTERPRISE_ORDER_FORM_AND_MSA_TEMPLATE.md), [DPA](DATA_PROCESSING_ADDENDUM_TEMPLATE.md) y [privacidad](DATA_PROTECTION_AND_PRIVACY_COMPLIANCE.md). Deben ser revisados por abogados de las jurisdicciones aplicables antes de uso. Anexar: exclusiones del SLA, cálculo de créditos, subencargados, residencia y retención, borrado/exportación, notificación de incidentes, responsabilidades de backups y límites de automatización. No se afirma cumplimiento legal ni certificaciones obtenidas.
