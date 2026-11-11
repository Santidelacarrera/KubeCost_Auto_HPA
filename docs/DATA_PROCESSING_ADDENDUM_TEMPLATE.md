# Enterprise Data Processing Addendum (DPA) Template
### Anexo de Tratamiento y Protección de Datos Personales (RGPD / CCPA)

Este **Acuerdo de Tratamiento de Datos** (*Data Processing Addendum* o "DPA") complementa el Contrato Marco de Licencia y Servicios (*Master Services Agreement* o "MSA") celebrado entre:

- **El Cliente:** `[Nombre Legal de la Empresa del Cliente]`, con domicilio en `[Dirección de la Empresa]`, actuando en calidad de **Responsable del Tratamiento** (*Data Controller*).
- **El Proveedor:** **KubeCost Auto-HPA Project / Santiago de la Carrera**, actuando en calidad de **Encargado del Tratamiento** (*Data Processor*).

---

## 1. Objeto y Ámbito de Aplicación

1.1. Este DPA regula los términos bajo los cuales el Proveedor pudiera tener acceso incidental a sistemas o telemetría del Cliente durante la prestación de servicios de soporte técnico enterprise, consultoría de arquitectura o mantenimiento del software **KubeCost Auto-HPA**.

1.2. El presente anexo se formula en estricto cumplimiento del **Artículo 28 del Reglamento General de Protección de Datos (RGPD — Reglamento UE 2016/679)**, la **Ley Orgánica 3/2018 (LOPD-GDD)** en España, la **Ley 19.628** en Chile, y los estándares de privacidad de la **CCPA/CPRA** en California.

---

## 2. Declaración de Ausencia de Datos de Carácter Personal (Zero-PII Covenant)

2.1. **Naturaleza del Software:** Ambas partes reconocen y aceptan expresamente que el software KubeCost Auto-HPA ha sido diseñado bajo los principios de **Privacidad desde el Diseño y por Defecto (*Privacy by Design & by Default*)**, operando de forma 100% local (*on-premises* / dentro de la VPC del Cliente).

2.2. **No Tratamiento de PII:** El software **NO** captura, recopila, transmite, almacena ni transfiere hacia el exterior datos identificativos de personas físicas, direcciones IP de clientes finales, credenciales de acceso, números de tarjeta de crédito (PCI-DSS) ni datos de salud (ePHI bajo HIPAA).

2.3. **Acceso Incidental en Soporte:** En el supuesto excepcional de que el personal de soporte técnico del Proveedor tenga acceso temporal a volcados de logs de depuración (*debug logs*) o capturas de pantalla proporcionadas voluntariamente por el Cliente para la resolución de un ticket de soporte P1/P2, dicho acceso se regirá de forma vinculante por las disposiciones del presente DPA.

---

## 3. Obligaciones del Encargado del Tratamiento (Proveedor)

De conformidad con el Artículo 28(3) del RGPD, el Proveedor se compromete a:

1. **Tratamiento según Instrucciones:** Tratar cualquier información únicamente siguiendo las instrucciones documentadas del Cliente.
2. **Deber de Confidencialidad:** Garantizar que todo el personal autorizado para gestionar solicitudes de soporte haya firmado acuerdos de estricta confidencialidad profesional o esté sujeto a una obligación legal estatutaria de confidencialidad.
3. **Medidas Técnicas y Organizativas (TOMs):** Implementar y mantener las medidas de seguridad técnicas descritas en el **Anexo B** para garantizar un nivel de seguridad adecuado al riesgo.
4. **Subencargados del Tratamiento (*Sub-processors*):**
   - El Proveedor **NO** utiliza subencargados externos de computación en la nube para procesar datos del Cliente.
   - Si en el futuro fuera necesario recurrir a un subencargado para servicios de soporte (por ejemplo, un sistema de tickets), se notificará previamente por escrito al Cliente con al menos treinta (30) días de antelación, otorgando al Cliente el derecho a formular objeciones.
5. **Asistencia al Responsable:** Colaborar con el Cliente para responder a posibles ejercicios de derechos de los interesados (Arts. 15-22 RGPD) en caso de que fuera técnicamente aplicable.
6. **Supresión o Devolución de Datos:** A la finalización de los servicios de soporte, eliminar o devolver todos los archivos temporales y registros compartidos por el Cliente, a menos que la legislación aplicable exija su conservación.

