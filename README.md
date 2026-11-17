# bottero

Bot de WhatsApp en Go construido sobre [whatsmeow](https://github.com/tulir/whatsmeow). En esta etapa es una herramienta de exploración: sirve para entender cómo se comporta un bot de este tipo en chats privados y en grupos. Es un solo binario: la sesión de WhatsApp y la lógica viven en el mismo proceso.

## Estado

Capacidad actual: emparejamiento por código, sesión guardada en disco y registro de eventos. Todavía no responde mensajes.

## Compilar (en la notebook)

Requiere Go 1.26 o más nuevo.

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o bottero ./cmd/bottero
```

Sin cgo el binario es estático y no depende de la glibc del homelab. `-s -w` quita la tabla de símbolos y la información de depuración para achicarlo; los stack traces de un panic siguen mostrando funciones y líneas.

## Instalar en el homelab

```sh
scp bottero hp-g4-vallejos:~/
ssh hp-g4-vallejos
sudo install -d -o ivan -g ivan -m 700 /var/lib/bottero
```

`/var/lib/bottero` guarda la base de la sesión (`session.db`). Se puede cambiar con `-data-dir`.

## Emparejar

Antes de emparejar, cerrá desde el teléfono cualquier sesión vinculada anterior del número dedicado.

```sh
./bottero pair -phone 549XXXXXXXXXX
```

El número va con código de país, 9, código de área y número, sin `+` y sin `15`. El comando imprime un código de 8 caracteres. En el teléfono del número dedicado: Dispositivos vinculados → Vincular un dispositivo → Vincular con número de teléfono, y escribís el código. Dura unos dos minutos. Cuando el teléfono lo acepta, el comando espera la primera conexión y termina.

Si ya hay una sesión guardada, `pair` no hace nada. Para volver a emparejar: desvincular el dispositivo desde el teléfono y borrar `session.db` (y sus archivos `-wal` y `-shm`).

## Correr

```sh
./bottero run
```

Se detiene con Ctrl-C. Al volver a correrlo reconecta con la sesión guardada, sin emparejar. Si WhatsApp cierra la sesión de forma definitiva (desvinculado desde el teléfono, baneo, otro proceso con la misma sesión, cliente desactualizado), el bot lo registra y termina con error.

## Registro de eventos

El bot escribe en stdout una línea por evento, en formato `clave=valor`. El campo `component` separa tres fuentes:

- `component=event`: cada evento que entrega whatsmeow.
- `component=whatsmeow`: logs internos de la biblioteca, con `module` indicando la parte (Client, Socket, Database...).
- `component=bottero`: mensajes del propio bot (arranque, apagado, errores). La línea de arranque muestra la versión de whatsmeow compilada.

Para ver solo los eventos: `./bottero run | grep component=event`.

En esta versión el registro guarda solo metadatos, nunca el contenido de los mensajes.

### Campos de un mensaje (`type=Message`)

| Campo | Qué es |
|---|---|
| `time` | Cuándo lo recibió el bot. |
| `msg_time` | El momento que WhatsApp le asigna al mensaje. |
| `offline_sync` | `true` si llegó durante la puesta al día después de reconectar (mensajes enviados mientras el bot estaba desconectado). |
| `scope` | `private`, `group`, `broadcast`, `status`, `newsletter` u `other`, según el servidor del JID del chat. |
| `chat` | JID del chat, tal como llega. |
| `sender` | JID del remitente, tal como llega (número `@s.whatsapp.net` o LID `@lid`). |
| `sender_alt` | La otra forma del remitente, si whatsmeow la trae. |
| `recipient_alt` | En privados, la otra forma del destinatario, si viene. |
| `addressing` | Modo de direccionamiento del mensaje: `pn` (número) o `lid`. |
| `from_me` | `true` si lo envió el propio número (desde el teléfono o desde el bot). |
| `id` | Identificador del mensaje. |
| `wa_type`, `media_type` | Tipo y tipo de medio que declara el propio mensaje en el protocolo. |
| `kind` | Qué parte del mensaje trae el contenido: `text`, `extended_text` (texto con menciones, respuestas o vista previa de enlaces), `image`, `document`, `video`, `audio`, `sticker`, `reaction`, `protocol`, etc. |
| `edit` | Presente si el mensaje es una edición, un mensaje fijado o una eliminación (el valor es el código que usa el protocolo). |
| `mentions` | JID mencionados, tal como llegan. |

### Otros eventos con campos propios

- `UndecryptableMessage`: los mismos metadatos de un mensaje, más `is_unavailable` y `decrypt_fail_mode`. whatsmeow le pide al remitente que lo reenvíe, así que el mismo `id` puede aparecer después como `Message`.
- `Connected`: `self_pn` y `self_lid`, las dos identidades del bot.
- `Receipt`: chat, remitente, `receipt_type` (`delivered`, `read`, ...), cantidad de mensajes y `msg_time`.
- `OfflineSyncPreview` / `OfflineSyncCompleted`: inicio y fin de la puesta al día, con los totales que anuncia el servidor.
- `HistorySync`: tipo de sincronización, cantidad de conversaciones, número de tramo y progreso.
- `JoinedGroup` / `GroupInfo`: JID del grupo (de ahí salen los JID para la lista de grupos habilitados), quién hizo el cambio y, al entrar, el modo de direccionamiento del grupo.
- Desconexiones definitivas (`LoggedOut`, `StreamReplaced`, `TemporaryBan`, ...): el motivo.

El resto de los eventos se registra solo con su tipo.
