# Playbook de Prospección Comercial y Secuencias de Venta (Sales Outreach Playbook)

Este playbook contiene las secuencias de correos electrónicos en frío, mensajes de LinkedIn, guiones telefónicos y argumentos de venta para comercializar **KubeCost Auto-HPA** ante directores de infraestructura, FinOps y líderes de ingeniería.

---

## 1. Identificación de Personas Clave (Buyer Personas)

| Cargo Objetivo | Principal Dolor | Gancho de Venta |
|---|---|---|
| **VP of Infrastructure / Head of Platform** | Sobredimensionamiento crónico de pods (30%-50% de desperdicio en AWS/GCP); desarrolladores ignorando tickets de Jira para reducir memoria/CPU. | *"Automatiza el redimensionamiento in-place sin reiniciar pods ni despertar ingenieros."* |
| **Director of FinOps / Cloud Economics** | El gasto en Kubernetes crece más rápido que los ingresos; las recomendaciones de Kubecost son de solo lectura y nadie las implementa. | *"Convierte las recomendaciones de Kubecost en acciones automáticas de autoescalado con payback en 10 días."* |
| **Lead SRE / Principal Architect** | Incumplimientos de p99 latency SLO durante picos imprevistos; miedo a usar instancias Spot por riesgo de caídas con avisos de 2 minutos. | *"Autoescala por $/petición protegiendo p99 latency con drenado seguro de instancias Spot sin errores 502."* |

---

## 2. Secuencia de Correos en Frío (Cold Email Cadence)

### Email 1: El gancho del 40% de sobrecoste en Kubernetes (Día 1)
**Asunto:** Reducción del 35% en el clúster de Kubernetes de {{Company}}  
**Destinatario:** VP of Infrastructure / Head of Platform / Director of FinOps

