# Media worker: rendition, poster, miniature e varianti delle immagini

Lo [streaming video](video-streaming.md) riproduce il file sorgente di un video così com'è. Il **media
worker** (`nexo-media-worker`) ricava altro dai video selezionati da una regola del bucket:

- **rendition**: qualità più basse (H.264/AAC) che la master playlist HLS offre accanto alla sorgente, così i
  player passano a quelle sulle reti lente;
- un **poster**: un fotogramma JPEG mostrato prima della riproduzione;
- **miniature della timeline**: uno sprite di piccoli fotogrammi con la sua mappa WebVTT, per le anteprime
  della barra di avanzamento.
- **varianti delle immagini**: ogni immagine selezionata da una regola in AVIF, WebP, JPEG e PNG a più
  larghezze, servita nel formato migliore per ogni client ([sotto](#immagini-ogni-formato-e-larghezza)).

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
| `renditions/<nome>.b.mp4` | la variante B di una rendition, per i video con la [filigrana forense](video-protection.it.md#6-filigrana-forense) |
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

### Filigrana forense

Quando la [protezione video](video-protection.it.md#6-filigrana-forense) di un bucket marca i suoi video
(`--forensic`), il worker codifica ogni rendition due volte dalle stesse immagini: `renditions/<nome>.mp4` porta un
motivo invisibile (±2 livelli di luminanza, una griglia morbida di segni casuali scelta per video) aggiunto
all'immagine, `<nome>.b.mp4` lo stesso motivo sottratto. Entrambe hanno gli stessi keyframe e segmenti; il server ne
sceglie una per ogni segmento di un flusso secondo il codice del token di riproduzione dello spettatore. Raddoppia il
tempo di codifica e lo spazio delle rendition. I video elaborati prima di attivare la filigrana forense vanno in
streaming solo dopo essere stati rielaborati (`nexoctl media reprocess`): un video marcato non va mai in streaming
senza marca.

`nexo-media-worker trace` rilegge la marca da una copia diffusa (un MP4 con video H.264) con le credenziali del
worker, e scrive i voti che `nexoctl media trace` trasforma nello spettatore a cui la copia era stata trasmessa:

```sh
nexo-media-worker trace --leak copia.mp4 --bucket lms --key corsi/intro.mp4 --out voti.json
nexoctl media trace local/lms/corsi/intro.mp4 --votes voti.json
```

`--offset` indica il momento del video mostrato dalla prima immagine di una clip; `--every` il tempo minimo tra due
voti (200 ms).

## Immagini: ogni formato e larghezza

Una **regola immagini** fa trasformare al worker ogni immagine selezionata in *varianti*: l'immagine alla sua
dimensione e a una serie di larghezze, in ogni formato della regola. I client chiedono la larghezza che
mostrano e Nexo serve il formato più compatto che sanno decodificare: AVIF ai browser recenti, WebP agli altri,
JPEG a tutti gli altri.

**Il modo veloce: un bucket di immagini.** Crea il bucket con l'opzione **Immagini in ogni formato** e ogni
immagine caricata riceve AVIF, WebP, JPEG e PNG, alla sua dimensione e larga 160, 320, 640, 1280, 1920 e 2560
pixel (solo le larghezze minori dell'immagine: un'immagine non viene mai ingrandita):

```console
$ nexoctl mb --with-images local/foto              # un nuovo bucket di immagini
$ nexoctl mb --with-hls --with-images local/media  # video e immagini nello stesso bucket
$ nexoctl media rule add local/foto --images       # lo stesso per un bucket esistente
$ nexoctl media rule add local/negozio --images --id prodotti --prefix prodotti/ --format webp,jpeg --width 320,800
```

Nella console è l'interruttore di **Crea bucket**; nell'Admin API, `"images": true` nel corpo di
`POST /buckets` (con `admin:SetBucketMedia`). L'opzione è la regola `{"id": "images", "images": {"formats":
["avif", "webp", "jpeg", "png"], "widths": [160, 320, 640, 1280, 1920, 2560]}}`. Una regola immagini non ha
rendition, poster né miniature; i suoi suffissi predefiniti sono quelli delle immagini qui sotto.

| | |
| --- | --- |
| Sorgenti | JPEG, PNG, GIF (primo fotogramma), WebP, AVIF, HEIC/HEIF (foto dell'iPhone), TIFF, BMP; riconosciute dal contenuto, non dall'estensione |
| Formati | `avif`, `webp`, `jpeg`, `png` (formati di una regola, predefinito tutti) |
| Larghezze | da 16 a 8192 pixel, al massimo 16 per regola (predefinite 160, 320, 640, 1280, 1920, 2560) |
| Orientamento | l'orientamento EXIF delle foto JPEG e TIFF viene applicato: le varianti sono dritte |
| Metadati | rimossi: le varianti non contengono EXIF, posizione GPS o dati della fotocamera |
| Trasparenza | mantenuta in AVIF, WebP e PNG; il JPEG viene appiattito su bianco, quindi un'immagine trasparente è servita come PNG ai client senza AVIF né WebP |

Le varianti stanno accanto all'immagine, come gli output di un video:
`.nexo-media/<chiave>/<versionId>/images/<larghezza>.<ext>` e `images/full.<ext>` per l'immagine alla sua
dimensione, con `status.json` che le elenca (`nexoctl media status` le stampa per formato).

### Come vengono servite le varianti

| Rotta | Per |
| --- | --- |
| `GET /api/v1/buckets/{bucket}/objects/image?key=...&w=640` | la console e i client dell'Admin API (sessione o SigV4), richiede `s3:GetObject` |
| `GET` o `HEAD /nexo/media/v1/{bucket}/image/{chiave}?w=640` | pagine web e app, sulla porta S3: SigV4, presigned o un link di condivisione, come gli [stream video](video-streaming.md) |

- `w`: la larghezza che il client mostra; viene servita la variante più stretta larga almeno così (la più
  larga se nessuna lo è). Senza `w`, l'immagine alla sua dimensione.
- `format`: `auto` (predefinito) sceglie il formato più compatto che l'header `Accept` nomina (AVIF, poi WebP;
  JPEG per `*/*`), e la risposta dice `Vary: Accept` per cache e CDN; `avif`, `webp`, `jpeg` o `png` ne
  chiedono uno.
- `versionId`: una versione dell'immagine (`s3:GetObjectVersion`).
- Le richieste condizionali (`If-None-Match`, `If-Modified-Since`) e i range funzionano; l'ETag nomina la
  sorgente, il formato e la larghezza.
- Una variante esiste solo finché `status.json` è done per il contenuto attuale dell'immagine: un'immagine
  sovrascritta risponde 404 finché il worker non ha elaborato la nuova.

Una pagina web lascia scegliere al browser:

```html
<img src="https://s3.example.com/nexo/media/v1/foto/image/spiaggia.jpg?w=640"
     srcset="https://s3.example.com/nexo/media/v1/foto/image/spiaggia.jpg?w=320 320w,
             https://s3.example.com/nexo/media/v1/foto/image/spiaggia.jpg?w=1280 1280w"
     sizes="(max-width: 700px) 100vw, 640px" alt="La spiaggia">
```

(per un bucket privato, firma gli URL o usa i link di condivisione). `nexoctl share download --image
local/foto/spiaggia.jpg` crea un link revocabile che serve l'immagine alla sua dimensione nel formato migliore
per chi lo apre; la console offre lo stesso nella finestra di condivisione di un'immagine.

Le varianti possono essere servite anche **all'URL dell'immagine stessa** da una distribuzione
(`https://cdn.example.com/photos/spiaggia.jpg` in AVIF, WebP o JPEG secondo ciò che accetta il browser, `?w=640`
per una larghezza) o da `GetObject` con `x-nexo-image-format`/`x-nexo-image-width`: vedi
[Immagini in ogni formato sullo stesso URL](distributions/README.it.md#immagini-in-ogni-formato-sullo-stesso-url).
La rotta dell'Admin API risponde anche a `HEAD` (gli header della variante senza il corpo).

### Limiti e memoria

La codifica AVIF è il passo lento e affamato di memoria (circa 6 s e 1,2 GB per una foto da 12 megapixel alla
sua dimensione, vedi [i benchmark](https://github.com/R3n3r0/Nexo/blob/main/docs/bench/media.md)): un job di 28
varianti di una foto così richiede circa 11 secondi su 4 core, 3 secondi senza AVIF. Il worker codifica le
varianti in parallelo entro un budget di memoria, e non produce mai una variante che da sola non ci starebbe
(un AVIF di un'immagine molto grande: la servono gli altri formati), quindi un'immagine grande non può fargli
esaurire la memoria.

| Flag | Variabile d'ambiente | Predefinito | |
| --- | --- | --- | --- |
| `--image-max-bytes` | `NEXO_WORKER_IMAGE_MAX_BYTES` | 256 MiB | file immagine più grande elaborato |
| `--image-max-pixels` | `NEXO_WORKER_IMAGE_MAX_PIXELS` | 100 000 000 | immagine più grande, letta dall'header prima di decodificarla (protegge dalle decompression bomb) |
| `--image-memory` | `NEXO_WORKER_IMAGE_MEMORY` | 2 GiB | memoria delle varianti codificate insieme |

Un'immagine più grande o non decodificabile fallisce in modo definitivo (`nexoctl media status` dice il
motivo); la sorgente non viene mai modificata.

## Sottotitoli

Una regola video con `subtitles` fa trascrivere al worker il parlato dei video che seleziona e tradurre la
trascrizione. Le tracce compaiono come didascalie nella console, nella [pagina del player](video-streaming.it.md)
e in ogni player HLS delle [distribuzioni](distributions/README.it.md): la playlist master le elenca come gruppo
di sottotitoli `EXT-X-MEDIA`, un WebVTT per lingua.

```json
{"rules": [{"id": "talks", "prefix": "talks/", "renditions": ["720p"],
            "subtitles": {"source": "it", "languages": ["en", "de"]}}]}
```

`source` è la lingua parlata (BCP 47: `it`, `en`, `pt-BR`...); senza, la rileva il servizio di trascrizione.
`languages` sono le traduzioni (al massimo 20). Con la CLI: `nexoctl media rule add local/videos --id talks
--prefix talks/ --subtitles=it --translate en,de` (`--subtitles` da solo rileva la lingua).

Nel worker non gira nessun modello: invia l'audio a servizi indicati per URL.

- **Trascrizione** (`--transcribe-url`): qualsiasi endpoint compatibile OpenAI che risponda `verbose_json` con
  segmenti temporizzati: l'API OpenAI (`https://api.openai.com/v1/audio/transcriptions`), un server
  [whisper.cpp](https://github.com/ggml-org/whisper.cpp) (`http://whisper:8080/inference`),
  faster-whisper-server o LocalAI. Il worker copia la traccia audio del video in parti M4A di al massimo 10
  minuti e 24 MB (senza decodifica, senza ffmpeg) e le invia una per volta.
- **Traduzione** (`--translate-url`): un server [LibreTranslate](https://libretranslate.com)
  (`http://libretranslate:5000/translate`, il motore predefinito) oppure, con `--translate-engine chat` e
  `--translate-model`, un endpoint di chat completion compatibile OpenAI (OpenAI, Ollama, vLLM...): le righe
  partono a gruppi di 40 e devono tornare in egual numero.

Una configurazione interamente self-hosted, con Docker:

```console
$ docker run -d --name whisper -p 8080:8080 ghcr.io/ggml-org/whisper.cpp:main-server \
    --model /models/ggml-small.bin --host 0.0.0.0
$ docker run -d --name libretranslate -p 5000:5000 libretranslate/libretranslate
$ export NEXO_WORKER_TRANSCRIBE_URL=http://whisper:8080/inference
$ export NEXO_WORKER_TRANSLATE_URL=http://libretranslate:5000/translate
```

Le tracce stanno nel sidecar della versione, descritte da `subtitles/manifest.json`:

- Una traccia **automatica** (`subtitles/auto/<lingua>.vtt`) vale per il contenuto da cui è stata creata: se il
  video viene sovrascritto non è più servita, finché il worker non lo rielabora. Un'elaborazione successiva
  riusa la trascrizione dello stesso contenuto invece di pagarla di nuovo, e rimuove le tracce delle lingue che
  la regola non chiede più.
- Una traccia **manuale** (`subtitles/<lingua>.vtt`), caricata o modificata nella console (scheda *Media*
  dell'oggetto, *Sottotitoli*), con `nexoctl media subtitles put` o con l'[Admin API](reference/admin-api.md),
  vale sempre. Il worker non la scrive mai, nemmeno se un utente la carica mentre una trascrizione è in corso
  (i file automatici stanno a parte e il manifest è aggiornato in modo condizionale): una traccia manuale della
  lingua parlata diventa la sorgente delle traduzioni al posto della trascrizione.

Una trascrizione o traduzione fallita non fa fallire il job: rendition, poster e miniature sono serviti, e
`nexoctl media status` (o la console) mostra perché i sottotitoli non sono riusciti; `nexoctl media reprocess`
riprova. Il worker invia a questi servizi l'audio e il testo dei video: per contenuti privati sceglili
self-hosted.

```console
$ nexoctl media subtitles ls local/videos/talks/intro.mp4
de       Deutsch            412 cues      38.1KiB  translated from it         2026-10-21 18:02:11 UTC
it       Italiano           412 cues      36.4KiB  manual (alice)             2026-10-21 18:10:45 UTC
$ nexoctl media subtitles get --format srt local/videos/talks/intro.mp4 de > intro.de.srt
$ nexoctl media subtitles put local/videos/talks/intro.mp4 en intro.en.vtt
```

## Configurazione

I quattro passi seguenti configurano un worker a mano, ovunque. Con Docker Compose, Docker Swarm e Helm i file
di deployment del repository svolgono da soli i passi 1 e 3 (un servizio o un Job di setup crea la policy,
l'utente e il target): vedi le [ricette di deployment](#ricette-di-deployment). Con qualunque metodo il worker
ha bisogno di:

- l'**API S3 di Nexo** (un nodo, o il load balancer di un cluster) e un **utente dai privilegi minimi** (passo 1);
- un **token** condiviso con il target `notify_media_worker` di Nexo (passi 2 e 3);
- una **directory dati persistente**: un job confermato a Nexo esiste solo nella coda che sta lì;
- **OpenH264**: scaricato da Cisco al primo avvio, oppure una libreria fornita da te;
- **memoria**: circa 1 GiB per job video e 2 GiB per job immagine (AVIF), moltiplicati per `--concurrency`.

### 1. Credenziali del worker

Crea un utente per il worker con questa policy (sostituisci `*` nelle risorse con i bucket delle tue regole
per restringerla ancora): legge i video e scrive solo sotto `.nexo-media/`.

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {"Effect": "Allow", "Action": ["s3:GetObject", "s3:GetObjectVersion", "nexo:DownloadProtectedMedia"], "Resource": ["arn:aws:s3:::*/*"]},
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

Gli archivi della release (`nexo-media-worker_<versione>_<os>_<arch>.tar.gz`) e i pacchetti deb/rpm
`nexo-media-worker` contengono il binario e una unit systemd irrobustita ([sotto](#systemd)).

Con Docker (l'immagine multi-arch `docker.io/paolor/nexo-media-worker`, oppure `make docker-media-worker` dai
sorgenti; scarica OpenH264 nel suo volume `/data` al primo avvio; pubblica la porta 9600 solo dove Nexo la
raggiunge):

```console
$ docker run -d --name nexo-media-worker -p 9600:9600 -v media-worker:/data \
    -e NEXO_WORKER_S3_ENDPOINT=http://nexo:9000 \
    -e NEXO_WORKER_ACCESS_KEY=media-worker -e NEXO_WORKER_SECRET_KEY='un-segreto-lungo-e-casuale' \
    -e NEXO_WORKER_AUTH_TOKEN=... docker.io/paolor/nexo-media-worker
```

| Flag | Ambiente | Default | |
| --- | --- | --- | --- |
| `--s3-endpoint` | `NEXO_WORKER_S3_ENDPOINT` | | URL dell'API S3 di Nexo (obbligatorio) |
| `--access-key`, `--secret-key` | `NEXO_WORKER_ACCESS_KEY`, `NEXO_WORKER_SECRET_KEY` | | credenziali del worker (obbligatorie; anche da file: `NEXO_WORKER_ACCESS_KEY_FILE`, `NEXO_WORKER_SECRET_KEY_FILE`) |
| `--auth-token` | `NEXO_WORKER_AUTH_TOKEN` | | bearer token inviato da Nexo (obbligatorio; anche da file: `NEXO_WORKER_AUTH_TOKEN_FILE`) |
| `--address` | `NEXO_WORKER_ADDRESS` | `:9600` | indirizzo dell'endpoint dei job (`POST /jobs`, `GET /healthz`) |
| `--data-dir` | `NEXO_WORKER_DATA_DIR` | `nexo-media-worker` | coda dei job, file di lavoro, libreria scaricata |
| `--concurrency` | `NEXO_WORKER_CONCURRENCY` | `1` | job elaborati insieme (ognuno usa già tutti i core) |
| `--max-attempts` | `NEXO_WORKER_MAX_ATTEMPTS` | `5` | tentativi di un job che fallisce per un motivo temporaneo |
| `--openh264` | `NEXO_WORKER_OPENH264` | | percorso della libreria (default: scaricata da Cisco) |
| `--s3-region` | `NEXO_WORKER_S3_REGION` | `us-east-1` | regione delle firme |
| `--name` | `NEXO_WORKER_NAME` | nome host | nome del worker in `status.json` |
| `--log-level`, `--log-format` | `NEXO_WORKER_LOG_LEVEL`, `NEXO_WORKER_LOG_FORMAT` | `info`, `text` | log (`json` per i collettori) |
| `--transcribe-url` | `NEXO_WORKER_TRANSCRIBE_URL` | | endpoint di trascrizione compatibile OpenAI dei [sottotitoli automatici](#sottotitoli) |
| `--transcribe-key`, `--transcribe-model` | `NEXO_WORKER_TRANSCRIBE_KEY`, `NEXO_WORKER_TRANSCRIBE_MODEL` | `whisper-1` | la sua chiave API (anche da `NEXO_WORKER_TRANSCRIBE_KEY_FILE`) e il modello |
| `--translate-url`, `--translate-engine` | `NEXO_WORKER_TRANSLATE_URL`, `NEXO_WORKER_TRANSLATE_ENGINE` | `libretranslate` | endpoint di traduzione: LibreTranslate oppure `chat` (chat completion compatibili OpenAI) |
| `--translate-key`, `--translate-model` | `NEXO_WORKER_TRANSLATE_KEY`, `NEXO_WORKER_TRANSLATE_MODEL` | | la sua chiave API (anche da `NEXO_WORKER_TRANSLATE_KEY_FILE`) e il modello chat |

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

Nella console: bucket → **Media**. Le regole richiedono `admin:SetBucketMedia` per essere modificate e
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

## Ricette di deployment

### Docker Compose

`deploy/docker/compose.media-worker.yml` è un overlay per i file Compose di Nexo. Aggiunge il worker (volume
`media-worker-data` per la sua coda, file system radice in sola lettura) e un setup che si esegue una volta: attende
Nexo, crea la policy `media-worker` e l'utente del worker e imposta il target `notify_media_worker` con il token,
quindi i passi 1 e 3 sono già fatti:

```console
$ cd deploy/docker
$ cp nexo.env.example nexo.env                  # credenziali root di Nexo
$ cp media-worker.env.example media-worker.env  # NEXO_WORKER_SECRET_KEY, NEXO_WORKER_AUTH_TOKEN
$ docker compose -f compose.single.yml -f compose.media-worker.yml up -d
$ docker compose -f compose.single.yml -f compose.media-worker.yml logs media-worker-setup
media-worker-setup: done: notify_media_worker -> http://media-worker:9600/jobs
```

Il setup è idempotente: dopo aver cambiato `media-worker.env` riesegui `up -d` (una nuova secret key ricrea
l'utente, un nuovo token aggiorna il target). La policy è `media-worker-policy.json` (tutti i bucket: elenca i
tuoi per restringerla). Con un file di cluster punta worker e setup al load balancer:
`NEXO_MEDIA_S3_URL=http://lb:9000 NEXO_MEDIA_ADMIN_URL=http://lb:9001`.

### Docker Swarm

`deploy/swarm/media-worker.yml` aggiunge gli stessi servizi allo stack di `deploy/swarm`, sul nodo Swarm con
l'etichetta `nexo.media=1` (la sua coda è un volume locale), con le credenziali root dai Docker secret dello
stack:

```console
$ docker node update --label-add nexo.media=1 swarm-e
$ cp ../docker/media-worker.env.example media-worker.env   # secret key e token
$ docker stack deploy -c stack.yml -c media-worker.yml nexo
```

### Kubernetes (Helm)

Il chart di Nexo installa il worker con `mediaWorker.enabled=true`: un Deployment di un pod (non root, file
system radice in sola lettura, probe su `/healthz`), un PersistentVolumeClaim per `/data`, un Service sulla porta
9600, un Secret con access key, secret key e token (generati e conservati negli upgrade, oppure
`mediaWorker.auth.existingSecret`) e un Job eseguito dopo ogni install e upgrade (hook di Helm) che crea la
policy (ristretta a `mediaWorker.setup.buckets`), l'utente e il target che punta al Service:

```console
$ helm upgrade --install nexo "$CHART" -n storage --set mediaWorker.enabled=true \
    --set 'mediaWorker.setup.buckets={videos,photos}'
$ kubectl -n storage logs job/nexo-media-worker-setup
$ helm test nexo -n storage                     # verifica anche l'endpoint dei job
```

Il limite di memoria predefinito è 3 GiB; alza `mediaWorker.resources` insieme a `mediaWorker.concurrency`. Senza
accesso a `ciscobinary.openh264.org`, monta una libreria e imposta `mediaWorker.openh264.path`. Gli altri valori
sono nel [README del chart](https://github.com/R3n3r0/Nexo/tree/main/deploy/helm/nexo#media-worker).

### systemd

Il pacchetto `nexo-media-worker` installa `/usr/bin/nexo-media-worker`, la unit `nexo-media-worker.service` e
`/etc/default/nexo-media-worker` (root, modo 0600); gli archivi contengono gli stessi file in `systemd/`. La unit
esegue il worker come utente dinamico nella sandbox di `nexo.service` (file system in sola lettura tranne la sua
directory di stato `/var/lib/nexo-media-worker`, nessuna capability, filtro delle system call), con
`OOMScoreAdjust=500` e pesi di CPU e I/O più bassi, così che su una macchina condivisa Nexo abbia la precedenza:

```console
$ sudo apt install ./nexo-media-worker_1.0.0_amd64.deb   # oppure dnf install ./nexo-media-worker-1.0.0-1.x86_64.rpm
$ sudoedit /etc/default/nexo-media-worker                # endpoint, access key, secret key, token
$ sudo systemctl enable --now nexo-media-worker
$ journalctl -u nexo-media-worker -f
```

Da un archivio: `sudo install -m 0755 nexo-media-worker /usr/local/bin/`, `sudo install -m 0600
systemd/nexo-media-worker.env.example /etc/default/nexo-media-worker`, `sudo install -m 0644
systemd/nexo-media-worker.service /etc/systemd/system/`, poi `sudo systemctl daemon-reload`. I passi 1 e 3 si
fanno a mano; l'endpoint del target è `http://<host del worker>:9600/jobs`.

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
