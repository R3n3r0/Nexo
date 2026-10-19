# Media worker: rendition, poster e miniature

Lo [streaming video](video-streaming.md) riproduce il file sorgente di un video così com'è. Il **media
worker** (`nexo-media-worker`) ricava altro dai video selezionati da una regola del bucket:

- **rendition**: qualità più basse (H.264/AAC) che la master playlist HLS offre accanto alla sorgente, così i
  player passano a quelle sulle reti lente;
- un **poster**: un fotogramma JPEG mostrato prima della riproduzione;
- **miniature della timeline**: uno sprite di piccoli fotogrammi con la sua mappa WebVTT, per le anteprime
  della barra di avanzamento.

Il worker è un programma separato, sulla stessa macchina o su un'altra. Nexo non decodifica mai video: manda
i job al worker, e il worker legge i video e scrive i suoi output attraverso l'API S3, con credenziali
proprie. Un deployment senza worker continua a fare streaming dei file sorgente.

## Come funziona

```text
 PUT video ──► nexo ── job (notify_media_worker) ──► nexo-media-worker
                 ▲                                         │ GET del video (API S3)
                 │                                         │ decodifica, scala, codifica (OpenH264)
                 └──────── PUT degli output (API S3) ◄─────┘ .nexo-media/<key>/<version>/
```

1. Una **regola media** del bucket seleziona un video per prefisso e suffisso e dice cosa ricavarne.
2. Quando un video così viene creato (PUT, multipart, copia) Nexo accoda un job per il target
   `notify_media_worker`; quando viene cancellato, un job di pulizia. I job viaggiano nella coda persistente
   dei [target degli eventi](events-lifecycle.md): un worker spento li riceve quando torna su.
3. Il worker salva ogni job sul suo disco (con `fsync`) prima di confermarlo, quindi un riavvio del worker non
   ne perde nessuno; li elabora, ritenta i fallimenti temporanei con un backoff e scrive `status.json` nella
   directory sidecar della versione.
4. Nexo serve gli output solo finché `status.json` dice `done` **per l'ETag del contenuto attuale**: un video
   sovrascritto non mostra mai le rendition del precedente.

