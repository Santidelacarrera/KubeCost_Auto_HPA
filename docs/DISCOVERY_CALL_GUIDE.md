# Guía de Llamada de Descubrimiento y Calificación Comercial (Discovery Call Guide)

Esta guía estructura una reunión inicial de ventas de 25 minutos con decisores corporativos (VPs de Plataforma, Directores de FinOps y Arquitectos Cloud) utilizando el marco **MEDDPICC**.

---

## 1. Estructura de la Reunión (25 Minutos)

```
 [00:00 - 03:00]  Alineación de Agenda y Rapport
 [03:00 - 12:00]  Diagnóstico del Dolor (Preguntas de Descubrimiento)
 [12:00 - 18:00]  Demostración de Ajuste de Solución y Calculadora de ROI
 [18:00 - 22:00]  Manejo de Objeciones Técnicas y de Seguridad
 [22:00 - 25:00]  Cierre y Activación del Piloto de 14 Días (dryRun)
```

---

## 2. Marco MEDDPICC para KubeCost Auto-HPA

| Criterio MEDDPICC | Pregunta Clave de Diagnóstico | Respuesta Esperada para Calificar |
|---|---|---|
| **M - Metrics (Métricas)** | *"¿Cuánto gastáis actualmente al mes en cómputo Kubernetes en AWS/GCP/Azure?"* | Gasto > \$5.000 USD/mes (más de 30 nodos). |
| **E - Economic Buyer (Comprador)** | *"Si demostramos un ahorro del 30% en 14 días con riesgo cero, ¿quién firma la orden de compra?"* | VP of Infrastructure, Head of DevOps o Director de FinOps. |
| **D - Decision Criteria (Criterios)** | *"¿Qué requisitos técnicos y de seguridad debe cumplir una herramienta antes de instalarla en vuestro clúster?"* | Sin agentes externos SaaS, compatibilidad con Prometheus/Kubecost, 0 CVEs, sin acceso root. |
| **D - Decision Process (Proceso)** | *"¿Cómo es el proceso de homologación de software desde la prueba de concepto hasta producción?"* | Piloto técnico en staging (2 semanas) -> Revisión CISO -> Aprobación de compra. |
| **P - Paper Process (Legal)** | *"¿Tenéis un contrato marco estándar o utilizáis nuestro formulario de orden y DPA pre-aprobado?"* | Revisión de DPA/MSA con asesoría legal (estándar enterprise). |
| **I - Implied Pain (Dolor)** | *"¿Cuántas recomendaciones de Kubecost o AWS quedan sin implementar porque los desarrolladores temen caídas?"* | Dolor alto: recomendaciones desatendidas, sobredimensionamiento crónico. |
| **C - Champion (Defensor Interno)** | *"¿Quién en vuestro equipo de SRE o plataforma se beneficiaría directamente de no recibir alertas de throttling?"* | Lead SRE o Tech Lead de infraestructura. |
| **C - Competition (Competidores)** | *"¿Estáis evaluando o usando herramientas como Karpenter, CAST AI o el HPA estándar de Kubernetes?"* | Karpenter (a nivel de nodo) o HPA nativo (con quejas de poca flexibilidad). |

---

## 3. Preguntas de Diagnóstico Recomendadas (Bucle de Preguntas)

1. *"¿Cómo calculáis hoy los requests y limits de CPU y memoria de vuestros pods? ¿Se ajustan manualmente o tenéis alguna automatización?"*
2. *"Cuando tenéis un pico de tráfico inesperado, ¿qué ocurre con la latencia p99 antes de que el HPA tradicional termine de levantar nuevos pods?"*
3. *"¿Utilizáis instancias Spot en producción? Si no es así, ¿qué os impide usarlas para ahorrar un 60%?"* (Respuesta típica: miedo a los avisos de interrupción de 2 minutos).
4. *"Si os mostramos un ahorro verificado de \$60.000 al año en Grafana durante 14 días sin tocar ningún pod en producción (`dryRun: true`), ¿tendría sentido avanzar con una suscripción comercial?"*

---

## 4. Matriz Rápida de Manejo de Objeciones en Llamada

### Objeción 1: "Ya usamos Karpenter / Cluster Autoscaler, ¿no hace esto lo mismo?"
> **Respuesta:**  
> *"Karpenter es una excelente herramienta para aprovisionar nodos (máquinas virtuales), pero es completamente ciego a lo que piden los pods por dentro. Si un desarrollador pide 4 cores para una aplicación que solo usa 0.5 cores, Karpenter levantará una máquina gigante y cara para alojar ese pod sobredimensionado. KubeCost Auto-HPA optimiza el pod desde dentro y lo redimensiona in-place. De hecho, cuando se usan juntos, Karpenter puede empaquetar los nodos hasta un 40% más densamente y apagar máquinas sobrantes. Son herramientas complementarias, no rivales."*

### Objeción 2: "¿Qué pasa si la herramienta se equivoca y reduce demasiado los pods en producción?"
> **Respuesta:**  
> *"Contamos con 3 capas de salvaguarda estrictas:  
> 1. Primero, fijas límites infranqueables: `minReplicas` y mínimos de CPU/memoria que el controlador nunca puede sobrepasar.  
> 2. Segundo, si la latencia p99 se acerca al SLO fijado, el motor activa el interruptor de emergencia y escala de inmediato.  
> 3. Tercero, si Prometheus o el motor fallan, el operador conmuta automáticamente al HPA nativo de Kubernetes en segundos sin interrumpir el servicio."*

### Objeción 3: "Nuestra política de seguridad prohíbe enviar telemetría a nubes de terceros."
> **Respuesta:**  
> *"KubeCost Auto-HPA corre 100% dentro de vuestro propio clúster y VPC. No hay ningún agente externo, no hay envío de telemetría a nuestros servidores y el pod está configurado para no tener salida a Internet. Además, nuestras imágenes están firmadas criptográficamente con Sigstore Cosign y tienen cero vulnerabilidades conocidas en escaneos de Trivy."*

---

## 5. El Cierre del Piloto (The 14-Day Pilot Close)

Al llegar al minuto 22 de la llamada, utiliza esta frase de cierre para acordar la prueba de concepto:

> *"{{Nombre}}, la mejor forma de que veas el valor sin ningún tipo de compromiso ni riesgo es activar nuestro modo de evaluación shadow (`spec.dryRun: true`).  
> Se instala en 10 minutos mediante nuestro chart de Helm o módulo de Terraform. Durante los próximos 14 días, el operador observará vuestras cargas reales y os mostrará en vuestro Grafana exactamente cuántos miles de dólares habríais ahorrado sin tocar un solo pod.  
> Si los números en dos semanas demuestran un retorno claro superior a 5x, hablamos de formalizar el soporte enterprise. Si no os convence, se desinstala en un minuto con `helm uninstall`.  
> ¿Qué os parece si agendamos 20 minutos este martes con vuestro equipo de plataforma para dejarlo corriendo en dry-run?"*
