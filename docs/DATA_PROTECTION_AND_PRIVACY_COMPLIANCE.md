# Auditoría y Certificación de Protección de Datos y Privacidad (Data Protection & Privacy Compliance)

Este documento certifica el cumplimiento normativo de **KubeCost Auto-HPA** frente a las principales leyes internacionales y regionales de protección de datos personales y seguridad de la información:

- **RGPD / GDPR** (Reglamento General de Protección de Datos — Reglamento UE 2016/679)
- **CCPA / CPRA** (California Consumer Privacy Act / California Privacy Rights Act — Cal. Civ. Code § 1798.100+)
- **HIPAA** (Health Insurance Portability and Accountability Act — 45 CFR Part 160/164)
- **LGPD** (Lei Geral de Proteção de Dados — Lei 13.709/2018 de Brasil)
- **Ley 19.628 / Proyecto de Nueva Ley de Protección de Datos Personales** (Chile) y normativas afines en Iberoamérica
- **AICPA SOC 2 Type II** (Criterios de Confidencialidad y Privacidad) e **ISO/IEC 27001 / 27701**

---

## 1. Resumen Ejecutivo de Cumplimiento Legal

> **Declaración de Conformidad:**  
> **KubeCost Auto-HPA opera exclusivamente sobre telemetría agregada de infraestructura de computación (milicores de CPU, MiB de RAM, tasas de peticiones HTTP por segundo y costos de cómputo en USD). El sistema NO recopila, NO procesa, NO almacena, NO transmite y NO tiene acceso a Datos de Carácter Personal (PII), Datos Sensibles ni Información Médica Protegida (ePHI).**

Gracias a su arquitectura de ejecución local (*in-cluster*) y aislamiento de red (*Zero-Egress*), el uso de KubeCost Auto-HPA:
1. **Queda fuera del ámbito material del tratamiento de datos personales** de conformidad con el **Artículo 2 y Artículo 4(1) del RGPD**.
2. **Cumple por diseño y por defecto con el Artículo 25 del RGPD (*Privacy by Design & by Default*)**.
3. **No realiza transferencias internacionales de datos transfronterizas** (Capítulo V del RGPD), eliminando cualquier riesgo derivado de sentencias como *Schrems II*.
4. **No requiere acuerdos de socio comercial (BAA) bajo HIPAA** para la inspección de cargas útiles de datos de salud.
5. **No vende ni comparte información personal** bajo la normativa CCPA/CPRA.

---

## 2. Matriz de Clasificación de Datos Tratados

Para verificar que ninguna categoría de datos regulados es accedida o retenida, se presenta el inventario exhaustivo de datos leídos e interpretados por el sistema:

| Categoría de Dato | Ejemplos Específicos | ¿Es Tratado por KubeCost Auto-HPA? | Justificación Técnica / Ubicación |
|---|---|:---:|---|
| **Datos Personales Directos (PII)** | Nombres, DNIs, pasaportes, direcciones postales, teléfonos | **NO** | El sistema no accede a capas de aplicación ni bases de datos. |
| **Identificadores Electrónicos** | Correos electrónicos, nombres de usuario, IDs de cuenta | **NO** | No interactúa con servicios de autenticación de usuarios finales. |
| **Datos de Red de Usuario** | Direcciones IP de clientes, cookies HTTP, User-Agents | **NO** | Los proxies y balanceadores filtran el tráfico; el motor solo recibe el contador agregado `http_requests_total`. |
| **Carga Útil de Aplicación (Payloads)** | Cuerpos HTTP POST/PUT, JSON de negocio, tarjetas de crédito | **NO** | No realiza intercepción de paquetes ni inspección profunda (*DPI*). |
| **Datos de Salud / Financieros (ePHI/PCI)** | Historias clínicas, números de cuenta, diagnósticos | **NO** | Totalmente fuera de alcance (*Out-of-Scope*). |
| **Telemetría de Infraestructura (No PII)** | CPU asignada (`cpu_millicores`), memoria en uso (`usage_mib`) | **SÍ** | Leído de la API de Kubernetes para calcular el redimensionamiento. |
| **Métricas Agregadas de Rendimiento** | Peticiones por segundo (`rps`), percentil de latencia (`p99_ms`) | **SÍ** | Obtenido de Prometheus en formato de agregación estadística abstracta. |
| **Costes Financieros de Cómputo** | Tarifa horaria de CPU/RAM (`$/core-hour`), costo por pod | **SÍ** | Obtenido de OpenCost/KubeCost para optimizar la ecuación económica. |

