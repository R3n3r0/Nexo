# Protezione dei video

Proteggi i video di un bucket dal download (per una piattaforma di corsi/LMS, un'area riservata, una intranet)
senza fornitori di DRM né license server: tutto gira in Nexo. Un video protetto:

- **va in streaming cifrato**: ogni segmento HLS è cifrato AES-128 (HLS `METHOD=AES-128`), una chiave nuova ogni 10
  segmenti (circa un minuto). Un downloader (yt-dlp, ffmpeg, estensioni del browser) senza chiavi salva rumore;
- **riceve le chiavi solo con un token di riproduzione** di quel video, emesso dalla tua applicazione per uno
  spettatore, al massimo *pace* volte più veloce della riproduzione e a non più di *max sessioni* flussi simultanei
  dello spettatore (un flusso più recente prende il posto del più vecchio: l'account prestato a un amico continua a
  buttare fuori uno dei due);
- **non si scarica come file**: `GetObject`, le copie, `SelectObjectContent`, download, anteprima e ZIP della
  console e le distribuzioni rifiutano l'originale a chi non ha `nexo:DownloadProtectedMedia`;
- **mostra la filigrana dello spettatore** (un nome, un'e-mail) in movimento sopra l'immagine nel player di Nexo e
  nell'[SDK JavaScript](https://github.com/R3n3r0/Nexo/tree/main/sdk/js), anche a schermo intero.

Due livelli:

- **standard** (predefinito): qualsiasi player HLS riproduce un video protetto (Safari/iOS nativamente, hls.js,
  Video.js, ExoPlayer, VLC); il token viaggia negli URL della playlist;
- **avanzato**: lo riproduce solo il player di Nexo (la sua pagina, `<nexo-player>` e `createPlayer()` dell'SDK). Le
  chiavi viaggiano su un canale di sessione (WebSocket) aperto con uno scambio di chiavi, ogni spettatore riceve
  segmenti cifrati per lui e le chiavi non compaiono mai nella scheda di rete
  ([§5](#5-protezione-avanzata-solo-il-player-di-nexo)).

Su entrambi si può attivare la **filigrana forense**: nasconde nell'immagine un codice invisibile del token di
riproduzione, così una copia diffusa, anche ricodificata, indica lo spettatore a cui era stata trasmessa
([§6](#6-filigrana-forense)).

## 1. Attivala

Console: bucket → **Media** → *Protezione video*. Oppure:

```sh
nexoctl media protection set local/lms --enable --prefix corsi/
nexoctl media protection get local/lms
```

| Impostazione | Flag | Predefinito | Significato |
| --- | --- | --- | --- |
| Attiva | `--enable` / `--disable` | no | |
| Prefisso | `--prefix` | tutto il bucket | solo le chiavi sotto il prefisso |
| Suffissi | `--suffix` | `.mp4`, `.mov`, `.m4v` | i video protetti (anche le loro rendition) |
| Durata token | `--token-ttl` | 2h (max 24h) | validità massima di un token |
| Sessioni | `--max-sessions` | 2 (`unlimited`) | flussi simultanei per spettatore |
| Ritmo | `--pace` | 2 | chiavi al massimo tante volte più veloci della riproduzione (dopo 3 periodi iniziali) |
| Filigrana | `--watermark` | sì | il player di Nexo disegna lo spettatore |
| Origini ammesse | `--origin` | tutte | origini web ammesse a chiedere le chiavi (`https://learn.example.com`) |
| Link di condivisione | `--share-links` | no | i link di streaming (presigned) riproducono anche i video protetti |
| Riproduzione | `--mode` | `standard` | `standard`: qualsiasi player HLS; `advanced`: solo il player di Nexo |
| Dispositivi datati | `--legacy-devices` | `refuse` | modalità avanzata: `standard` fa riprodurre in modo standard i browser senza Media Source Extensions (iPhone datati) |
| Filigrana forense | `--forensic` | no | filigrana invisibile che indica il token di una copia diffusa (richiede il media worker) |

Il media worker legge gli originali per derivare le rendition: la sua policy richiede `nexo:DownloadProtectedMedia`
(la policy della [guida del media worker](media-worker.it.md) lo include).

## 2. Emetti i token dalla tua applicazione

Il backend (mai il browser) chiama l'Admin API con una access key che ha `nexo:IssuePlaybackToken` sui video (e
`admin:RevokePlaybackTokens` sul bucket per revocarli):

```sh
nexoctl media token local/lms/corsi/intro.mp4 --subject studente-1842 --watermark "mario.rossi@example.com"
```

```http
POST /api/v1/buckets/lms/objects/playback-tokens?key=corsi/intro.mp4      (SigV4, porta dell'Admin API)
{"subject": "studente-1842", "watermark": "mario.rossi@example.com", "ttlSeconds": 7200}
```

La risposta contiene `playlist` (per qualsiasi player HLS), `player` (la pagina del player di Nexo), `sessionId` e
`expiresAt`. L'[SDK JavaScript](https://github.com/R3n3r0/Nexo/tree/main/sdk/js) la incapsula per Node.js
(`createNexoClient().issuePlaybackToken()`); funziona qualsiasi linguaggio con un firmatario SigV4, e il pacchetto
Go `nexoadmin` (`IssuePlaybackToken`).

## 3. Riproduci

- **iframe**: `<iframe src="{player}" allow="fullscreen">`;
- **la tua pagina**: `<nexo-player src="{playlist}">` dell'SDK (servito da Nexo in `/nexo/media/sdk/index.js`),
  oppure `createPlayer()` in React, Angular, Vue;
- **qualsiasi player HLS** con `{playlist}` (app: AVPlayer, ExoPlayer).

## 4. Revoca

```sh
nexoctl media revoke local/lms --session 9f2c...         # un token
nexoctl media revoke local/lms --subject studente-1842   # tutti i token emessi finora a uno spettatore
```

Un token revocato si ferma su tutti i nodi entro una decina di secondi.

## 5. Protezione avanzata: solo il player di Nexo

```sh
nexoctl media protection set local/lms --mode advanced
```

Console: *Protezione video* → *Riproduzione* → **Solo player Nexo**. Nella tua applicazione non cambia nulla:
emette gli stessi token e incorpora il player di Nexo (iframe, `<nexo-player>`, `createPlayer()`) con lo stesso URL
`playlist`. Cosa cambia sulla rete:

1. Il token da solo non apre nulla: le playlist rispondono 403 (`X-Nexo-Playback: session`) a ogni altro player.
2. Il player di Nexo apre una **sessione** su `wss://…/nexo/media/v1/{bucket}/session` ed esegue uno scambio di
   chiavi (ECDH P-256, HKDF-SHA256) la cui chiave privata WebCrypto non lascia mai leggere alla pagina. Il server
   risponde con un ticket valido solo per quella sessione.
3. Ogni segmento è cifrato **per quella sessione** (AES-128-GCM): un segmento catturato da uno spettatore è rumore
   per chiunque altro, e le playlist non indicano alcuna chiave che un downloader possa scaricare.
4. Il player chiede le chiavi sul canale; arrivano avvolte con la chiave di sessione e vengono importate in WebCrypto
   come non estraibili: non compaiono mai in chiaro nella scheda di rete né in una variabile JavaScript.
5. La revoca, un flusso più recente dello spettatore (max sessioni) e la scadenza del token chiudono subito il
   canale; lo stesso fa l'arresto di un nodo (il player si ricollega a un altro).

**Dispositivi datati.** Il player di Nexo decifra in un loader Media Source Extensions: Chrome, Edge, Firefox,
Safari su macOS e iPadOS, Safari su iPhone da iOS 17.1. Con `--legacy-devices standard` gli altri browser
riproducono in modo standard; con `refuse` (predefinito) mostrano un errore.

**Proxy.** La sessione preferisce un WebSocket. nginx richiede gli header `Upgrade` su `/nexo/media/v1/*/session`
(la [configurazione in `deploy/proxy`](https://github.com/R3n3r0/Nexo/tree/main/deploy/proxy/nginx.conf) li ha);
Caddy, Traefik, HAProxy e Cloudflare trasportano i WebSocket così come sono. Se un proxy lo blocca, il player
ripiega su semplici richieste `POST` sullo stesso percorso: la riproduzione funziona comunque.

## 6. Filigrana forense

La filigrana in movimento si può ritagliare o sfocare. La filigrana forense non si vede e sopravvive a
ricodifica, ridimensionamento e registrazione dello schermo di qualità accettabile:

```sh
nexoctl media protection set local/lms --forensic
nexoctl media reprocess local/lms/corsi/intro.mp4    # ogni video caricato prima
```

1. Il [media worker](media-worker.it.md) codifica **due volte** ogni rendition di un video marcato (varianti A e B,
   che differiscono per un motivo invisibile di ±2 livelli di luminanza): il doppio del tempo di codifica e dello
   spazio delle rendition.
2. Ogni token di riproduzione ha un codice segreto di 64 bit; il segmento *n* dei suoi flussi viene dalla variante A
   o B secondo il bit *n mod 64*. Una copia di pochi minuti porta l'intero codice.
3. All'emissione, Nexo registra il token (soggetto, filigrana, sessione, orari) nel bucket, cifrato, sotto
   `.nexo-media/.forensic/`. Un video marcato non va mai in streaming prima che esistano le sue rendition marcate
   (503 nel frattempo), e mai dall'originale.

**Risalire a una fuga.** Scarica la copia (convertila in MP4 con video H.264 se serve), poi:

```sh
nexo-media-worker trace --leak copia.mp4 --bucket lms --key corsi/intro.mp4 --out voti.json
nexoctl media trace local/lms/corsi/intro.mp4 --votes voti.json
```

```text
Code read from the copy of `local/lms/corsi/intro.mp4`: 0110…1 (64 bits known)
#    SUBJECT                  SESSION                          AGREE     ISSUED
1    studente-1842            9f2c…                            63/64     2026-03-02T18:04:11Z  (mario.rossi@example.com)
```

Il worker confronta alcune immagini al secondo della copia con le due varianti e scrive dei voti; Nexo legge il
codice ed elenca i token che concordano con almeno 7/8 dei suoi bit (`admin:TracePlaybackLeaks` sul bucket, Admin
API `POST /api/v1/buckets/{bucket}/objects/playback-trace`). Una clip che inizia più avanti nel video richiede
`--offset`. Una clip breve (pochi segmenti) concorda con più token: più è lunga la copia, meno sono i candidati.

## Come funziona

- Le chiavi derivano da una chiave del deployment (derivata a sua volta dalle credenziali root e dall'ID del
  deployment), dal bucket, dalla chiave, dalla versione e dall'ETag: nulla viene salvato per video, sovrascrivere un
  video ne cambia le chiavi, tutti i nodi calcolano le stesse chiavi. **Cambiare le credenziali root invalida tutti i
  token e tutte le chiavi.**
- Revoche e flussi degli spettatori stanno nei documenti di sistema condivisi dai nodi; il ritmo è misurato su
  ciascun nodo.
- I proprietari continuano a vedere i loro video protetti nella console.
- Le distribuzioni rifiutano l'originale e le playlist dei video protetti: i video protetti si riproducono sulla
  porta S3, con un token.
- `nexo_media_keys_total{result="ok|sessions|pace"}` conta le chiavi servite e rifiutate.
- Il progetto, il protocollo delle sessioni e la filigrana forense sono descritti nella
  [proposta](https://github.com/R3n3r0/Nexo/blob/main/docs/proposals/video-protection.md) (§10, §11).

## Cosa non fa

Non è un DRM commerciale (Widevine, FairPlay, PlayReady): un browser non può tenere un segreto al suo utente. Chi è
determinato può registrare lo schermo o, in modalità avanzata, intercettare il decoder della pagina, alla velocità
della riproduzione, per il video e la sessione che sta guardando. La protezione rende inutile lo scaricamento
massivo, limita la condivisione, e le filigrane riconducono una fuga all'account. La filigrana forense è indebolita
da spettatori che colludono (mediando copie di più account), da ritagli pesanti o trasformazioni geometriche e da
clip molto brevi.
