<p align="center">
  <img src="docs/images/logo.svg" alt="Nexo" width="88" height="88">
</p>

<h1 align="center">Nexo</h1>

<p align="center">
  <b>Object storage self-hosted compatibile S3, scritto in Go, con lo streaming video incluso.</b><br>
  Un solo binario: server S3, Admin API e console web. Da un disco a un cluster multi-nodo.
</p>

<p align="center">
  <a href="https://github.com/R3n3r0/Nexo/actions/workflows/ci.yml"><img src="https://github.com/R3n3r0/Nexo/actions/workflows/ci.yml/badge.svg?branch=main" alt="CI"></a>
  <a href="https://r3n3r0.github.io/Nexo/it/"><img src="https://img.shields.io/badge/docs-r3n3r0.github.io%2FNexo-4c6ef5" alt="Documentazione"></a>
  <a href="https://hub.docker.com/r/paolor/nexo"><img src="https://img.shields.io/docker/pulls/paolor/nexo?label=docker%20pulls&color=4c6ef5" alt="Docker Hub"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-AGPL--3.0-green" alt="Licenza: AGPL-3.0"></a>
  <img src="https://img.shields.io/badge/s3--tests-451%2F451-brightgreen" alt="Ceph s3-tests: 451 su 451">
  <img src="https://img.shields.io/badge/mint-638%2F638-brightgreen" alt="MinIO mint: 638 su 638">
</p>

<p align="center">
  <a href="#-avvio-rapido"><b>Avvio rapido</b></a> ·
  <a href="#-funzionalità"><b>Funzionalità</b></a> ·
  <a href="#-video-integrato"><b>Video</b></a> ·
  <a href="#-screenshot"><b>Screenshot</b></a> ·
  <a href="https://r3n3r0.github.io/Nexo/it/"><b>Documentazione</b></a> ·
  <a href="README.md"><b>English</b></a>
</p>

<p align="center">
  <img src="docs/images/demo.gif" alt="Demo della console di Nexo: accesso, creazione di un bucket con versioning, caricamento di foto, anteprima e link di condivisione" width="960">
</p>

<p align="center"><sub>Accesso, un bucket con versioning, un caricamento, un'anteprima e un link di condivisione nella console web (registrata con <code>make demo-gif</code>).</sub></p>

## ✨ Perché Nexo

- **Un solo binario, niente pezzi esterni.** `nexo` è il server S3, l'Admin API e la console web; non c'è un
  database di metadati esterno. Lo stesso binario gira su un disco, su più dischi con erasure coding o come
  cluster di nodi.
- **S3 davvero.** Supera tutti i 451 test applicabili di `s3-tests` di Ceph e i 638 delle suite SDK `mint` di
  MinIO, su un disco, più dischi e in cluster: SDK, `aws`, `rclone` e gli strumenti di backup funzionano e basta.
- **I tuoi video si vedono appena arrivano.** Un MP4 caricato va subito in streaming HLS, e un media worker può
  aggiungere ogni qualità da 240p a 4K, un poster e le miniature della barra di avanzamento, senza ffmpeg.
- **Pensato per chi lo gestisce.** Una console per tutto, metriche Prometheus con una dashboard Grafana pronta,
  trace e log in tempo reale, pacchetti per ogni piattaforma.

