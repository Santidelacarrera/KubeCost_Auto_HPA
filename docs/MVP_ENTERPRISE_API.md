# Alcance verificable del MVP empresarial

El MVP permite observar clústeres, analizar muestras, generar recomendaciones, planificar consolidación, revisar aprobaciones e importar costos, manteniendo al operador Kubernetes como responsable de las escrituras ya soportadas.

## Flujo local

```powershell
$env:PYTHONPATH='engine'
$env:ENGINE_TOKEN='<token local generado por el operador>'
$env:ENGINE_TOKEN_TENANT_ID='mi-organizacion'
$env:SAAS_DB_PATH='.local-validation/control-plane.db'
# Crear previamente .local-validation y proteger los permisos de este directorio.
python -m kubecost_engine.server
```

Abrir `http://127.0.0.1:8000/app.html`; usar la misma organización y credencial. El navegador conserva la credencial solo en memoria. Para uso remoto se exige HTTPS en la consola. No publicar el modo de desarrollo sin autenticación.

## Contratos nuevos

`GET /v1/openapi.json` documenta la extensión empresarial; no representa todavía un contrato exhaustivo de todas las rutas históricas. Las claves de usuario requieren `enterprise:read`/`enterprise:write`, y las aprobaciones `consolidation:approve`. Los administradores de organización siguen sujetos a su tenant.

Las recomendaciones reciben `cluster_id`, `workload`, `current` y `samples`. Cada muestra incluye `timestamp`, `cpu_millicores`, `memory_mib` y opcionalmente `oom_killed`. Las predicciones reciben muestras `{timestamp,demand}`, horizonte y periodo estacional opcional. Los datos deben provenir de una fuente observada; estos endpoints no recolectan Prometheus automáticamente.

Las importaciones requieren `provider` (`gcp`, `azure`, `focus`) y `records`, con `record_id` estable. FOCUS requiere BilledCost, EffectiveCost, BillingCurrency, ChargePeriodStart y ChargePeriodEnd. Se preservan resource_id y dimensiones de asignación suministradas. Azure usa CostInBillingCurrency y periodos explícitos; GCP usa cost, credits, currency, usage_start_time y usage_end_time. Esta es una interfaz normalizada de importación, no un lector universal de cada variante de exportación cloud.

Una factura requiere identidad, hash SHA-256 del documento, moneda, periodo y total. Un match confirma igualdad contable con los registros importados; no autentica el documento ni demuestra ahorro atribuible.

## Fuera del MVP validado

Consolidación autónoma de extremo a extremo, equivalencia completa con kube-scheduler, eliminación de VM, facturación real, SSO/usuarios, retención automática, HA de la API y validación empresarial externa. Ningún botón de la consola afirma ejecutar estas funciones.