> Hola {{Nombre}},
> 
> En la mayoría de las organizaciones con más de 50 nodos en Kubernetes, entre el 30% y el 45% de la factura de cómputo en la nube se gasta en capacidad ociosa "por seguridad" frente a posibles picos de tráfico.
> 
> Desarrollamos **KubeCost Auto-HPA**, el primer autoescalador autónomo que escala cargas de trabajo según la economía unitaria (**coste por petición served**) en lugar de porcentajes ciegos de CPU, garantizando estrictamente los SLOs de latencia p99 y redimensionando pods *in-place* sin reinicios.
> 
> En flotas de 80 a 150 nodos, generamos ahorros netos anuales verificados de entre **\$50.000 y \$140.000 USD**, con un período de amortización inferior a 10 días.
> 
> ¿Tendrías 15 minutos este jueves o viernes para mostrarte una simulación con la calculadora de ROI adaptada a vuestra infraestructura?
> 
> Saludos,  
> **Santiago de la Carrera**  
> Fundador & Lead Architect — KubeCost Auto-HPA  
> [Calculadora de ROI en Vivo](https://santidelacarrera.github.io/KubeCost_Auto_HPA/)

---

### Email 2: El problema de las recomendaciones de solo lectura (Día 4)
**Asunto:** Re: Por qué las recomendaciones de Kubecost no se aplican en {{Company}}

> Hola {{Nombre}},
> 
> Conversando con equipos de plataforma, encontramos un patrón recurrente: herramientas como Kubecost o AWS Cost Explorer generan paneles llenos de sugerencias de ahorro, pero los desarrolladores rara vez las aplican por temor a causar caídas en producción.
> 
> **KubeCost Auto-HPA cierra ese ciclo de forma autónoma y segura:**
> 1. Cuenta con un **modo shadow con cero riesgo (`dryRun: true`)**: evalúa vuestro clúster durante 14 días y proyecta el ahorro real en dólares en Grafana sin tocar ningún pod.
> 2. Dispone de **interruptores automáticos de seguridad (*circuit breakers*)**: si la latencia p99 se acerca al límite, la optimización económica cede el paso de inmediato al escalado agresivo.
> 3. Es **100% *air-gapped* y privado**: corre dentro de vuestro VPC, sin enviar telemetría ni datos fuera de vuestro perímetro.
> 
> ¿Te gustaría activar una prueba en modo dry-run esta semana en un clúster de staging?
> 
> Saludos,  
> **Santiago**

---

### Email 3: Certificación técnica y casos de uso (Día 8)
**Asunto:** Certificación de 500 usuarios concurrentes y 0% de caídas

> Hola {{Nombre}},
> 
> La estabilidad de producción suele ser la principal duda al automatizar el dimensionamiento de pods.
> 
> Acabamos de publicar nuestro informe de certificación de alta concurrencia: el motor procesó **2.000 transacciones simultáneas de 500 usuarios concurrentes con 0% de errores y cero caídas**, gracias a su arquitectura con pool de hilos y cola de backlog ampliada.
> 
> Además, cuenta con un módulo de drenado proactivo de instancias Spot que intercepta el aviso de terminación de 2 minutos de AWS/GCP, migrando pods sin pérdida de conexiones ni errores 502.
> 
> Te adjunto nuestro [White Paper de Seguridad para CISOs](https://github.com/Santidelacarrera/KubeCost_Auto_HPA/blob/main/docs/ENTERPRISE_SECURITY_WHITE_PAPER.md) por si quieres revisarlo con tu equipo de InfoSec.
> 
> ¿Qué día te vendría bien para una demo técnica rápida de 15 minutos?
> 
> Saludos,  
> **Santiago**

---

### Email 4: Cierre / "Break-up Email" (Día 14)
**Asunto:** Último intento: Optimización de clústeres en {{Company}}

> Hola {{Nombre}},
> 
> Entiendo que optimizar la infraestructura de Kubernetes puede no ser una prioridad urgente en este momento o que vuestro equipo esté enfocado en otros lanzamientos.
> 
> No volveré a insistir por correo. Te dejo a mano el repositorio de código abierto y la documentación comercial por si decides retomarlo más adelante:
> - Repositorio: https://github.com/Santidelacarrera/KubeCost_Auto_HPA
> - Calculadora de ROI y Planes Enterprise: https://github.com/Santidelacarrera/KubeCost_Auto_HPA/blob/main/docs/COMMERCIAL.md
> 
> Si en el futuro necesitas reducir entre un 28% y un 40% de sobrecoste en cómputo con un piloto garantizado de 14 días, aquí me tienes.
> 
> Un cordial saludo,  
> **Santiago de la Carrera**

---

## 3. Mensajes Directos para LinkedIn / InMail

### Opción A: Enfoque FinOps / Directores
> *"Hola {{Nombre}}, vi que lideras el área de infraestructura en {{Company}}. Desarrollamos un operador de autoescalado para Kubernetes que optimiza pods basándose en el coste por petición ($/req) manteniendo la latencia p99. Estamos ayudando a equipos de Kubernetes a recortar un 35% de gasto ocioso en AWS/GCP con un piloto de 14 días con riesgo cero (`dryRun`). Me encantaría compartirte nuestra calculadora de ROI si te parece relevante. ¡Un saludo!"*

### Opción B: Enfoque SRE / Arquitectos Técnicos
> *"Hola {{Nombre}}, un gusto conectar. Creamos KubeCost Auto-HPA para solucionar el redimensionamiento in-place de pods sin reinicios y el drenado seguro de Spot ante avisos de 2 minutos de AWS. El proyecto es código abierto con distribución firmada por Sigstore Cosign y 0 CVEs. ¿Te interesaría probarlo en staging en modo shadow (`dryRun: true`)? Te dejo el link por si quieres echarle un ojo: https://github.com/Santidelacarrera/KubeCost_Auto_HPA"*

---

## 4. Guión para Llamadas Telefónicas en Frío (Cold Calling - 45 Segundos)

1. **Apertura y Permiso (5s):**  
   *"Hola {{Nombre}}, te habla Santiago de la Carrera de KubeCost Auto-HPA. Sé que te pillo en mitad del día, ¿me das 30 segundos para contarte por qué te llamo y tú decides si colgamos o seguimos?"*
2. **El Problema Central (15s):**  
   *"Hablo a diario con responsables de infraestructura que tienen clústeres de Kubernetes en AWS o GCP donde el 40% del cómputo pagado está completamente ocioso porque los desarrolladores sobredimensionan CPU y memoria por miedo a picos de tráfico."*
3. **Nuestra Solución (15s):**  
   *"Desarrollamos un operador que autoescala y redimensiona pods en caliente sin reiniciarlos, evaluando el coste por petición y protegiendo el SLA de latencia p99, logrando un ahorro de hasta el 40% con amortización en 10 días."*
4. **Llamada a la Acción (10s):**  
   *"No pretendo venderte nada hoy. Solo quiero proponerte una demo de 15 minutos este jueves para mostrarte la simulación con vuestro tamaño de flota. ¿Cómo tienes la agenda el jueves por la mañana?"*
