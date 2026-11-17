# Notas: cómo se comporta un bot de WhatsApp en grupos y en privados

Borrador armado con lo que mostró el registro de eventos durante la etapa de exploración (noviembre de 2026). Cada punto sale de líneas reales del registro (`component=event` y `component=responder`). Los identificadores reales se reemplazaron por marcadores. Las conclusiones quedan para completar.

## Contexto de las pruebas

- `bottero`, un binario en Go sobre whatsmeow (commit `2ecca2c`, 16/11/2026), vinculado como dispositivo al número dedicado. Corre como servicio de systemd en el homelab.
- Dos números: el dedicado (el bot) y el personal.
- Dos grupos propios con solo esos dos números: uno habilitado y otro no.

## Identidades del bot

- Al conectar, el bot tiene dos identidades, las dos con número de dispositivo: `<número>:2@s.whatsapp.net` y `<lid>:2@lid`. El teléfono es el dispositivo 0; el bot es otro dispositivo vinculado.
- Cuando otros lo mencionan o lo citan, la identidad llega sin número de dispositivo (`<lid>@lid`). Para reconocer al bot hay que comparar sin el dispositivo.
- Los mensajes que manda el bot tienen ids que empiezan con `3EB0` (los genera whatsmeow); los del iPhone empiezan con `3A`. Sirve para distinguirlos en el registro.

## Cómo llegan los identificadores

| Campo | Privado | Grupo |
|---|---|---|
| `chat` | LID de la otra persona (`<lid>@lid`) | JID del grupo (`<id>@g.us`) |
| `sender` | LID | LID |
| `sender_alt` | número (`<número>@s.whatsapp.net`) | número |
| `addressing` | vacío | `lid` |

- En los dos casos, el número de la persona llega solo en `sender_alt`. Reconocer a alguien por su número (la lista `allowed_people`) funcionó siempre por `sender_alt` (`matched_via=sender_alt`); nunca hizo falta la relación LID → número que guarda whatsmeow.
- El aviso de entrada a un grupo (`JoinedGroup`) trae el modo de direccionamiento del grupo (`lid`) y quién agregó al bot, como LID y como número (`sender_pn`).
- Los avisos de recepción (`Receipt`) traen el remitente como LID, sin `sender_alt`.
- Los mensajes que el teléfono del número dedicado le manda al bot (sincronización entre dispositivos) llegan con `from_me=true`, `kind=protocol` y el LID propio como chat y remitente.

## Menciones

- Mencionar al bot convierte el mensaje en texto extendido (`kind=extended_text`). La lista de mencionados (`mentions`) trae el LID del bot sin dispositivo.
- Dentro del texto, la mención aparece como `@` seguido del LID del bot, no del número: `@<lid> ping`. Para comparar con las palabras clave hay que sacarla.
- En el pie de una imagen pasa lo mismo: `caption=@<lid>`.
- En todos los casos observados, la mención llegó como LID (`mentioned_as=lid`). No vimos ningún grupo con direccionamiento por número.

## Citas (respuestas a un mensaje)

- whatsmeow trae el id y el autor del mensaje citado. Si se cita un mensaje del bot, el autor llega como el LID del bot sin dispositivo, tanto en grupo como en privado (`quoted_bot=lid`). El id es el de la respuesta que mandó el bot.
- Citar sin etiquetar no es una mención: con la regla actual, en un grupo eso no hace que el bot responda.

## Qué recibe el bot de un grupo

- El bot recibe y descifra todos los mensajes de todos los grupos en los que está, esté habilitado o no el grupo y lo etiqueten o no. La regla de responder solo cuando lo etiquetan limita lo que hace, no lo que recibe. En el grupo no habilitado, los mensajes quedaron en el registro con sus metadatos.
- Para la privacidad, eso significa que todo lo que se dice en un grupo donde está el bot pasa por su proceso. Lo que se puede decidir es qué se guarda.
- El primer mensaje de cada miembro después de que el bot entra a un grupo llega como dos eventos con el mismo id: uno trae la clave que ese miembro le pasa al bot para poder leer sus mensajes en el grupo (`kind=sender_key`) y el otro trae el contenido. Pasa una vez por miembro.

## Avisos de recepción

- Después de cada respuesta del bot llegan tres avisos:
  - `sender`: la respuesta llegó al otro dispositivo de la cuenta del bot, el teléfono del número dedicado;
  - `delivered`: llegó al destinatario;
  - `read`: el destinatario la leyó.

  En grupo llegan los mismos tres.
- En el chat propio del bot también aparecen avisos `peer_msg`, ligados a la sincronización entre dispositivos.

## Ediciones

