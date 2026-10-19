# Streaming video

Nexo riproduce i video MP4 di un bucket come **stream HLS**, impacchettati al volo: niente
transcodifica e niente da preparare. Un video caricato con `nexoctl cp`, dalla console o da qualsiasi
client S3 si riproduce subito nella console, e un **link di streaming** lo condivide con chi non ha un
account, in un browser o in qualsiasi player HLS.

## Video supportati

| | |
| --- | --- |
| Contenitore | MP4 e varianti (`.mp4`, `.m4v`, `.mov`), con l'indice (`moov`) all'inizio o alla fine |
| Video | H.264 (AVC) o H.265 (HEVC) |
| Audio | AAC, facoltativo |

I file MP4 frammentati, gli oggetti cifrati con SSE-C e gli altri codec sono rifiutati con `400`:
condividili come file. Gli oggetti cifrati con SSE-S3 o SSE-KMS vanno in streaming come gli altri.

I segmenti sono tagliati sui keyframe del video, circa ogni 6 secondi: un video codificato con un
keyframe ogni 1-2 secondi (il default della maggior parte degli encoder e delle videocamere) si sposta
con precisione.

La sorgente viene riprodotta così com'è. Per qualità ridotte su cui i player passano con reti lente, un
poster e le miniature della timeline, usa il [media worker](media-worker.md): la master playlist elenca allora
le sue rendition accanto alla sorgente.

## Nella console

L'anteprima di un video MP4 nel browser degli oggetti è uno stream HLS: la riproduzione parte subito
e spostarsi nel video non scarica il file. I browser senza supporto HLS ripiegano sul file.

## Link di streaming

Un link di streaming è un [link di condivisione gestito](security/sharing.md): appartiene a chi lo crea,
scade (al massimo 7 giorni) e si revoca come ogni condivisione. Fornisce due URL:

- la **pagina del player** del server, per le persone: `https://s3.example.com/nexo/media/player/#/nexo/media/v1/…`;
- la **playlist HLS**, per i player (VLC, Safari, app mobili) e per le pagine che incorporano il video con
  [hls.js](https://github.com/video-dev/hls.js): `https://s3.example.com/nexo/media/v1/<bucket>/master.m3u8?…`.

Nella console, **Condividi** su un video propone **Condividi come video** (attivo di default per i file
MP4). Con la CLI:

```console
$ nexoctl share download --stream --expire 7d local/videos/talk.mp4
URL: local/videos/talk.mp4
Expire: 7d
Share: https://s3.example.com/nexo/media/v1/videos/master.m3u8?key=talk.mp4&X-Amz-Algorithm=…
Player: https://s3.example.com/nexo/media/player/#/nexo/media/v1/videos/master.m3u8?key=talk.mp4&X-Amz-Algorithm=…
ID: 2xk4m7q9fz3a (revoke with: nexoctl share revoke local 2xk4m7q9fz3a)
```

Revocare la condivisione ferma la riproduzione: playlist e segmenti sono firmati dalla condivisione,
fino alla sua scadenza. Un link è legato alla versione del video per cui è stato creato: se l'oggetto
viene sovrascritto, un player aperto si ferma (`412`) e un nuovo link riproduce la nuova versione.

La pagina del player e le playlist sono servite dal listener S3, quindi un link di streaming funziona
anche quando su Internet è esposta solo l'API S3. La pagina non contiene dati: la playlist viaggia nel
frammento del suo URL (dopo `#`), che i browser non inviano mai a un server.

## API HLS

Gli stream seguono l'autorizzazione di `GetObject`: chi può leggere un oggetto può riprodurlo.

| Percorso (listener S3) | Contenuto |
| --- | --- |
| `/nexo/media/v1/{bucket}/master.m3u8?key=…[&versionId=…]` | master playlist |
| `/nexo/media/v1/{bucket}/media.m3u8?key=…&rev=…` | media playlist (VOD) |
| `/nexo/media/v1/{bucket}/init.mp4?key=…&rev=…` | segmento di inizializzazione fMP4 |
| `/nexo/media/v1/{bucket}/segment.m4s?key=…&rev=…&n=…` | segmento fMP4 `n` |
| `/nexo/media/player/` | pagina pubblica del player |

Una richiesta può essere anonima (un bucket leggibile da tutti), firmata negli header o presigned. Gli
URI di una master playlist presigned sono a loro volta presigned con le stesse credenziali, fino alla
stessa scadenza: qualsiasi master playlist presigned (`nexoctl share download --raw` del percorso della
master, un SDK) si riproduce in un player senza credenziali. `rev` è l'ETag dell'oggetto: gli URI
rispondono `412` quando cambia.

La console serve la stessa presentazione ai suoi utenti sotto
`/api/v1/buckets/{bucket}/objects/hls/` con la loro sessione ([Admin API](reference/admin-api.md)).

L'impacchettamento costa poco al server: circa 0,3 ms per indicizzare una versione alla prima richiesta (poi in
cache per ETag), 0,1 ms per segmento più la sua lettura a intervallo ([benchmark](https://github.com/R3n3r0/Nexo/blob/main/docs/bench/media.md)).

Gli stream video contano nei [limiti di richieste](configuration.md#limiti-di-richieste) dell'API S3, a differenza degli
altri percorsi `/nexo/` (probe di salute, metriche).
