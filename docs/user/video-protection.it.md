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

Qualsiasi player HLS riproduce un video protetto (Safari/iOS nativamente, hls.js, Video.js, ExoPlayer, VLC): il
token viaggia negli URL della playlist.

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

## Cosa non fa

Non è un DRM commerciale (Widevine, FairPlay, PlayReady): chi è determinato può registrare lo schermo o estrarre le
chiavi del flusso che sta guardando, alla velocità della riproduzione. La protezione rende inutile lo scaricamento
massivo, limita la condivisione, e la filigrana mobile riconduce una fuga all'account.