---

## 3. Análisis Artículo por Artículo bajo el RGPD (UE 2016/679)

### 3.1. Artículos 2 y 4(1) — Ámbito de Aplicación y Definición de Datos Personales
- **Marco Legal:** El RGPD aplica al tratamiento total o parcialmente automatizado de datos personales de personas físicas identificadas o identificables.
- **Conformidad:** Todas las métricas consumidas son valores numéricos escalares o flotantes (por ejemplo: `cpu: 250m`, `p99: 45ms`, `rps: 120`). Ningún dato permite, directa ni indirectamente mediante correlación, identificar a un ser humano. Los datos son **estrictamente anónimos y de infraestructura técnica**.

### 3.2. Artículo 5 — Principios relativos al Tratamiento
- **Limitación de la finalidad (Art. 5.1.b):** Los datos métricos se usan con el único propósito de calcular decisiones de autoescalado horizontal y vertical.
- **Minimización de datos (Art. 5.1.c):** El endpoint `/v1/evaluate` solo solicita los 6 a 8 parámetros numéricos necesarios para la fórmula matemática de costo y latencia.
- **Limitación del plazo de conservación (Art. 5.1.e):** El motor no almacena registros históricos de tráfico. Redis solo retiene estados de estabilización y caché con expiraciones (TTL) efímeras de 20 a 60 segundos.

### 3.3. Artículo 25 — Protección de Datos desde el Diseño y por Defecto (*Privacy by Design*)
- **Aislamiento en Red (*Air-Gap*):** El software no posee mecanismos de actualización automática ni telemetría que conecten con servidores de la empresa proveedora.
- **Registro Silencioso (*No Logging*):** El servidor HTTP de evaluación en Python suprime voluntariamente los logs de peticiones (`def log_message(self, *args): pass`), evitando que cadenas de texto accidentales queden guardadas en discos o gestores de logs (Elasticsearch/Datadog/Loki).
- **Protección de Credenciales:** Los tokens de acceso a Prometheus y KubeCost se leen de archivos montados en memoria (`/var/run/secrets`) y nunca se imprimen en logs ni se reflejan en el estado de los recursos de Kubernetes (`status.conditions`).

### 3.4. Artículo 28 y 29 — Encargado del Tratamiento (*Data Processor*)
- En despliegues estándar autogestionados (*Self-Hosted*), el cliente es el operador y custodio exclusivo de su infraestructura.
- Para formalizar la relación comercial y dar cumplimiento a los departamentos de compras legales corporativos, se proporciona una plantilla de **Acuerdo de Tratamiento de Datos (DPA)** con Cláusulas Contractuales Tipo (SCC).

### 3.5. Capítulo V (Arts. 44 a 49) — Transferencias Internacionales de Datos
- **Impacto *Schrems II* / Marco de Privacidad de Datos:** Dado que **ningún dato sale de la Virtual Private Cloud (VPC)** o clúster local del cliente, **no existen transferencias internacionales de datos hacia Estados Unidos ni terceros países**. El software respeta de forma absoluta la soberanía de los datos (*Data Residency*).

### 3.6. Artículos 15 a 22 — Derechos de los Interesados (Acceso, Rectificación, Supresión / ARCO)
- Puesto que no se procesa ni almacena información personal de usuarios finales, las solicitudes de ejercicio de derechos ARCO dirigidas a la organización del cliente **no afectan ni requieren intervención en KubeCost Auto-HPA**, al no existir registros de personas físicas.

---

## 4. Cumplimiento frente a Leyes de Privacidad de EE.UU. (CCPA / CPRA / HIPAA)

### 4.1. CCPA / CPRA (California)
- **No Venta de Datos (*"Do Not Sell or Share"*):** KubeCost Auto-HPA no monetiza, no vende y no transfiere información a redes de publicidad, corredores de datos (*data brokers*) ni terceros.
- **Exención de Rastreo:** No utiliza cookies, scripts de seguimiento, SDKs analíticos de terceros (como Google Analytics o Mixpanel) ni identificadores persistentes de dispositivos.