---

## 4. Medidas de Seguridad Técnicas y Organizativas (TOMs)

El Proveedor garantiza que el software y sus procesos de soporte mantienen los siguientes controles de seguridad conforme al **Artículo 32 del RGPD**:

- **Cifrado en Tránsito:** Implementación de cifrado TLS v1.2 / TLS v1.3 y soporte de autenticación mTLS con certificados X.509 de cliente entre el operador y el motor.
- **Principio de Mínimo Privilegio (RBAC):** Roles de Kubernetes estrictamente acotados a subrecursos necesarios (`deployments/scale`, `pods/resize`), sin permisos de lectura sobre `Secrets` o `ConfigMaps` sensibles.
- **Aislamiento de Carga de Trabajo:** Contenedores configurados con usuario no privilegiado (`UID 65532`), sistema de archivos raíz de solo lectura y políticas de red (`NetworkPolicy`) que bloquean el tráfico exterior.
- **Verificación de Integridad de la Cadena de Suministro:** Imágenes de contenedores firmadas criptográficamente con Sigstore Cosign y analizadas periódicamente para mantener **0 vulnerabilidades de severidad Alta o Crítica**.

---

## 5. Gestión y Notificación de Incidentes de Seguridad

5.1. Si el Proveedor detecta o tiene sospecha fundada de una vulnerabilidad de seguridad crítica o una brecha que comprometa la integridad del software instalado en el Cliente, lo notificará al Cliente por correo electrónico de seguridad dentro de un plazo **máximo de cuarenta y ocho (48) horas** tras tener conocimiento de la misma.

5.2. La notificación incluirá:
- Descripción técnica de la vulnerabilidad o incidente.
- Cargas de trabajo o versiones potencialmente afectadas.
- Medidas de mitigación inmediatas (por ejemplo, activación de `autoscaling.kubecost.io/pause="true"` o parche de actualización de imagen).

---

## 6. Transferencias Internacionales de Datos

6.1. **Cero Transferencia Transfronteriza:** El software se ejecuta en el centro de datos o región de nube seleccionada y controlada por el Cliente. Ningún dato técnico del clúster es transferido automáticamente a través de fronteras nacionales o fuera del Espacio Económico Europeo (EEE).

6.2. En la medida en que las actividades de soporte técnico internacional pudieran considerarse una transferencia transfronteriza, ambas partes acuerdan someterse a las **Cláusulas Contractuales Tipo (SCC)** aprobadas por la Comisión Europea (Decisión de Ejecución (UE) 2021/914).

---

## 7. Firmas de Conformidad

Este Acuerdo de Tratamiento de Datos entra en vigor en la fecha de firma del Formulario de Pedido (*Order Form*) o firma digital de las partes:

**Por el Cliente (Responsable del Tratamiento):**  
Razón Social: _____________________________________________  
Firma: ____________________________________________________  
Nombre: ___________________________________________________  
Cargo: ____________________________________________________  
Fecha: ____________________________________________________  

**Por el Proveedor (Encargado del Tratamiento):**  
Razón Social: KubeCost Auto-HPA Project / Santiago de la Carrera  
Firma: ____________________________________________________  
Nombre: Santiago de la Carrera  
Cargo: Lead Maintainer & Arquitecto Comercial  
Fecha: ____________________________________________________  

---

### Anexo A: Descripción del Flujo Técnico de Telemetría
- **Datos de Entrada:** Solicitudes y límites de CPU/memoria de pods, contadores numéricos de Prometheus (`http_requests_total`), histogramas agregados de latencia (`p99_ms`), tarifas horarias de facturación de nodo (`$/core-hour`).
- **Datos de Salida:** Números enteros de réplicas recomendadas y valores numéricos de redimensionamiento de milicores y MiB.
- **Colectivos de Interesados Afectados:** Ninguno (datos desvinculados de personas físicas).

### Anexo B: Resumen de Medidas Técnicas y Organizativas (TOMs)
- **Perfil de Seguridad:** Conforme al estándar Kubernetes *Restricted Pod Security Standard*.
- **Firmado Digital:** Verificable con `cosign verify --certificate-oidc-issuer https://token.actions.githubusercontent.com`.
- **Aislamiento Físico y Lógico:** Ingress y Egress controlados mediante NetworkPolicies nativas.
