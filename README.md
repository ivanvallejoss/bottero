# bottero

Bot de WhatsApp que armé en Go sobre [whatsmeow](https://github.com/tulir/whatsmeow). Es un solo binario: la sesión de WhatsApp y la lógica de respuesta viven en el mismo proceso. Corre como servicio de systemd en mi homelab, `hp-g4-vallejos`, una HP con Ubuntu Server y disco rígido conectada por Wi-Fi. No expone puertos: solo abre conexiones salientes hacia WhatsApp.

## Qué hace

Responde con mensajes fijos que salen de la configuración. Nunca inicia una conversación.

No responde a mensajes del propio número, a ediciones ni a mensajes que no sean texto, imagen o documento. No marca mensajes como leídos ni muestra "escribiendo". Los mensajes que llegaron mientras el bot estaba desconectado se responden al reconectar, salvo los que la persona mandó hace más de 24 horas (ver "Mensajes pendientes").

- **Privados:** responde si el remitente está en `allowed_people` y el texto completo coincide con una palabra clave. Responde con texto plano, sin citar.
- **Grupos:** responde solo si el mensaje etiqueta al bot con una mención explícita (`@`) y el grupo está en `enabled_groups`. Responde a cualquier miembro, sin pasar por `allowed_people`. Saca del texto las menciones del bot (con su LID y con su número), sin tocar las de otro identificador que empiece con los mismos dígitos, y compara el resto con las palabras clave; si no coincide, responde con `default_reply`, o se queda en silencio si está vacío. La respuesta cita el mensaje que lo etiquetó. Citar un mensaje del bot sin `@` no cuenta como etiqueta.
- **Grupos no habilitados:** silencio total, aunque lo etiqueten.
- **Imágenes y documentos:** se confirman con `image_ack` o `document_ack` y las mismas reglas: en privado, a personas de `allowed_people`; en grupos habilitados, cuando el pie etiqueta al bot (y la confirmación cita el archivo). El pie no se compara con las palabras clave. El bot no descarga el archivo: los metadatos vienen en el propio mensaje. Video, audio, notas de voz y stickers solo se registran.

### Mensajes pendientes

whatsmeow entrega los mensajes de a uno: mientras un handler no termina, no procesa el siguiente. Por eso el bot separa recibir de responder.

**Recibir.** El handler aplica las reglas de arriba para saber si el bot atiende el mensaje. Si lo atiende, lo guarda en `state.db` y recién entonces le avisa a whatsmeow que el mensaje se procesó; whatsmeow lo confirma al servidor, que ya no lo vuelve a entregar. Del mensaje se guarda solo lo que usa la respuesta: el texto, los metadatos del archivo y, en grupos, lo que va en la cita.

- Cada mensaje se identifica por chat e id. Si ya estaba guardado (pendiente o atendido), es una reentrega y no se guarda de nuevo.
- Si la base falla, o si el handler entra en pánico, el bot le avisa a whatsmeow que el mensaje no se procesó. whatsmeow no lo confirma y el servidor lo vuelve a entregar en la próxima conexión. whatsmeow guarda cada mensaje descifrado hasta que el handler termina, así que esa reentrega llega de nuevo al bot en vez de descartarse.

**Responder.** Otra parte del proceso, el worker, toma los mensajes guardados de a uno, en el orden en que llegaron dentro de cada chat. Un mensaje que espera un reintento frena solo a los siguientes de su chat.

1. Si la persona lo mandó hace más de 24 horas, lo cierra sin responder (`expired`).
2. Decide la respuesta. Si no hay nada que responder, lo cierra (`no_match`).
3. Arma la respuesta, le fija un id y guarda las dos cosas antes de enviar. Si la persona mandó el mensaje hace 15 minutos o más, la respuesta empieza con `delay_notice`, en el mismo mensaje.
4. Envía. Si el envío sale bien, lo cierra.

Si el proceso muere entre enviar y anotar el resultado, al arrancar el worker reenvía la misma respuesta con el mismo id. Probado el 19/11/2026: el servidor acepta el segundo envío y en el teléfono se ve un solo mensaje, en privado y en grupo (iPhone; Android y WhatsApp Web sin probar). Por la misma razón, un cambio en la configuración no cambia una respuesta que ya está armada, y el aviso de demora se decide una sola vez: una respuesta que salió a tiempo en el primer intento no lo suma en un reintento.

Si el envío falla:

- **Sin conexión:** el intento no cuenta; el worker espera a que whatsmeow reconecte.
- **Errores que no se arreglan reintentando** (el destinatario no está en WhatsApp, el bot no está en el grupo, el grupo no existe, el bot no tiene permiso para escribir): lo cierra como `failed`.
- **El resto** (por ejemplo, la confirmación del servidor no llegó a tiempo): hasta 5 intentos en total, con esperas de 10 s, 30 s, 2 min y 10 min. Después, `failed`.

Al cerrar un mensaje se borran su contenido y su respuesta. Queda una marca, sin remitente ni contenido, durante 14 días (el plazo en que el servidor de WhatsApp puede volver a entregar un mensaje), para reconocer las reentregas. Cuando el worker se queda sin mensajes pendientes, trunca el WAL de `state.db`, donde quedan copias viejas de las páginas con el texto.

**Apagado.** Con `systemctl stop` o `restart`: el worker deja de tomar mensajes y termina el envío en curso (hasta 30 s); después el bot se desconecta, espera a que termine un handler en curso y cierra las bases. Un mensaje que llega en esos segundos queda guardado y se responde al arrancar.

El historial que manda el teléfono llega como eventos `HistorySync`, no como mensajes, y nunca se responde.

## Puesta en marcha

### Compilar (en la notebook)

Requiere Go 1.27.3 o más nuevo (lo fija `go.mod`).

```sh
git status --short    # tiene que salir vacío
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o bottero ./cmd/bottero
```

Sin cgo el binario es estático y no depende de la glibc del homelab. `-s -w` quita la tabla de símbolos y la información de depuración para achicarlo; los stack traces de un panic siguen mostrando funciones y líneas.

`go build` guarda en el binario el commit del que sale y si el árbol tenía cambios (`vcs.modified`). Cualquier línea de `git status --short` lo marca como modificado, también un archivo sin seguimiento. Con el árbol limpio, el binario corresponde a un commit exacto. `go vet ./...` revisa el código, pero no genera el binario.

### Instalar en el homelab (primera vez)

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

El servicio corre como el usuario de sistema `bottero`. `/var/lib/bottero` es solo de ese usuario: systemd la crea si no existe y le cambia el dueño si ya existía. Ahí quedan las dos bases (ver "Bases de datos").

### Configuración

Vive en `/etc/bottero/config.json` (dueño `root`, legible por el grupo `bottero`). Se edita con `sudoedit /etc/bottero/config.json` y se aplica con `sudo systemctl restart bottero`. El repo solo tiene el ejemplo `deploy/config.example.json`; la configuración real no se sube porque tiene números de teléfono.

```json
{
  "keywords": { "hola": "¡Hola! ...", "ping": "pong" },
  "default_reply": "No entendí. ...",
  "image_ack": "Recibí tu imagen: {mimetype}, {width}×{height}, {size}.",
  "document_ack": "Recibí tu archivo {name}: {mimetype}, {size}.",
  "delay_notice": "Perdón la demora, recién vuelvo a estar en línea.",
  "allowed_people": ["549XXXXXXXXXX"],
  "enabled_groups": []
}
```

- `keywords`: palabra clave → respuesta fija. Un mensaje coincide si su texto completo es igual a la palabra clave, sin distinguir mayúsculas ni espacios en los extremos.
- `default_reply`: respuesta cuando etiquetan al bot en un grupo habilitado y el resto del texto no es una palabra clave. Vacío significa silencio.
- `image_ack`, `document_ack`: confirmación de una imagen o de un documento (cualquier archivo enviado como documento). Marcadores que el bot completa con los metadatos del mensaje: `{mimetype}` y `{size}` en las dos; `{width}` y `{height}` en `image_ack`; `{name}` y `{pages}` en `document_ack`. Un dato que el mensaje no trae aparece como `?`. Un marcador que no corresponde al tipo hace que la configuración se rechace al arrancar. Vacía significa que ese tipo no se confirma.
- `delay_notice`: texto que va antes de una respuesta tardía (15 minutos o más desde que la persona mandó el mensaje), separado por una línea en blanco, en el mismo mensaje. Es fijo: la hora de WhatsApp ya muestra cuánto se demoró. Vacío significa que las respuestas tardías van sin aviso.
- `allowed_people`: números a los que el bot responde en privado, en el mismo formato que `pair` (solo dígitos: 549 + código de área + número). El remitente se busca primero como número, después en el identificador alternativo (`sender_alt`) y por último en la relación LID → número que guarda whatsmeow.
- `enabled_groups`: JID de los grupos donde el bot actúa (`...@g.us`). El JID de un grupo sale del registro: es el `chat` de cualquier mensaje de ese grupo.

Si el archivo falta, no es JSON válido, tiene un campo desconocido o un valor inválido (por ejemplo, el `549XXXXXXXXXX` del ejemplo), el bot termina con código 4 y systemd no lo reinicia. El motivo queda en el journal.

### Emparejar

El servicio no empareja: si no hay sesión, termina y systemd no lo reinicia. Antes de emparejar se cierra, desde el teléfono, cualquier sesión vinculada anterior del número dedicado.

```sh
sudo systemctl stop bottero
sudo -u bottero /usr/local/bin/bottero pair -phone 549XXXXXXXXXX
sudo systemctl start bottero
```

El número va con código de país, 9, código de área y número, sin `+` y sin `15`. El comando imprime un código de 8 caracteres, que se escribe en el teléfono del número dedicado: Dispositivos vinculados → Vincular un dispositivo → Vincular con número de teléfono. El código dura unos dos minutos; cuando el teléfono lo acepta, el comando termina. La primera conexión, que sube las claves de cifrado iniciales, la hace el servicio al arrancar; en el disco rígido de la HP tardó más de 30 s.

Si ya hay una sesión guardada, `pair` no hace nada. Para volver a emparejar: desvincular el dispositivo desde el teléfono, detener el servicio y borrar la base (`sudo rm /var/lib/bottero/session.db*`).

## Mantenimiento

### Actualizar el binario

Con el binario recién compilado en la notebook:

```sh
# notebook
scp bottero ivan@hp-g4-vallejos:~/
# homelab
sudo install -m 755 ~/bottero /usr/local/bin/bottero && rm ~/bottero
sudo systemctl restart bottero
```

`install` reemplaza el archivo aunque el servicio esté corriendo. Borrar `~/bottero` después evita volver a instalar una copia vieja. Si cambia `deploy/bottero.service`, se copia igual a `/etc/systemd/system/` y se corre `sudo systemctl daemon-reload` antes de reiniciar.

### Verificar qué binario corre

```sh
grep -ao 'vcs\.[a-z]*=[^[:space:]]*' /usr/local/bin/bottero | sort -u
grep -ao '?_pragma=[a-z_]*([^)]*)\(&_pragma=[a-z_]*([^)]*)\)*\(&_txlock=\(immediate\|deferred\|exclusive\)\)\?' /usr/local/bin/bottero
stat -c '%y' /usr/local/bin/bottero; systemctl show bottero -p ActiveEnterTimestamp
```

- `vcs.revision` tiene que coincidir con `git rev-parse HEAD` en la notebook, y `vcs.modified` tiene que ser `false`.
- El segundo comando muestra las cadenas de conexión de las dos bases tal como quedaron compiladas. La de la sesión es la que empieza con `foreign_keys(1)`.
- Si el servicio arrancó después de la fecha del binario, lo que corre es lo que está en disco.

Los dos `grep` sirven igual sobre el `bottero` de la notebook, antes de copiarlo.

### Tests

En la notebook; el homelab no tiene Go instalado:

```sh
go test ./...
```

Dos grupos de tests abren bases SQLite reales, y en el homelab muestran cómo responde el disco rígido:

- `internal/session/store_test.go`, con la cadena de conexión de la sesión: reproducen el choque entre conexiones que describe "Bases de datos".
- `internal/state`: la base de estado, incluido que el texto de un mensaje cerrado no quede en ningún archivo y cuánto espera un guardado del handler mientras el worker escribe.

```sh
# notebook
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -c -trimpath -o /tmp/session.test ./internal/session
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -c -trimpath -o /tmp/state.test ./internal/state
scp /tmp/session.test /tmp/state.test ivan@hp-g4-vallejos:~/
# homelab
mkdir -p ~/bottero-test
TMPDIR=~/bottero-test ./session.test -test.v -test.run 'TestOpenStore|TestSession|TestDeferred'
TMPDIR=~/bottero-test ./state.test -test.v
rm -rf ~/bottero-test session.test state.test
```

`TMPDIR` lleva las bases de prueba al disco rígido. El binario de test va a `/tmp` en la notebook porque `.gitignore` no lo cubre.

### Bases de datos

Las dos viven en `/var/lib/bottero`, en modo WAL (cada una con sus archivos `-wal` y `-shm`).

- **`session.db`**: la sesión de whatsmeow (credenciales, claves de cifrado, contactos, relación LID → número y los mensajes descifrados mientras el bot los procesa). La cadena de conexión agrega dos ajustes:
  - `_txlock=immediate`: cada transacción toma el bloqueo de escritura al empezar. whatsmeow descifra cada mensaje en una transacción que primero lee la sesión de cifrado y después la escribe. Con el `BEGIN` común, si otra conexión está escribiendo, SQLite le responde `SQLITE_BUSY` al instante, sin esperar el `busy_timeout`, y el mensaje no se descifra. Pasó en la sincronización inicial, mientras whatsmeow guardaba los nombres de contactos de a uno. `TestDeferredTransactionsFailAtOnce` documenta esa falla: si empieza a fallar después de actualizar el driver, SQLite cambió ese comportamiento y conviene revisar la razón de este ajuste.
  - `synchronous=NORMAL`: un commit no espera el fsync del disco; el fsync queda para los checkpoints. Un corte de luz puede deshacer los últimos commits que el sistema todavía no había escrito en el disco. La base no se corrompe, y una caída o un reinicio del proceso no pierden nada.
- **`state.db`**: los mensajes que esperan respuesta y las marcas de los ya atendidos (ver "Mensajes pendientes"). La cadena de conexión agrega:
  - `synchronous=FULL`: cada commit espera a que el WAL esté en disco. Un mensaje guardado ya está confirmado a WhatsApp, que no lo va a volver a entregar, así que el guardado tiene que sobrevivir a un corte de luz. En la HP, un guardado tarda unos 45 ms (p99 de 67 a 78 ms); mientras el worker escribe sin parar, unos 90 ms (p99 de 179 ms).
  - `secure_delete`: lo que se borra se pisa con ceros en el archivo de la base.
  - `_txlock=immediate`: por la misma razón que en la sesión. Hoy no hace falta, porque la base usa una sola conexión y el handler y el worker se turnan dentro del proceso.
  - La versión del esquema va en `user_version`. Al pasar de la versión anterior, los ids de la tabla `answered` se convierten en marcas con resultado `answered`.

### Reinicios y reconexión

- Ante una caída de red, whatsmeow reconecta solo y espera 2 segundos más después de cada intento fallido. Si al arrancar no hay red, sigue intentando de la misma forma.
- Si el proceso termina con error, systemd lo reinicia a los 10 segundos.
- Código de salida 3: no hay sesión, o WhatsApp la cerró de forma definitiva (desvinculado desde el teléfono, baneo temporal, otro proceso con la misma sesión, cliente desactualizado). systemd no lo reinicia, porque reintentar no lo arregla; el motivo queda en el journal.
- Código de salida 4: la configuración falta o es inválida. Tampoco se reinicia.

### Correr a mano

Antes se detiene el servicio: dos procesos con la misma sesión hacen que WhatsApp corte uno de los dos.

```sh
sudo systemctl stop bottero
sudo -u bottero /usr/local/bin/bottero run
```

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

El registro guarda metadatos de todos los mensajes. El texto (campo `text`) se agrega solo en los mensajes privados de personas de `allowed_people` y en todos los mensajes de los grupos de `enabled_groups`.

### Decisiones (`component=responder`)

Solo para mensajes privados de texto, imágenes o documentos de personas de la lista y para mensajes de grupo que etiquetan al bot. Los mensajes de grupo sin etiqueta no generan línea de decisión. Un mensaje que el bot atiende deja dos líneas: `queued` cuando el handler lo guarda, y la del resultado cuando el worker lo cierra. El campo `outcome` dice qué pasó:

| `outcome` | Qué significa |
|---|---|
| `queued` | El handler guardó el mensaje; el worker lo va a responder. En privados, `matched_via` dice por qué identificador se reconoció al remitente (`sender`, `sender_alt` o `lid_map`); en grupos, `mentioned_as` dice con cuál identidad del bot llegó la mención (`lid` o `pn`). |
| `replied` | Respondió. Incluye `keyword` (vacío si usó la respuesta por defecto), `default_reply`, `delay_notice` (si la respuesta llevó el aviso de demora), `reply_id` (el id del mensaje enviado), `attempt` (en qué intento salió) y `waited` (cuánto pasó desde que se guardó), además de `matched_via` o `mentioned_as`. |
| `file_ack` | Confirmó una imagen o un documento. `media` dice cuál (`image` o `document`); incluye `delay_notice`, `reply_id`, `attempt` y `waited`. Si la plantilla de ese tipo está vacía, la decisión es `no_match` con `media`. |
| `no_match` | El texto no coincide con ninguna palabra clave (y, en grupos, `default_reply` está vacío). Silencio. |
| `group_not_enabled` | Etiquetaron al bot en un grupo que no está en `enabled_groups`. Silencio; no se guarda. |
| `duplicate` | El mensaje ya estaba guardado; es una reentrega. Silencio. |
| `retry` | El envío falló y se va a reintentar. Incluye `attempt`, `err` y `next_attempt`. |
| `send_deferred` | No había conexión para enviar. El intento no cuenta; se reintenta al reconectar. |
| `failed` | El bot dejó de intentar: un error que no se arregla reintentando, o el quinto intento fallido. Incluye `attempt` y `err`. |
| `expired` | La persona lo mandó hace más de 24 horas. `age` dice cuánto. Silencio. |
| `save_error` | El handler no pudo guardar el mensaje. whatsmeow no lo confirma y el servidor lo vuelve a entregar en la próxima conexión. |
| `handler_panic` | El handler entró en pánico; incluye `panic` y `stack`. Igual que `save_error`, el mensaje vuelve en la próxima conexión. |
| `state_error` | Error de la base `state.db` en el worker. El mensaje queda como estaba y el worker lo vuelve a tomar a los 5 segundos. |

Con `msg=state` aparecen dos líneas más: `outcome=pruned` cuando se borran marcas de más de 14 días (`records` dice cuántas) y `outcome=state_error` cuando falla la base fuera de un mensaje.

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
| `quoted_id`, `quoted_from` | Si el mensaje cita a otro (una respuesta): id y autor del mensaje citado, tal como llegan. |
| `quoted_bot` | Presente si el mensaje citado es del bot: `lid` o `pn`, según la forma en que llegó el autor. |
| `mime`, `size` | En imágenes, documentos, video, audio y stickers: tipo de archivo y tamaño en bytes, tal como vienen en el mensaje. |
| `width`, `height` | Imágenes y video. |
| `pages` | Documentos (0 si el mensaje no lo trae). |
| `seconds`, `voice_note` | Duración de video y audio; `voice_note` indica una nota de voz. |
| `animated` | Stickers. |
| `has_caption` | Si el archivo vino con pie. |
| `caption`, `file_name` | Pie y nombre del archivo. Cuentan como contenido: solo donde también se registra `text`. |
| `text` | Texto del mensaje. Solo en privados de personas de `allowed_people` y en grupos de `enabled_groups`. |

### Otros eventos con campos propios

- `UndecryptableMessage`: los mismos metadatos de un mensaje, más `is_unavailable` y `decrypt_fail_mode`. whatsmeow le pide al remitente que lo reenvíe, así que el mismo `id` puede aparecer después como `Message`.
- `Connected`: `self_pn` y `self_lid`, las dos identidades del bot.
- `Receipt`: chat, remitente, `receipt_type` (`delivered`, `read`, ...), cantidad de mensajes y `msg_time`.
- `OfflineSyncPreview` / `OfflineSyncCompleted`: inicio y fin de la puesta al día, con los totales que anuncia el servidor.
- `HistorySync`: tipo de sincronización, cantidad de conversaciones, número de tramo y progreso.
- `JoinedGroup` / `GroupInfo`: JID del grupo (de ahí salen los JID para la lista de grupos habilitados), quién hizo el cambio y, al entrar, el modo de direccionamiento del grupo.
- Desconexiones definitivas (`LoggedOut`, `StreamReplaced`, `TemporaryBan`, ...): el motivo.

El resto de los eventos se registra solo con su tipo.
