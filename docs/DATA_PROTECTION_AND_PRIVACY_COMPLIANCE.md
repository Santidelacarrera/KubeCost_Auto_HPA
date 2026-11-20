# Protección de datos en despliegues de KubeCost Auto-HPA

La Ley 21.719 modifica la Ley 19.628 y crea la Agencia de Protección de Datos Personales. La reforma entra en vigencia el **1 de diciembre de 2026**. Esta guía identifica controles del producto y decisiones que debe tomar quien opere el servicio en Chile. La evaluación jurídica depende de las finalidades, las partes y el despliegue concretos. [Texto oficial y vigencia](https://www.bcn.cl/leychile/navegar?f=2026-12-01&i=1209272)

## Inventario para el responsable del despliegue

| Flujo | Qué revisar | Medida operativa |
|---|---|---|
| Cuentas y permisos | El almacén del motor conserva ID, nombre visible, rol, estado, historial de cambios y vínculos con credenciales. | Usar identificadores internos. Limitar la administración de usuarios y separar las credenciales por persona. |
| Telemetría y decisiones | Los nombres de workloads, namespaces, nodos y tenants pueden identificar indirectamente a personas. | Evitar nombres personales y RUT en identificadores de infraestructura. Configurar retención según finalidad. |
| Auditoría, alertas y exportaciones | Las aprobaciones identifican al actor. El colector, los receptores de alertas y Redis pueden mantener copias con políticas propias. | Documentar destino, región, acceso, plazo de conservación y procedimiento de búsqueda por tenant. |
| Facturación y pagos | Importaciones y conciliaciones almacenan identificadores financieros. Stripe Checkout intercambia referencias con el proveedor cuando se configura. | Inventariar proveedor, contratos y transferencias. Usar el checkout para introducir los datos de pago. |
| Infraestructura y respaldos | El ingreso HTTP, los registros del clúster, las instantáneas y los respaldos pueden conservar información adicional. | Cifrar los soportes, restringir lectores, fijar vencimientos y revisar las supresiones antes de reabrir un respaldo restaurado. |

## Acceso y rectificación del perfil

Una persona con una clave vinculada a su cuenta puede consultar `GET /v1/users/me`. El JSON contiene el identificador, el nombre visible, el rol, el estado, la fecha de creación y la versión. Para corregir el nombre, enviar a `POST /v1/users/me` un objeto como `{"display_name":"Alias operativo","version":1}`, usando la versión de la consulta. Ante un conflicto, volver a leer el perfil. La identidad y la organización se obtienen de la credencial.

El perfil cubre la cuenta operativa. Para una solicitud que abarque auditoría, soporte, pagos o copias externas, el responsable debe reunir también esos antecedentes y proteger los datos de terceros en su respuesta.

## Tramitación de derechos

1. Publicar un canal accesible también a antiguos usuarios y representantes. Registrar fecha, derecho invocado y sistemas afectados con un identificador de caso. Verificar identidad o representación de manera proporcional.
2. Asignar responsable y fecha de respuesta según el artículo 11. Separar solicitudes de bloqueo temporal para atención urgente. Mientras se resuelve una solicitud fundada de bloqueo, suspender el tratamiento alcanzado y responder dentro de dos días hábiles. Deshabilitar una cuenta sólo limita su acceso.
3. Buscar por organización y por identificadores verificados. Resolver acceso, rectificación, supresión, oposición, portabilidad o bloqueo según corresponda. Documentar las excepciones de conservación y los destinatarios a quienes deban comunicarse los cambios.
4. Entregar la respuesta por un canal seguro, registrar su entrega y fundamentar un eventual rechazo. Probar el circuito con datos ficticios antes de operar. [Derechos y procedimiento, arts. 4 a 11](https://www.bcn.cl/leychile/navegar?i=1209272)

## Retención, incidentes y proveedores

La tarea de retención se configura con `engine.retention.enabled` y `engine.retention.days`. Revisar el efecto primero con `POST /v1/maintenance/retention` y `{"days":30,"dry_run":true}`. Los treinta días son un ejemplo de configuración. La tarea abarca telemetría y mantiene marcas de deduplicación. Cuentas, auditoría, pagos, streams y respaldos requieren políticas propias. La publicación de decisiones en Redis se habilita con `telemetry.enabled`. Comprobar quién puede leer el stream compartido antes de activarla.

Ante una vulneración, contenerla, preservar evidencia con acceso restringido e identificar sistemas, categorías de datos y personas afectadas. El encargado debe escalar al responsable. Evaluar y registrar la procedencia de avisar a la Agencia y a los titulares conforme al artículo 14 sexies. Conservar cronología, decisiones y mensajes emitidos. [Ley 21.719, obligaciones de seguridad y reporte](https://www.bcn.cl/leychile/navegar?i=1209272)

Antes de producción, asignar responsables, publicar finalidades, bases de licitud y canales, verificar contratos con encargados y revisar destinos internacionales. Validar RGPD, CCPA/CPRA, HIPAA o LGPD por separado cuando correspondan. El equipo debe acreditar controles y prácticas con evidencia de su propia instalación.