### 4.2. HIPAA / HITECH (Sector Sanitario y Cuidado de la Salud)
- **Cero Impacto sobre Información Médica Protegida (ePHI):** En clústeres de hospitales, aseguradoras o telemedicina, KubeCost Auto-HPA opera en la capa del hipervisor de contenedores (cgroups / subrecurso de escalado).
- **Compatibilidad con BAA:** No se requiere la firma de un *Business Associate Agreement* (BAA) para el software en sí mismo, ya que el software no actúa como receptor ni procesador de registros médicos, de acuerdo con las directrices del HHS (U.S. Department of Health and Human Services).

---

## 5. Medidas Técnicas y Organizativas de Seguridad (TOMs — Art. 32 RGPD)

De acuerdo con el Artículo 32 del RGPD (Seguridad del Tratamiento), KubeCost Auto-HPA incorpora controles técnicos verificados:

```
                  POLÍTICA DE AISLAMIENTO Y SEGURIDAD
 +-------------------------------------------------------------------+
 |  Kubernetes Cluster (VPC / On-Premise)                            |
 |                                                                   |
 |   +-----------------------------------------------------------+   |
 |   | PodSecurityStandard: Restricted                           |   |
 |   |  - runAsNonRoot: true (UID 65532)                         |   |
 |   |  - readOnlyRootFilesystem: true                           |   |
 |   |  - capabilities: drop [ALL]                               |   |
 |   |  - seccompProfile: RuntimeDefault                         |   |
 |   +-----------------------------------------------------------+   |
 |                                                                   |
 |   +-----------------------------------------------------------+   |
 |   | Cifrado en Tránsito y Control de Acceso                   |   |
 |   |  - mTLS estricto con validación de CA (TLS v1.2 / 1.3)   |   |
 |   |  - Tokens Bearer verificados mediante HMAC en tiempo cte  |   |
 |   |  - NetworkPolicy de Egress: Bloqueo de salida a Internet  |   |
 |   +-----------------------------------------------------------+   |
 |                                                                   |
 |   +-----------------------------------------------------------+   |
 |   | Integridad de Cadena de Suministro                        |   |
 |   |  - Firmas digitales criptográficas Sigstore Cosign        |   |
 |   |  - SBOM machine-readable en formatos SPDX / CycloneDX     |   |
 |   |  - 0 Vulnerabilidades Críticas / Altas (Trivy Scan)       |   |
 |   +-----------------------------------------------------------+   |
 +-------------------------------------------------------------------+
```

1. **Cifrado en Tránsito:** Soporte nativo para mTLS bidireccional (`TLS_CLIENT_CA`, `TLS_CERT`, `TLS_KEY`) con forzado de protocolo mínimo `TLSv1.2` para toda comunicación interna entre el Operador Go y el Motor Python.
2. **Defensa en Profundidad en Red:** Políticas de red de Kubernetes (`NetworkPolicy`) que aíslan el pod del motor y Redis para rechazar cualquier tráfico que no provenga del operador, y restringen la salida (*egress*) a los endpoints internos autorizados.
3. **Firmado Criptográfico e Inmutabilidad:** Los paquetes e imágenes en `ghcr.io` cuentan con atestación OIDC de GitHub Actions y firmas criptográficas verificables en el registro público de transparencia Rekor de Sigstore.

---

## 6. Procedimiento de Notificación de Brechas de Seguridad (Art. 33 y 34 RGPD)

A pesar de que el sistema no almacena datos personales, KubeCost Auto-HPA cuenta con un protocolo formal de respuesta ante vulnerabilidades de seguridad:
- **Canal de Divulgación Privada:** Vulnerabilidades de seguridad deben reportarse mediante el canal oficial en GitHub Security Advisories o vía correo seguro a `security@delacarrera.com`.
- **Compromiso de Notificación:** En caso de identificarse una vulnerabilidad crítica que pudiera comprometer la disponibilidad o integridad del clúster del cliente, el equipo de mantenimiento emitirá un parche y aviso de seguridad en un plazo **inferior a 24 horas**.

---

## 7. Certificación para Equipos de Compliance y Asesorías Jurídicas

Este documento, junto con el [Anexo de Tratamiento de Datos (DPA)](DATA_PROCESSING_ADDENDUM_TEMPLATE.md) y el [White Paper de Seguridad](ENTERPRISE_SECURITY_WHITE_PAPER.md), constituye el expediente formal de diligencia debida (*Vendor Due Diligence*) que puede ser incorporado directamente en las auditorías de cumplimiento SOC 2, ISO 27001 o evaluaciones de impacto de protección de datos (EIPD / DPIA).