**Arrivi da MinIO?** Segui la [guida alla migrazione](https://r3n3r0.github.io/Nexo/it/migrate-from-minio/) (ogni
versione, policy e utenti, verificata con `nexoctl diff`). **Stai scegliendo uno storage?** Vedi il
[confronto](https://r3n3r0.github.io/Nexo/it/comparison/) con MinIO, SeaweedFS, Garage e Ceph RGW, compreso quando
è meglio un altro progetto.

## 🚀 Avvio rapido

<table>
<tr>
<td width="50%" valign="top">

**Docker**

```bash
docker run -d --name nexo -p 9000:9000 -p 9001:9001 \
  -e NEXO_ROOT_USER=admin \
  -e NEXO_ROOT_PASSWORD=supersegreta \
  -v nexo-data:/data paolor/nexo:latest server /data
```

</td>
<td width="50%" valign="top">

**Binario** ([release](https://github.com/R3n3r0/Nexo/releases) o `make build`)

```bash
export NEXO_ROOT_USER=admin NEXO_ROOT_PASSWORD=supersegreta
nexo server ~/nexo-data
# più dischi: nexo server /mnt/disk{1...4}
```

</td>
</tr>
</table>

Apri la console su <http://localhost:9001> (utente `admin`, password `supersegreta`) oppure usa la CLI:

```bash
nexoctl alias set local http://localhost:9000 admin supersegreta
nexoctl mb local/foto
nexoctl cp hello.txt local/foto/
nexoctl share download --expire 2h local/foto/hello.txt     # un link revocabile
```

Qualsiasi client S3 funziona con l'endpoint `http://localhost:9000` e le stesse credenziali, per esempio
`aws --endpoint-url http://localhost:9000 s3 ls`. Le immagini sono su Docker Hub:
[`paolor/nexo`](https://hub.docker.com/r/paolor/nexo) (server e CLI) e
[`paolor/nexoctl`](https://hub.docker.com/r/paolor/nexoctl) (solo CLI).

## 🧩 Funzionalità

<table>
<tr>
<td width="50%" valign="top">

### 🪣 API S3
SigV4, bucket, oggetti, multipart, versioning, Object Lock, lifecycle, notifiche, CORS, checksum, cifratura
SSE-S3 / SSE-KMS / SSE-C, URL presigned e POST policy.

</td>
<td width="50%" valign="top">

### 🛡️ Durabilità
Erasure coding Reed-Solomon con parità configurabile, protezione dal bitrot, healing automatico, sostituzione
dei dischi a caldo, espansione con nuovi pool.

</td>
</tr>
<tr>
<td width="50%" valign="top">

### 🔐 Sicurezza
Utenti, gruppi e policy IAM, access key con scadenza, STS, regole di accesso con glob e regex, link di
condivisione revocabili, TLS con ricarica a caldo, mTLS tra i nodi, KMS integrato o KES, audit log.

</td>
<td width="50%" valign="top">

### 🎬 Video
I video MP4 vanno in streaming HLS impacchettati al volo, nella console e tramite link di streaming revocabili
con una pagina player pubblica. Il media worker aggiunge rendition, poster e miniature ([sotto](#-video-integrato)).

</td>
</tr>
<tr>
<td width="50%" valign="top">

### 🌍 Distribuzioni
Un bucket servito sui tuoi domini, in stile CloudFront: certificati ACME, URL firmati, cache in memoria e su
disco con invalidazione automatica.

</td>
<td width="50%" valign="top">

### 📈 Operatività
Console web, metriche Prometheus con dashboard Grafana e alert, trace e log in tempo reale, OpenTelemetry,
deb/rpm, Homebrew, immagini Docker, Helm chart, unit systemd.

</td>
</tr>
</table>

## 🎬 Video integrato

Carica un MP4 e si vede subito, in qualsiasi browser e player HLS: Nexo impacchetta lo stream al volo dal file
che hai salvato, senza transcodifica e senza niente da preparare. Condividilo con un link di streaming
revocabile che apre una pagina player pronta.

Crea il bucket con l'opzione **Video HLS** (o aggiungi una **regola media** a uno esistente) e
`nexo-media-worker` ricava di più da ogni video che seleziona:
qualità ridotte per le reti lente (da 240p a 2160p, offerte dalla stessa playlist), un poster e le miniature della
barra di avanzamento. Il worker è un programma Go separato basato su OpenH264 di Cisco, **niente ffmpeg**, che
lavora tramite l'API S3 con una policy minima e non perde mai un job.

<p align="center">
  <img src="docs/images/video-pipeline.svg" alt="La pipeline video: un MP4 caricato su Nexo va subito in streaming HLS; una regola media manda un job a nexo-media-worker, che scrive rendition, poster e miniature poi offerti dalla playlist" width="960">
</p>

<table>
  <tr>
    <td width="50%" valign="top">
      <a href="docs/images/screenshots/video-player.png"><img src="docs/images/screenshots/video-player.png" alt="La pagina player di un link di streaming" width="100%"></a><br>
      <sub><b>Player</b>: HLS in qualsiasi browser, con la qualità scelta in base alla rete</sub>
    </td>
    <td width="50%" valign="top">
      <a href="docs/images/screenshots/media-panel.png"><img src="docs/images/screenshots/media-panel.png" alt="Il pannello media di un video nella console" width="100%"></a><br>
      <sub><b>Pannello media</b>: poster, miniature della timeline e rendition di un video</sub>
    </td>
  </tr>
  <tr>
    <td width="50%" valign="top">
      <a href="docs/images/screenshots/media-rules.png"><img src="docs/images/screenshots/media-rules.png" alt="Le regole media di un bucket" width="100%"></a><br>
      <sub><b>Regole media</b>: quali video di un bucket ricevono quali output</sub>
    </td>
    <td width="50%" valign="top">

```bash
# streaming di qualsiasi MP4 con un link revocabile
nexoctl share download --stream --expire 7d \
  local/video/talk.mp4

# un bucket video: ogni qualità, poster, miniature
nexoctl mb --with-hls local/video
nexoctl cp intro.mp4 local/video/
nexoctl media status local/video/intro.mp4
```

</td>
  </tr>
</table>

| Job su un video 1080p di 20 s (4 vCPU Xeon 2,1 GHz) | Velocità |
|---|---|
| rendition 720p + 360p + 240p | **3,2× il tempo reale** (un video di 10 minuti in circa 3 minuti) |
| una rendition | 4–4,5× il tempo reale |
| miniature, una al secondo | 74× il tempo reale |
| poster | 44 ms |

Ogni job usa tutti i core: il video è diviso nei suoi keyframe e ogni parte è decodificata e codificata sul
proprio core, con lo stesso risultato byte per byte. Dettagli: [streaming video](docs/user/video-streaming.it.md),
[media worker](docs/user/media-worker.it.md), [benchmark](docs/bench/media.md).

## 📸 Screenshot

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/images/screenshots/overview-dark.png">
  <img src="docs/images/screenshots/overview.png" alt="Panoramica della console di Nexo: deployment, stato, capacità e spazio per bucket">
</picture>

<sub>La panoramica della console web (in modalità scura se il browser la preferisce). Le immagini sono
prese da un deployment dimostrativo con `make screenshots`; la console è mostrata in inglese e la capacità dei dischi è indicativa (4 × 1 TiB).</sub>

<table>
  <tr>
    <td width="50%" valign="top">
      <a href="docs/images/screenshots/object-browser.png"><img src="docs/images/screenshots/object-browser.png" alt="Browser degli oggetti" width="100%"></a><br>
      <sub><b>Browser degli oggetti</b>: cartelle, upload, versioni, anteprime e link di condivisione</sub>
    </td>
    <td width="50%" valign="top">
      <a href="docs/images/screenshots/bucket-lifecycle.png"><img src="docs/images/screenshots/bucket-lifecycle.png" alt="Lifecycle del bucket" width="100%"></a><br>
      <sub><b>Lifecycle del bucket</b>: scadenza, versioni non correnti e upload incompleti</sub>
    </td>
  </tr>
  <tr>
    <td width="50%" valign="top">
      <a href="docs/images/screenshots/policy-editor.png"><img src="docs/images/screenshots/policy-editor.png" alt="Editor delle policy IAM" width="100%"></a><br>
      <sub><b>Editor delle policy IAM</b>: JSON delle policy AWS con completamento, validazione e builder visuale</sub>
    </td>
    <td width="50%" valign="top">
      <a href="docs/images/screenshots/access-rules.png"><img src="docs/images/screenshots/access-rules.png" alt="Regole di accesso" width="100%"></a><br>
      <sub><b>Regole di accesso</b>: consenti e nega per prefisso, glob o regex, con condizioni</sub>
    </td>
  </tr>
  <tr>
    <td width="50%" valign="top">
      <a href="docs/images/screenshots/distributions.png"><img src="docs/images/screenshots/distributions.png" alt="Distribuzioni" width="100%"></a><br>
      <sub><b>Distribuzioni</b>: bucket serviti su domini personalizzati, con TLS e traffico</sub>
    </td>
    <td width="50%" valign="top">
      <a href="docs/images/screenshots/cache.png"><img src="docs/images/screenshots/cache.png" alt="Cache delle distribuzioni" width="100%"></a><br>
      <sub><b>Cache delle distribuzioni</b>: hit ratio, banda risparmiata e domini più richiesti</sub>
    </td>
  </tr>
  <tr>
    <td width="50%" valign="top">
      <a href="docs/images/screenshots/monitoring.png"><img src="docs/images/screenshots/monitoring.png" alt="Monitoraggio" width="100%"></a><br>
      <sub><b>Monitoraggio</b>: richieste, errori, latenza e banda di ogni nodo</sub>
    </td>
    <td width="50%" valign="top">
      <a href="docs/images/screenshots/cluster.png"><img src="docs/images/screenshots/cluster.png" alt="Cluster" width="100%"></a><br>
      <sub><b>Cluster</b>: pool, erasure set, nodi e dischi a colpo d'occhio</sub>
    </td>
  </tr>
  <tr>
    <td width="50%" valign="top">
      <a href="docs/images/screenshots/users.png"><img src="docs/images/screenshots/users.png" alt="Utenti" width="100%"></a><br>
      <sub><b>Utenti</b>: utenti IAM con le loro policy e i gruppi</sub>
    </td>
    <td width="50%" valign="top">
      <a href="docs/images/screenshots/login.png"><img src="docs/images/screenshots/login.png" alt="Accesso" width="100%"></a><br>
      <sub><b>Accesso</b>: la console usa le stesse access key dell'API S3</sub>
    </td>
  </tr>
</table>

## ⚡ Prestazioni

Misurate su una macchina virtuale Xeon con 4 vCPU, con il metodo e la macchina descritti in
[docs/bench](docs/bench/README.md) (`make bench` e `make bench-media` misurano il tuo hardware e salvano la sua
descrizione con i risultati):

| warp, stessa VM | 1 disco | 4 dischi, EC 2+2 | cluster, 4 processi × 2 dischi, EC 4+4 |
|---|---|---|---|
| PUT di oggetti da 64 MiB | 727 MiB/s | 384 MiB/s | 296 MiB/s |
| GET di oggetti da 64 MiB | 3,8 GiB/s | 2,9 GiB/s | 1,4 GiB/s |
| Carico misto | 4439 op/s | 2498 op/s | 327 op/s |
| LIST | 61 680 chiavi/s | 18 604 chiavi/s | 7093 chiavi/s |

La riga del cluster esegue quattro nodi sulla stessa macchina con 4 vCPU: un cluster reale li distribuisce su
macchine e reti. I job video sono nella [sezione sopra](#-video-integrato).

## 📚 Documentazione

**📖 Sito della documentazione: <https://r3n3r0.github.io/Nexo/it/>** (inglese: <https://r3n3r0.github.io/Nexo/>),
con ricerca, il riferimento della CLI e l'Admin API.

Le stesse pagine sono in [`docs/user`](docs/user/README.it.md), in inglese con la traduzione italiana accanto a
ogni pagina (`<pagina>.it.md`; anche su <https://github.com/R3n3r0/Nexo/tree/main/docs/user>, per chi legge
questo file da un archivio di release):

| | |
|---|---|
| 🏁 [Quickstart](docs/user/quickstart.it.md) | il percorso completo in cinque minuti |
| 📦 [Installazione](docs/user/installation.it.md) | binari, deb/rpm, Homebrew, Docker, Compose, Helm, systemd |
| 🗄️ [Modalità di deployment](docs/user/deployment-modes.it.md) | singolo disco, più dischi, cluster, dimensionamento |
| ⚙️ [Configurazione](docs/user/configuration.it.md) | flag, variabili d'ambiente, configurazione dinamica |
| 🩺 [Guida operativa](docs/user/operations/monitoring.it.md) | monitoraggio, healing, aggiornamenti, backup, risoluzione dei problemi |
| 🔐 [Sicurezza](docs/user/security/users-policies.it.md) | policy, condivisione, regole di accesso, TLS, KMS |
| 🌍 [Distribuzioni e cache](docs/user/distributions/README.it.md) | domini personalizzati, con un [tutorial](docs/user/distributions/tutorial-static-site.it.md) completo |
| 🎬 [Video](docs/user/video-streaming.it.md) | streaming HLS, link di streaming e il [media worker](docs/user/media-worker.it.md) (rendition, poster, miniature) |
| 📘 [Riferimenti](docs/user/reference/cli.it.md) | CLI, [Admin API](docs/user/reference/admin-api.it.md), [compatibilità S3](docs/user/reference/s3-compatibility.it.md) |

`make docs-site` la pubblica come sito statico con MkDocs (in `bin/site`, in inglese e in italiano sotto `/it/`).

<details>
<summary><b>Documenti di progetto</b> (architettura, design, roadmap, piano di test)</summary>

| # | Documento | Contenuto |
|---|-----------|-----------|
| 1 | [Architettura](docs/01-architecture.md) | Componenti, layout del repository, stack, scelte tecniche |
| 2 | [Funzionalità](docs/02-features.md) | Catalogo delle funzionalità di server, dashboard e CLI, differenze rispetto ad S3 |
| 3 | [Cluster](docs/03-cluster.md) | Topologia, erasure coding, quorum, lock distribuiti, healing, espansione |
| 4 | [API](docs/04-api.md) | API S3 supportate, Admin API, Internode RPC |
| 5 | [Roadmap e task](docs/05-roadmap.md) | Milestone, task con ID, dipendenze e criteri di accettazione, stato |
| 6 | [Piano di test](docs/06-test-plan.md) | Strategia e casi di test per server, CLI, dashboard e cluster |
| 7 | [Sicurezza e operatività](docs/07-security-operations.md) | Threat model, TLS, KMS, osservabilità |
| 8 | [Modalità e rilascio](docs/08-deployment-modes.md) | Modalità di deployment, packaging e processo di rilascio |
| 9 | [Presigned, policy e condivisione](docs/09-presigned-policy-sharing.md) | URL presigned, POST policy, policy, share link revocabili |
| 10 | [Accessi, domini e distribuzioni](docs/10-domains-access-distributions.md) | Regole di accesso, domini, distribuzioni dietro proxy |
| 11 | [Cache per dominio](docs/11-domain-cache.md) | Cache memoria/disco, policy per livello, invalidazione, cache in cluster |
| 12 | [Architettura a servizi](docs/12-service-architecture.md) | Convenzione vincolante per servizi, rotte e composizione in `internal/app` |

Le modifiche di ogni release sono nel [CHANGELOG](CHANGELOG.md); la copertura dei test di ogni funzionalità del
catalogo è nella [matrice di tracciabilità](test/TRACEABILITY.md).

</details>

## 🛠️ Sviluppo

Servono Go (versione in `go.mod`) e, per la console, Node.js.

<details>
<summary><b>Target make</b></summary>

| Comando | Cosa esegue |
|---------|-------------|
| `make build` | console React e binari `bin/nexo` e `bin/nexoctl` con la console inclusa (`make build-go`: solo Go) |
| `make test` / `make test-race` | test Go (unit e integrazione in-process nelle tre modalità) |
| `make test-integration` | suite di integrazione S3/IAM/presigned in tutte le modalità, test lenti inclusi |
| `make test-cluster` | test del cluster con 4 processi `bin/nexo` reali (kill, pausa, riavvio) |
| `make test-s3compat` | suite di compatibilità Ceph `s3-tests` |
| `make lint` / `make web-lint` / `make web-test` / `make e2e` | lint Go, lint e test della console, Playwright |
| `make run-cluster` | cluster locale di 4 processi con S3 su `127.0.0.1:9101`, `9103`, `9105`, `9107` e console sulla porta successiva (Linux) |
| `make docs` / `make docs-check` | riferimento della CLI in `docs/user/cli` |
| `make traceability` / `make traceability-check` | matrice funzionalità → test in `test/TRACEABILITY.md` |
| `make security` | govulncheck, gosec e `npm audit` |
| `make bench` / `make bench-media` | benchmark warp del server in ogni modalità / benchmark della pipeline video ([docs/bench](docs/bench/README.md)) |
| `make build-media-worker` / `make docker-media-worker` | il binario `bin/nexo-media-worker` / la sua immagine |
| `make zap-scan` | scansione attiva autenticata OWASP ZAP dell'Admin API da `api/openapi.yaml` contro `bin/nexo` (richiede Java; [test/security/zap-api-scan.sh](test/security/zap-api-scan.sh)) |
| `make screenshots` | rigenera gli screenshot della console in `docs/images/screenshots` da un deployment dimostrativo popolato (Playwright; [scripts/screenshots](scripts/screenshots/run.sh)) |
| `make dco` | il controllo del sign-off delle pull request sui commit del branch ([CONTRIBUTING.md](CONTRIBUTING.md)) |
| `make help` | tutti i target |

</details>

Il codice segue la convenzione di [docs/12](docs/12-service-architecture.md): ogni servizio ha
un'interfaccia e un'implementazione non esportata, la composizione avviene solo in `internal/app`.
I contributi sono benvenuti: vedi [CONTRIBUTING.md](CONTRIBUTING.md).

## 📄 Licenza

Nexo è software libero distribuito con la licenza [GNU Affero General Public License v3.0 only](LICENSE)
(`AGPL-3.0-only`): se lo modifichi e permetti agli utenti di interagirci attraverso una rete, devi offrire loro il
codice sorgente della tua versione. Il file [NOTICE](NOTICE) aggiunge condizioni di attribuzione (sezione 7(b)
dell'AGPL): conservalo, e conserva "Powered by Nexo (https://github.com/R3n3r0/Nexo)" nelle note legali
dell'interfaccia utente. Le organizzazioni che non possono rispettare l'AGPL possono acquistare una
[licenza commerciale](COMMERCIAL-LICENSE.md) dal titolare del copyright; i contributi sono accettati alle
condizioni di [CONTRIBUTING.md](CONTRIBUTING.md). Archivi, pacchetti deb/rpm, immagini dei container e Helm chart
contengono `LICENSE` (e `NOTICE` negli archivi e nei pacchetti). In caso di dubbio fa fede il testo inglese di
questa sezione nel [README](README.md#-license) e il testo della licenza.