- Una edición llega como un mensaje nuevo, con su propio id (distinto del original) y `edit=1`.
- El texto nuevo no viene donde va el de un mensaje común: la clasificación del registro la marcó `kind=other` y el texto no quedó registrado.
- Según el código de whatsmeow, las ediciones son mensajes de protocolo envueltos en un contenedor de "mensaje editado". Falta ver la estructura que llega para saber dónde vienen el texto nuevo y el id del mensaje original.
- El bot ignora las ediciones: no vuelve a responder.

## Archivos

Los metadatos vienen en el propio mensaje; el bot no descarga nada.

| Tipo | Lo que trae |
|---|---|
| Imagen | tipo (`image/jpeg`), tamaño, ancho y alto |
| Documento | tipo (`application/pdf`), tamaño, páginas y nombre del archivo |
| Video | tipo (`video/mp4`), tamaño, duración, ancho y alto |
| Nota de voz | tipo (`audio/ogg; codecs=opus`), tamaño, duración y la marca de nota de voz |
| Sticker | tipo (`image/webp`), tamaño y si es animado |

- Las fotos de ver una sola vez no les llegan a los dispositivos vinculados. Llega solo un aviso sin contenido (`UndecryptableMessage` con `is_unavailable=true`, tipo `view_once`). whatsmeow le pide el mensaje al teléfono, pero el teléfono no lo manda. El bot no recibe ni contenido ni metadatos.
- Para que un modelo procese un archivo, el bot tendría que descargarlo; una nota de voz, además, habría que transcribirla.

## Desconexiones y reentregas

- Los mensajes que se mandan mientras el bot está desconectado llegan al reconectar, entre `OfflineSyncPreview` y `OfflineSyncCompleted`, con `offline_sync=true`. Las demoras observadas entre el momento del mensaje y su recepción fueron de 41 s y 23 s, incluidos los reintentos de reconexión.
- whatsmeow reintenta la conexión sumando 2 s de espera después de cada intento fallido.
- Una vez, una reentrega de un mensaje que ya se había descifrado fue descartada por la capa de cifrado ("Ignoring message", clave ya usada), sin emitir ningún evento. Con el buffer de mensajes descifrados activado, si un mensaje no se terminó de procesar, su reentrega vuelve a llegar al bot.
- No se llegó a observar el camino de envío fallido seguido de reentrega (`send_failed` y después `replied` o `duplicate`). Con la regla de firewall, los avisos que el bot confirma cortan la conexión antes de que llegue el mensaje.

## Sesión y primera conexión

- Emparejar por código desde una terminal SSH funcionó.
- La primera conexión después de emparejar sube 812 claves de cifrado. En el disco rígido de la HP, eso tardó más de 30 s.
- La sincronización inicial trajo el historial (3 conversaciones) y unos 2000 nombres de contactos. Guardar esos nombres, de a uno por escritura, tuvo la base ocupada en el disco rígido y un mensaje no se pudo descifrar (`SQLITE_BUSY`). Se pasó a `synchronous=NORMAL`; el problema no volvió a aparecer, aunque tampoco hubo otra sincronización grande.
- El teléfono mostró la carga del historial como "en pausa" para el dispositivo del bot. No afecta la recepción de mensajes nuevos.
- Consumo del servicio: 1,5 s de CPU en tres horas y 10,8 MB de memoria como máximo.

## Preguntas para la etapa siguiente

El documento de requerimientos deja estas preguntas abiertas. Debajo de cada una van las observaciones que se relacionan con ella.

**¿Un número por proyecto, o un solo número para varios grupos?**
- Un número recibe todo lo de todos sus grupos. Cada grupo se identifica por su JID, que es estable.
- La lista de grupos habilitados ya separa en qué grupos actúa el bot, sin necesidad de un número aparte.
- _A completar._

**¿Qué guarda el registro cuando el bot está en grupos con otras personas?**
- El bot recibe todo, así que lo único que se puede decidir es qué se guarda. Hoy guarda el texto solo en privados de personas de la lista y en grupos habilitados; el nombre del archivo y el pie cuentan como contenido.
- Los metadatos (quién, cuándo, en qué grupo, qué tipo de mensaje) se registran siempre.
- _A completar._

**¿Cómo se le pasa la documentación al modelo?**
- Esta etapa no deja observaciones sobre esto.

## Conclusiones

_A completar._

## Pendiente de observar

- Un grupo con direccionamiento por número.
- La estructura de una edición: dónde vienen el texto nuevo y el id original.
- El camino de envío fallido y reentrega.
- La relación entre la duración de una desconexión y la demora de los mensajes.
- La HP conectada por cable.
