# SDK del player di Nexo (JavaScript)

Riproduci i video di [Nexo](https://github.com/R3n3r0/Nexo), protetti o no, in qualsiasi pagina web, ed emetti i
loro token di riproduzione dal tuo backend. Nessun fornitore di DRM, nessun license server: tutto gira dentro Nexo.

*[Read in English](README.md)*

## Serve un player particolare?

No. Un video protetto è HLS standard con cifratura `METHOD=AES-128`: **qualsiasi player HLS lo riproduce** (Safari e
iOS nativamente, hls.js, Video.js, Shaka, ExoPlayer, AVPlayer, VLC), purché riceva l'URL della playlist con il suo
token di riproduzione. Il token viaggia in ogni URL della playlist: il player non ha bisogno di credenziali né di
codice particolare.

Questa è la modalità **standard**. Nella modalità **avanzata** (`--mode advanced`,
[sotto](#protezione-avanzata-solo-il-player-di-nexo)) solo questo SDK, e la pagina del player di Nexo, riproduce i
video: le chiavi si scambiano su un canale di sessione e non compaiono mai nella scheda di rete.

Questo SDK rende solo breve il caso comune:

| Parte | Cosa fa | Dove gira |
| --- | --- | --- |
| `<nexo-player>` / `createPlayer()` | Riproduce la playlist (HLS nativo o hls.js), disegna la **filigrana mobile** dello spettatore sopra l'immagine (anche a schermo intero), segnala token scaduti o revocati | Browser: HTML semplice, React, Angular, Vue, Svelte... |
| `createNexoClient()` (`@nexo-storage/player/server`) | Emette e revoca i token di riproduzione con l'Admin API (firma SigV4, nessuna dipendenza) | Il tuo backend: Node.js 18+, Deno, Bun |

La access key **non** arriva mai al browser: il backend emette un token per l'utente autenticato e passa alla pagina
solo l'URL della playlist.

```
 Browser (pagina dell'LMS)        Il tuo backend                   Nexo
 ─────────────────────────        ──────────────                   ────
 GET /lezione/42 ───────────────► verifica che l'utente possa vedere
                                  issuePlaybackToken() ──────────► POST /api/v1/buckets/lms/objects/playback-tokens
                 ◄─────────────── pagina con l'URL della playlist ◄ { playlist, player, sessionId, expiresAt }
 <nexo-player src=playlist> ─────────────────────────────────────► playlist, segmenti cifrati, chiavi (?pt=token)
```

## 1. Proteggi i video (una volta)

Console → bucket → **Media** → *Protezione video*, oppure:

```sh
nexoctl media protection set local/lms --enable --prefix corsi/ --max-sessions 2 --origin https://learn.example.com
```

Crea la access key del backend con una policy che permette solo i token:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {"Effect": "Allow", "Action": ["nexo:IssuePlaybackToken"], "Resource": ["arn:aws:s3:::lms/corsi/*"]},
    {"Effect": "Allow", "Action": ["admin:RevokePlaybackTokens"], "Resource": ["arn:aws:s3:::lms"]}
  ]
}
```

## 2. Emetti un token nel backend

```js
import { createNexoClient } from '@nexo-storage/player/server';

const nexo = createNexoClient({
  endpoint: 'https://nexo.example.com:9001', // l'Admin API (indirizzo della console)
  accessKey: process.env.NEXO_ACCESS_KEY,
  secretKey: process.env.NEXO_SECRET_KEY,
});

app.get('/lezione/:id', requireLogin, async (req, res) => {
  const lezione = await db.lezioni.find(req.params.id);
  if (!(await puoVedere(req.user, lezione))) return res.sendStatus(403);
  const { playlist } = await nexo.issuePlaybackToken({
    bucket: 'lms',
    key: lezione.video,                 // corsi/intro.mp4
    subject: String(req.user.id),       // conta i flussi simultanei di questo utente
    watermark: req.user.email,          // disegnata sopra il video
  });
  res.render('lezione', { playlist });
});

// Abbonamento scaduto, o account condiviso:
await nexo.revokePlaybackSubject('lms', String(user.id));
```

Altri linguaggi (PHP, Python, Java, Go...): è una sola chiamata HTTP,
`POST /api/v1/buckets/{bucket}/objects/playback-tokens?key=...` con `{"subject": "...", "watermark": "...",
"ttlSeconds": 7200}`, firmata AWS Signature V4 (servizio `s3`) come una richiesta S3: usa il firmatario SigV4
dell'SDK AWS del tuo linguaggio, il pacchetto Go `nexoadmin`, oppure `nexoctl media token` in uno script.

## 3. Riproducilo nella pagina

### HTML semplice (senza build)

L'SDK lo serve Nexo stesso:

```html
<script src="https://cdn.jsdelivr.net/npm/hls.js@1"></script>
<script type="module">
  import { defineNexoPlayer } from 'https://nexo.example.com:9000/nexo/media/sdk/index.js';
  defineNexoPlayer();
</script>

<nexo-player src="{{ playlist }}" style="max-width: 960px"></nexo-player>
```

Attributi: `src`, `watermark` (testo, oppure `off`), `autoplay`, `muted`, `poster`. L'elemento emette `nexo-error`
con `{code, status, message}`.

Oppure, senza script, un iframe con la pagina del player di Nexo (`player` nella risposta del token):

```html
<iframe src="{{ player }}" allow="fullscreen; autoplay" style="width: 100%; aspect-ratio: 16/9; border: 0"></iframe>
```

### React, Angular, Vue

`createPlayer(contenitore, { src: playlist, hls: Hls })` in un effetto / `ngOnChanges` / `onMounted`, e
`player.destroy()` allo smontaggio: gli esempi completi sono nel [README inglese](README.md#react).

## Protezione avanzata: solo il player di Nexo

```sh
nexoctl media protection set local/lms --mode advanced
```

Il tuo codice non cambia: `<nexo-player>` e `createPlayer()` (con hls.js) capiscono dal token che il video si
riproduce in una sessione e la aprono da soli. In pratica:

1. aprono una WebSocket su `…/nexo/media/v1/{bucket}/session` (semplici richieste `POST` se un proxy blocca le
   WebSocket) ed eseguono uno scambio di chiavi ECDH P-256 la cui chiave privata WebCrypto tiene non estraibile;
2. riproducono la presentazione della sessione, i cui segmenti sono cifrati solo per lei (AES-128-GCM);
3. chiedono le chiavi sul canale, le ricevono avvolte con la chiave di sessione e le importano in WebCrypto come non
   estraibili: non compaiono mai in chiaro nella scheda di rete né in una variabile JavaScript;
4. si fermano con `denied` quando il token viene revocato, scade o è sostituito da un flusso più recente dello
   spettatore, e si ricollegano quando un nodo si ferma.

Ogni altro player HLS riceve 403. I pezzi sono esportati per un'integrazione su misura con hls.js
(`openSession`, `sessionLoader`): l'esempio è nel [README inglese](README.md#advanced-protection-the-nexo-player-only).

I browser senza Media Source Extensions (iPhone prima di iOS 17.1) non possono eseguirlo: ricevono l'errore
`unsupported`, oppure riproducono in modo standard se il bucket lo consente (`--legacy-devices standard`).

## Errori

| `code` | HTTP | Significato |
| --- | --- | --- |
| `denied` | 403 | Token scaduto o revocato, oppure un flusso più recente dello stesso utente ha preso il posto di questo (`maxSessions`), oppure l'origine della pagina non è ammessa |
| `too-fast` | 429 | Chiavi chieste più velocemente della riproduzione (un downloader) |
| `not-found` | 404 | Il video non esiste |
| `unsupported` | 403 / 0 | Il video si riproduce solo nel player di Nexo e questo browser non può eseguirlo, oppure il browser non riproduce HLS |

Per rinnovare il token senza ricaricare la pagina: `player.load(nuovaPlaylist)`.

## Cosa protegge e cosa no

I segmenti sono cifrati (AES-128, una chiave ogni 10 segmenti); le chiavi vanno solo a un token valido, al ritmo
della riproduzione, a non più di `maxSessions` flussi simultanei per utente; l'originale non si scarica senza
`nexo:DownloadProtectedMedia`. Un downloader salva rumore; un link condiviso scade, si ferma alla revoca e mostra il
nome di chi l'ha condiviso. Nella modalità avanzata le chiavi viaggiano solo sul canale di sessione, avvolte, e ogni
spettatore riceve segmenti cifrati per lui. Con la filigrana forense, una copia diffusa indica il token con cui è
stata trasmessa ([guida](https://github.com/R3n3r0/Nexo/blob/main/docs/user/video-protection.it.md#6-filigrana-forense)).
Come per ogni schema senza DRM commerciale, chi è determinato può registrare lo schermo o intercettare il decoder
della pagina: le filigrane identificano l'account da cui proviene.

Licenza: AGPL-3.0-only, come Nexo. Contatti: <paolorusso1991@gmail.com>.
