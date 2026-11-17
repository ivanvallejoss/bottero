# bottero

Bot de WhatsApp en Go construido sobre [whatsmeow](https://github.com/tulir/whatsmeow). En esta etapa es una herramienta de exploración: sirve para entender cómo se comporta un bot de este tipo en chats privados y en grupos. Es un solo binario: la sesión de WhatsApp y la lógica viven en el mismo proceso.

## Estado

Capacidades actuales: emparejamiento por código, sesión guardada en disco, registro de eventos, servicio de systemd con arranque automático y reconexión, y respuestas fijas por palabra clave en chats privados, sin responder dos veces el mismo mensaje. Todavía no actúa en grupos ni confirma archivos.

## Compilar (en la notebook)

Requiere Go 1.26 o más nuevo.

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o bottero ./cmd/bottero
```

Sin cgo el binario es estático y no depende de la glibc del homelab. `-s -w` quita la tabla de símbolos y la información de depuración para achicarlo; los stack traces de un panic siguen mostrando funciones y líneas.

## Instalar en el homelab (primera vez)

Desde la notebook:

```sh
scp bottero deploy/bottero.service deploy/config.example.json ivan@hp-g4-vallejos:~/
```

En el homelab:

```sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin bottero
sudo install -m 755 ~/bottero /usr/local/bin/bottero
sudo install -m 644 ~/bottero.service /etc/systemd/system/bottero.service
sudo install -d -m 755 /etc/bottero
sudo install -m 640 -o root -g bottero ~/config.example.json /etc/bottero/config.json
sudoedit /etc/bottero/config.json      # cargar números y palabras clave (ver "Configuración")
sudo systemctl daemon-reload
sudo systemctl enable --now bottero
```

El servicio corre como el usuario de sistema `bottero`. `/var/lib/bottero` es solo de ese usuario: systemd la crea si no existe y le cambia el dueño si ya existía. Guarda dos bases: `session.db` (la sesión de whatsmeow) y `state.db` (los mensajes ya respondidos).

## Actualizar el binario

```sh
# notebook
scp bottero ivan@hp-g4-vallejos:~/
# homelab
sudo install -m 755 ~/bottero /usr/local/bin/bottero
sudo systemctl restart bottero
```

`install` reemplaza el archivo aunque el servicio esté corriendo. Si cambia `deploy/bottero.service`, se copia igual a `/etc/systemd/system/` y se corre `sudo systemctl daemon-reload` antes de reiniciar.

## Configuración

Vive en `/etc/bottero/config.json` (dueño `root`, legible por el grupo `bottero`). Se edita con `sudoedit /etc/bottero/config.json` y se aplica con `sudo systemctl restart bottero`. El repo solo tiene el ejemplo `deploy/config.example.json`; la configuración real no se sube porque tiene números de teléfono.

```json
{
  "keywords": { "hola": "¡Hola! ...", "ping": "pong" },
  "default_reply": "No entendí. ...",
  "allowed_people": ["549XXXXXXXXXX"],
  "enabled_groups": []
}
```

- `keywords`: palabra clave → respuesta fija. Un mensaje coincide si su texto completo es igual a la palabra clave, sin distinguir mayúsculas ni espacios en los extremos.
- `default_reply`: respuesta cuando etiquetan al bot en un grupo sin palabra clave. Se usa a partir de la capacidad de grupos.
- `allowed_people`: números a los que el bot responde en privado, en el mismo formato que `pair` (solo dígitos: 549 + código de área + número). El remitente se busca primero como número, después en el identificador alternativo (`sender_alt`) y por último en la relación LID → número que guarda whatsmeow.
- `enabled_groups`: JID de los grupos habilitados (`...@g.us`). Vacío hasta la capacidad de grupos.

Si el archivo falta, no es JSON válido, tiene un campo desconocido o un valor inválido (por ejemplo, el `549XXXXXXXXXX` del ejemplo), el bot termina con código 4 y systemd no lo reinicia. El motivo queda en el journal.

## Respuestas y deduplicación

En un chat privado, el bot responde si el mensaje es de texto, no es del propio número, no es una edición, el remitente está en `allowed_people` y el texto completo coincide con una palabra clave. Responde con texto plano al mismo chat, sin citar el mensaje, sin marcarlo como leído y sin mostrar "escribiendo". Todo lo demás es silencio. Los mensajes que llegaron mientras el bot estaba desconectado se responden igual.

Cada mensaje respondido queda registrado por su id en `state.db`:

1. Antes de responder, el bot registra el id. Si ya estaba, es una reentrega y no responde.
2. Envía la respuesta.
3. Si el envío falla, borra el registro y le avisa a whatsmeow que el mensaje no se procesó. whatsmeow no confirma el mensaje al servidor, que lo vuelve a entregar al reconectar, y el bot reintenta.

whatsmeow guarda cada mensaje descifrado hasta que el bot termina de procesarlo, así que una reentrega después de una caída llega de nuevo al bot en vez de descartarse.

El historial que manda el teléfono llega como eventos `HistorySync`, no como mensajes, y nunca se responde.

## Emparejar

El servicio no empareja: si no hay sesión, termina y systemd no lo reinicia. Antes de emparejar, cerrá desde el teléfono cualquier sesión vinculada anterior del número dedicado.

```sh
sudo systemctl stop bottero
sudo -u bottero /usr/local/bin/bottero pair -phone 549XXXXXXXXXX
sudo systemctl start bottero
```

El número va con código de país, 9, código de área y número, sin `+` y sin `15`. El comando imprime un código de 8 caracteres. En el teléfono del número dedicado: Dispositivos vinculados → Vincular un dispositivo → Vincular con número de teléfono, y escribís el código. Dura unos dos minutos. Cuando el teléfono lo acepta, el comando termina. La primera conexión, que sube las claves de cifrado iniciales, la hace el servicio al arrancar y puede tardar unos segundos.

Si ya hay una sesión guardada, `pair` no hace nada. Para volver a emparejar: desvincular el dispositivo desde el teléfono, detener el servicio y borrar la base (`sudo rm /var/lib/bottero/session.db*`).

## Correr a mano

Para correrlo fuera del servicio, primero hay que detener el servicio: dos procesos con la misma sesión hacen que WhatsApp corte uno de los dos.

```sh
sudo systemctl stop bottero
sudo -u bottero /usr/local/bin/bottero run
```

## Reinicios y reconexión

- Ante una caída de red, whatsmeow reconecta solo y espera 2 segundos más después de cada intento fallido. Si al arrancar no hay red, sigue intentando de la misma forma.
- Si el proceso termina con error, systemd lo reinicia a los 10 segundos.
- Código de salida 3: no hay sesión, o WhatsApp la cerró de forma definitiva (desvinculado desde el teléfono, baneo temporal, otro proceso con la misma sesión, cliente desactualizado). systemd no lo reinicia, porque reintentar no lo arregla; el motivo queda en el journal.
- Código de salida 4: la configuración falta o es inválida. Tampoco se reinicia.

## Registro de eventos

El bot escribe en stdout una línea por evento, en formato `clave=valor`. Como servicio, esa salida va al journal del sistema. El campo `component` separa cuatro fuentes:

- `component=event`: cada evento que entrega whatsmeow.
- `component=responder`: una línea por cada decisión sobre un mensaje que el bot respondería (ver abajo).
- `component=whatsmeow`: logs internos de la biblioteca, con `module` indicando la parte (Client, Socket, Database...).
- `component=bottero`: mensajes del propio bot (arranque, configuración cargada, apagado, errores). La línea de arranque muestra la versión de whatsmeow compilada.

```sh
journalctl -u bottero -o cat -f                         # en vivo
journalctl -u bottero -o cat -b                         # desde el último arranque de la máquina
journalctl -u bottero -o cat | grep component=event     # solo los eventos
```

`-o cat` muestra las líneas tal como las escribe el bot, sin el prefijo del journal. El journal sobrevive a los reinicios si existe `/var/log/journal`.

El registro guarda metadatos de todos los mensajes. El texto (campo `text`) se agrega solo en los mensajes privados de personas de `allowed_people`.

### Decisiones (`component=responder`)

Solo para mensajes privados de texto de personas de la lista. El campo `outcome` dice qué pasó:

| `outcome` | Qué significa |
|---|---|
| `replied` | Respondió. Incluye `keyword`, `matched_via` (por qué identificador se reconoció al remitente: `sender`, `sender_alt` o `lid_map`) y `reply_id`, el id del mensaje enviado. |
| `no_match` | El texto no coincide con ninguna palabra clave. Silencio. |
| `duplicate` | El mensaje ya se había respondido; es una reentrega. Silencio. |
| `send_failed` | El envío falló. Se reintenta en la próxima entrega del mensaje. |
| `dedup_error`, `release_error` | Error de la base `state.db`. |

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
| `text` | Texto del mensaje. Solo en privados de personas de `allowed_people`. |

### Otros eventos con campos propios

- `UndecryptableMessage`: los mismos metadatos de un mensaje, más `is_unavailable` y `decrypt_fail_mode`. whatsmeow le pide al remitente que lo reenvíe, así que el mismo `id` puede aparecer después como `Message`.
- `Connected`: `self_pn` y `self_lid`, las dos identidades del bot.
- `Receipt`: chat, remitente, `receipt_type` (`delivered`, `read`, ...), cantidad de mensajes y `msg_time`.
- `OfflineSyncPreview` / `OfflineSyncCompleted`: inicio y fin de la puesta al día, con los totales que anuncia el servidor.
- `HistorySync`: tipo de sincronización, cantidad de conversaciones, número de tramo y progreso.
- `JoinedGroup` / `GroupInfo`: JID del grupo (de ahí salen los JID para la lista de grupos habilitados), quién hizo el cambio y, al entrar, el modo de direccionamiento del grupo.
- Desconexiones definitivas (`LoggedOut`, `StreamReplaced`, `TemporaryBan`, ...): el motivo.

El resto de los eventos se registra solo con su tipo.