Il worker non modifica né cancella mai un video: scrive solo sotto il prefisso riservato `.nexo-media/` del
bucket, e la sua [policy](#1-credenziali-del-worker) può imporlo.

## Niente ffmpeg: OpenH264

Il worker è un unico programma Go senza ffmpeg. Decodifica e codifica H.264 con
[OpenH264](https://www.openh264.org/), il codec che Cisco pubblica come open source (BSD) **e** come binari
per cui Cisco paga le royalty dei brevetti H.264 a MPEG LA. L'uso senza royalty vale per i binari che l'utente
finale scarica da Cisco, quindi né il worker né la sua immagine includono la libreria: al primo avvio il worker
scarica la build della sua piattaforma da `ciscobinary.openh264.org` nella sua directory dati e ne verifica lo
SHA-256 con il valore fissato nel worker. Sono supportati Linux (amd64, arm64), macOS (amd64, arm64) e Windows
(amd64).

> OpenH264 Video Codec provided by Cisco Systems, Inc.

Senza accesso a Internet, esegui `nexo-media-worker fetch-openh264 --dir lib` su una macchina connessa della
stessa piattaforma (scarica e verifica la libreria e ne stampa il percorso), poi copia il file nella directory
`lib` della directory dati del worker, oppure ovunque e passane il percorso con `--openh264`. Funziona anche una libreria compilata dai sorgenti, ma non è coperta dalla licenza dei brevetti
di Cisco.

## Video supportati

| | |
| --- | --- |
| Contenitore | MP4 (`.mp4`, `.m4v`, `.mov`), non frammentato |
| Video | H.264 (AVC), qualsiasi profilo (B-frame compresi), 4:2:0 |
| Audio | AAC, copiato così com'è nelle rendition |

Un video che il worker non sa elaborare (H.265, un altro codec, un file danneggiato) riceve `status.json` con
stato `failed` e il motivo, e non viene ritentato; va comunque in streaming come file sorgente se il
[packager](video-streaming.md#video-supportati) lo supporta.

## Output

| Output | Contenuto |
| --- | --- |
| `renditions/<nome>.mp4` | H.264 Constrained Baseline + la traccia AAC della sorgente, MP4 progressivo, un keyframe ogni 2 secondi |
| `poster.jpg` | il keyframe a un decimo del video (al massimo a 10 secondi), largo al massimo 1280 pixel |
| `thumbnails/sprite.jpg` | riquadri larghi 160 pixel, 10 per riga, uno per intervallo (al massimo 500) |
| `thumbnails/thumbnails.vtt` | mappa WebVTT dello sprite (`sprite.jpg#xywh=x,y,w,h`) |
| `status.json` | stato del job: `processing` (con il passo in corso), `done` o `failed` |

Le rendition sono i preset qui sotto; quelli più alti della sorgente sono saltati (un video 720p non riceve il
1080p). La larghezza segue le proporzioni della sorgente.

| Preset | Altezza | Bitrate video |
| --- | --- | --- |
| `2160p` | 2160 | 16 Mbit/s |
| `1440p` | 1440 | 8 Mbit/s |
| `1080p` | 1080 | 5 Mbit/s |
| `720p` | 720 | 2,8 Mbit/s |
| `480p` | 480 | 1,4 Mbit/s |
| `360p` | 360 | 800 kbit/s |
| `240p` | 240 | 400 kbit/s |

Gli output di una versione stanno sotto `.nexo-media/<key>/<versionId>/` (`null` in un bucket non
versionato). La console e `nexoctl ls`, `find` e `tree` nascondono questo prefisso per default (`--media` lo mostra); l'API S3 lo elenca come
qualsiasi altro, quindi backup, mirror e replica conservano gli output insieme ai video. Contano nella quota
del bucket.

## Configurazione

### 1. Credenziali del worker

Crea un utente per il worker con questa policy (sostituisci `*` nelle risorse con i bucket delle tue regole
per restringerla ancora): legge i video e scrive solo sotto `.nexo-media/`.

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {"Effect": "Allow", "Action": ["s3:GetObject", "s3:GetObjectVersion"], "Resource": ["arn:aws:s3:::*/*"]},
    {"Effect": "Allow", "Action": ["s3:PutObject", "s3:DeleteObject", "s3:DeleteObjectVersion"],
     "Resource": ["arn:aws:s3:::*/.nexo-media/*"]},
    {"Effect": "Allow", "Action": ["s3:ListBucket", "s3:ListBucketVersions"], "Resource": ["arn:aws:s3:::*"],
     "Condition": {"StringLike": {"s3:prefix": [".nexo-media/*"]}}}
  ]
}
```

```console
$ nexoctl admin policy create local media-worker media-worker-policy.json
$ nexoctl admin user add local media-worker 'un-segreto-lungo-e-casuale' --policy media-worker
```

### 2. Avvio del worker

Con il binario:

```console
$ export NEXO_WORKER_S3_ENDPOINT=http://nexo.internal:9000
$ export NEXO_WORKER_ACCESS_KEY=media-worker NEXO_WORKER_SECRET_KEY='un-segreto-lungo-e-casuale'
$ export NEXO_WORKER_AUTH_TOKEN="$(openssl rand -hex 32)"
$ nexo-media-worker --data-dir /var/lib/nexo-media-worker
level=INFO msg="OpenH264 loaded" version=2.4.1 license="OpenH264 Video Codec provided by Cisco Systems, Inc."
level=INFO msg="nexo-media-worker started" address=:9600 s3=http://nexo.internal:9000 concurrency=1
```

Con Docker (costruisci l'immagine con `make docker-media-worker`; scarica OpenH264 nel suo volume `/data` al
primo avvio):

```console
$ docker run -d --name nexo-media-worker -p 9600:9600 -v media-worker:/data \
    -e NEXO_WORKER_S3_ENDPOINT=http://nexo:9000 \
    -e NEXO_WORKER_ACCESS_KEY=media-worker -e NEXO_WORKER_SECRET_KEY='un-segreto-lungo-e-casuale' \
    -e NEXO_WORKER_AUTH_TOKEN=... docker.io/paolor/nexo-media-worker
```

| Flag | Ambiente | Default | |
| --- | --- | --- | --- |
| `--s3-endpoint` | `NEXO_WORKER_S3_ENDPOINT` | | URL dell'API S3 di Nexo (obbligatorio) |
| `--access-key`, `--secret-key` | `NEXO_WORKER_ACCESS_KEY`, `NEXO_WORKER_SECRET_KEY` | | credenziali del worker (obbligatorie) |
| `--auth-token` | `NEXO_WORKER_AUTH_TOKEN` | | bearer token inviato da Nexo (obbligatorio) |
| `--address` | `NEXO_WORKER_ADDRESS` | `:9600` | indirizzo dell'endpoint dei job (`POST /jobs`, `GET /healthz`) |
| `--data-dir` | `NEXO_WORKER_DATA_DIR` | `nexo-media-worker` | coda dei job, file di lavoro, libreria scaricata |
| `--concurrency` | `NEXO_WORKER_CONCURRENCY` | `1` | job elaborati insieme (ognuno usa già tutti i core) |
| `--max-attempts` | `NEXO_WORKER_MAX_ATTEMPTS` | `5` | tentativi di un job che fallisce per un motivo temporaneo |
| `--openh264` | `NEXO_WORKER_OPENH264` | | percorso della libreria (default: scaricata da Cisco) |
| `--s3-region` | `NEXO_WORKER_S3_REGION` | `us-east-1` | regione delle firme |
| `--name` | `NEXO_WORKER_NAME` | nome host | nome del worker in `status.json` |
| `--log-level`, `--log-format` | `NEXO_WORKER_LOG_LEVEL`, `NEXO_WORKER_LOG_FORMAT` | `info`, `text` | log (`json` per i collector) |

`nexo-media-worker healthcheck` controlla un worker in esecuzione (l'`HEALTHCHECK` dell'immagine). La
directory dati deve avere spazio per il video in elaborazione e le sue rendition.

### 3. Invio dei job al worker

Il target `notify_media_worker` consegna i job, con le chiavi di un [webhook](events-lifecycle.md)
(`endpoint`, `auth_token`, `client_cert`, `client_key`, `tls_skip_verify`, `queue_dir`, `queue_limit`):

```console
$ nexoctl admin config set local notify_media_worker enable=on endpoint=http://media-worker.internal:9600/jobs auth_token=...
```

`nexoctl admin notify targets test local _:media_worker` invia un job di prova, che il worker conferma senza
elaborare nulla. A differenza dei target degli eventi, questo non richiede una notifica del bucket: sono le
regole media a decidere quali video riceve.

### 4. Regole media

**Il modo veloce: un bucket video.** Crea il bucket con l'opzione **Video HLS** e ogni video caricato riceve
tutte le rendition (da 240p a 2160p, fino all'altezza del video), un poster e le miniature ogni 10 secondi,
senza scrivere regole:

```console
$ nexoctl mb --with-hls local/video                # un nuovo bucket video
$ nexoctl media rule add local/video --hls          # lo stesso per un bucket esistente
```

Nella console è l'interruttore **Video HLS** di **Crea bucket**; nell'Admin API, `"hls": true` nel corpo di
`POST /buckets` (richiede `admin:SetBucketMedia` oltre a `s3:CreateBucket`). L'opzione è la regola
`{"id": "hls", "renditions": [tutti i preset], "poster": true, "thumbnails": {"interval": "10s"}}`: modificala o
aggiungi altre regole come sotto.

**Regole personalizzate.** Un bucket ha fino a 100 regole. Una regola seleziona le chiavi per `prefix` (vuoto: tutto il bucket) e
`suffixes` (default `.mp4`, `.mov`, `.m4v`, senza distinzione tra maiuscole e minuscole); si applica la prima
regola che corrisponde.

```json
{
  "rules": [
    {"id": "corsi", "prefix": "corsi/", "renditions": ["1080p", "720p", "360p"], "poster": true,
     "thumbnails": {"interval": "10s"}},
    {"id": "clip", "prefix": "clip/", "poster": true}
  ]
}
```

```console
$ nexoctl media set local/video media.json
$ nexoctl media rule add local/video --id clip --prefix clip/ --poster
$ nexoctl media get local/video
```

Nella console: bucket → **Video**. Le regole richiedono `admin:SetBucketMedia` per essere modificate e
`admin:GetBucketMedia` per essere lette; fanno parte dei metadati del bucket (export e import del cluster
compresi).

Una regola si applica ai video scritti dopo di essa; per elaborare quelli esistenti, o di nuovo dopo aver
cambiato una regola:

```console
$ nexoctl media reprocess local/video/corsi/intro.mp4
$ nexoctl media status local/video/corsi/intro.mp4
Video      : local/video/corsi/intro.mp4
State      : done (current)
Rule       : corsi
Source     : 1920x1080, 4m12s
Renditions : 1080p 1920x1080 5.1 Mbit/s, 720p 1280x720 2.9 Mbit/s, 360p 640x360 896 kbit/s
Poster     : yes
Thumbnails : 26 every 10s (160x90, 10 columns)
Updated    : 2026-10-19 13:04:06 UTC
```

## Come vengono serviti gli output

Quando un video è `done`, la sua master playlist HLS elenca le rendition dopo la sorgente, con banda,
risoluzione e codec: il player della console, i [link di streaming](video-streaming.md#link-di-streaming) e
qualsiasi player HLS le scelgono da soli. Poster e miniature hanno URL propri, con l'autorizzazione di
`GetObject` sul video come le playlist:

| Percorso (listener S3) | Contenuto |
| --- | --- |
| `/nexo/media/v1/{bucket}/poster.jpg?key=…` | poster |
| `/nexo/media/v1/{bucket}/thumbnails.vtt?key=…` | mappa WebVTT, con gli URI che puntano a `sprite.jpg` (prefirmati se la richiesta lo è) |
| `/nexo/media/v1/{bucket}/sprite.jpg?key=…` | sprite delle miniature |
| `/nexo/media/v1/{bucket}/status.json?key=…` | stato del job (anche per i video mai elaborati: `none`) |

La console serve gli stessi file sotto `/api/v1/buckets/{bucket}/objects/hls/` e lo stato sotto
`/api/v1/buckets/{bucket}/objects/media/status` ([Admin API](reference/admin-api.md)).

## Cancellazioni e versioni

- Cancellare definitivamente una versione di un video (un DELETE in un bucket non versionato, un DELETE con
  `versionId`, una scadenza del lifecycle) invia un job di pulizia che rimuove la sua directory sidecar, in
  tutte le sue versioni.
- Un delete marker conserva la versione, quindi i suoi output restano: sono ancora serviti per quel
  `versionId`.
- In un bucket versionato il worker rimuove le versioni dei propri output sostituite da una nuova
  elaborazione, così rielaborare un video non accumula vecchie rendition.
- Solo i video selezionati da una regola inviano job di pulizia: dopo aver rimosso una regola, gli output dei
  suoi video restano finché non li cancelli (`nexoctl rm --recursive --force local/video/.nexo-media/clip/`).

## Prestazioni

Misurate su una macchina virtuale con 4 vCPU Intel Xeon a 2,1 GHz (AVX-512, senza SMT), 15,7 GiB di RAM, un
disco virtio ext4, Ubuntu 24.04, Go 1.27.2 e OpenH264 2.4.1, su un video H.264 High 1080p di 20 secondi a 6 Mbit/s
con grana ([macchina e metodo](https://github.com/R3n3r0/Nexo/blob/main/docs/bench/media.md); `make bench-media` misura il tuo hardware e salva la sua descrizione con i
risultati):

| Job (sorgente H.264 1080p) | Velocità |
|----------------------------|----------|
| 720p + 360p + 240p | 3,2× il tempo reale (un video di 10 minuti in circa 3 minuti) |
| una rendition | 4–4,5× |
| poster | 44 ms |
| miniature, una al secondo | 74× il tempo reale |

Un job usa tutti i core: il video è diviso nelle sue immagini IDR in tratti di almeno 2 secondi, ognuno decodificato
e codificato sul proprio core, e i tratti sono uniti in ordine (il risultato è identico, byte per byte, con qualsiasi
numero di core). La velocità cresce con i core; un video più corto di 2 secondi, o con una sola immagine IDR, è un
solo tratto su un core.

## Scalabilità e sicurezza

- Ogni job decodifica il video una volta, su tutti i core (un tratto per core), e codifica le sue rendition in
  parallelo; `--concurrency` esegue più job insieme, utile per molti video brevi. Scala un worker in verticale (core, `--concurrency`) piuttosto che in
  orizzontale: ogni target `notify_media_worker:<id>` riceve tutti i job, quindi due target elaborerebbero ogni
  video due volte.
- Il decoder legge file non fidati: esegui il worker come utente non privilegiato (l'immagine usa l'UID
  65532), con limiti di risorse e con la policy minima qui sopra. Non servono diritti di amministrazione né
  accesso di rete oltre all'API S3 di Nexo e, al primo avvio, `ciscobinary.openh264.org`.
- L'endpoint dei job autentica Nexo con il bearer token (confrontato in tempo costante); mettilo dietro TLS
  (`client_cert`/`client_key` e un reverse proxy) quando la rete tra Nexo e il worker non è fidata.
