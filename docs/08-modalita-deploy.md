# 8. Modalità di esecuzione e rilascio

Nexo ha **due modalità**, entrambe di prima classe, con lo **stesso binario** e la **stessa immagine Docker**:
la modalità si deduce dagli argomenti di avvio, senza flag dedicati né build diverse.

| Modalità | Varianti | Uso tipico |
|----------|----------|------------|
| **Singolo nodo** | **SNSD** (Single-Node Single-Drive): 1 directory, nessun erasure coding | Sviluppo, test, edge, piccoli volumi |
| | **SNMD** (Single-Node Multi-Drive): N dischi su 1 nodo, erasure coding | Server unico con più dischi, tolleranza ai guasti dei dischi |
| **Cluster** | **MNMD** (Multi-Node Multi-Drive): N nodi × M dischi, uno o più pool | Produzione, alta disponibilità, scalabilità |

Tutte le funzionalità (S3, IAM, policy, URL presigned, versioning, lifecycle, eventi, SSE, dashboard, CLI)
sono disponibili in **tutte** le modalità. Le uniche differenze:

| Funzionalità | SNSD | SNMD | Cluster |
|--------------|------|------|---------|
| API S3, IAM, policy, presigned, POST policy | ✅ | ✅ | ✅ |
| Dashboard web e CLI | ✅ | ✅ | ✅ |
| Versioning, object lock, lifecycle, SSE, eventi, quota | ✅ | ✅ | ✅ |
| Tolleranza al guasto di dischi | ❌ (affidata al sottosistema di storage) | ✅ fino a P dischi | ✅ fino a P dischi per set |
| Tolleranza al guasto di nodi | ❌ | ❌ | ✅ |
| Bitrot detection | ✅ (hash) / riparazione ❌ | ✅ con riparazione | ✅ con riparazione |
| Healing | — | ✅ | ✅ |
| Lock distribuiti, RPC internode | — (lock locali) | — (lock locali) | ✅ |
| Espansione | ❌ | ➜ migrazione | ✅ aggiunta di pool |
| Pagina dashboard "Cluster" | Mostra 1 nodo/1 disco | Mostra 1 nodo e i dischi | Pool, nodi, dischi, set |

## 8.1 Rilevamento della modalità

```
nexo server /data                                   → SNSD
nexo server /data{1...4}                            → SNMD (EC su 4 dischi)
nexo server http://node{1...4}:9000/data{1...4}     → Cluster (1 pool)
```

- Regole: tutti path locali e 1 solo → SNSD; tutti path locali e > 1 → SNMD; almeno un URL → Cluster
  (tutti gli endpoint devono essere URL e il nodo deve riconoscere i propri tramite IP/hostname locali).
- La modalità è registrata in `format.json` (`"mode": "snsd" | "erasure"`): avviare un disco SNSD con una topologia
  erasure (o viceversa) fallisce con un errore esplicativo.
- La dashboard e `nexoctl admin info` mostrano sempre la modalità corrente.

### Implementazione
- `ObjectLayer` ha due implementazioni sotto la stessa interfaccia:
  - `fsObjects` (SNSD): mappa diretta su file system, `xl.meta` per metadati/versioni, nessun encoding;
  - `ServerPools → ErasureSets → ErasureObjects` (SNMD e Cluster).
- In SNMD lo `StorageAPI` è sempre locale e il locker è locale: nessun RPC avviato, nessun overhead di rete.
- In Cluster gli stessi componenti usano `storageRESTClient` e `dsync`.
- **Tutta la suite di test S3/IAM/presigned/dashboard/CLI viene eseguita in tutte e tre le modalità** (vedi §8.6).

## 8.2 Migrazione tra modalità
| Da → A | Procedura |
|--------|-----------|
| SNSD → SNMD/Cluster | `nexoctl mirror --preserve old/ new/` + `nexoctl admin export/import` di IAM, config e metadati bucket (policy, lifecycle, eventi…) |
| SNMD → Cluster | Come sopra, oppure aggiunta di un pool cluster e decommission del pool SNMD (P2) |
| Cluster → Cluster più grande | Aggiunta di pool (nessuna migrazione) |

Comandi dedicati: `nexoctl admin cluster export ALIAS > backup.zip` (IAM + config + metadati di tutti i bucket)
e `nexoctl admin cluster import ALIAS backup.zip` (`--dry-run`, `--mode merge|replace`). L'import va eseguito
prima del `mirror --preserve`: crea i bucket con il loro versioning, così il target conserva tutte le versioni
copiate (un bucket target senza versioning con più versioni da copiare è rifiutato). Verificato da TM-04
(`test/integration/migration_test.go`).

## 8.3 Distribuzione come binario

| Artefatto | Contenuto |
|-----------|-----------|
| `nexo` | Server + dashboard React embedded (statico, `CGO_ENABLED=0`) |
| `nexoctl` | CLI (statica) |
| Piattaforme | linux/amd64, linux/arm64, darwin/amd64, darwin/arm64, windows/amd64 (`nexoctl` anche windows/arm64) |
| Pacchetti | `.tar.gz`/`.zip`, `.deb`/`.rpm` (`nexo` con unit systemd e `nexoctl` con i completamenti della shell), cask Homebrew, checksum SHA256 firmati con cosign, SBOM SPDX (dettaglio in §8.7) |

Esempi:

```bash
# Singolo nodo
NEXO_ROOT_USER=admin NEXO_ROOT_PASSWORD=supersegreta ./nexo server /srv/nexo
# Console: http://localhost:9001   S3: http://localhost:9000

# Cluster (stesso comando su ogni nodo)
./nexo server http://nexo{1...4}.lan:9000/mnt/disk{1...4}

# CLI
./nexoctl alias set local http://localhost:9000 admin supersegreta
./nexoctl mb local/foto && ./nexoctl cp img.jpg local/foto/
./nexoctl share download --expire 2h local/foto/img.jpg
```

Il binario include `nexo version`, `nexo server --help` con esempi per ogni modalità e un check di avvio
(`nexo server --dry-run …`) che valida topologia, permessi dei dischi, porte e certificati senza avviare il servizio.

## 8.4 Distribuzione con Docker

### Immagini
| Immagine | Base | Contenuto |
|----------|------|-----------|
| `ghcr.io/r3n3r0/nexo` | `gcr.io/distroless/static:nonroot` | `nexo` + `nexoctl` (utile per `docker exec`) |
| `ghcr.io/r3n3r0/nexoctl` | `distroless/static` | Solo CLI, per pipeline CI e uso da container |

- Multi-arch (amd64/arm64), firmate con cosign, SBOM allegato, tag `latest`, `vX.Y.Z`, `vX.Y`.
- Utente non-root (UID 65532), `VOLUME /data`, `EXPOSE 9000 9001`, `HEALTHCHECK` con `nexo healthcheck`
  (sottocomando interno, perché l'immagine distroless non ha `curl`): sonda la console su loopback e, con
  `NEXO_CERTS_DIR` (o `--certs-dir`), passa a HTTPS fidandosi di `public.crt` e `CAs/` della directory e
  verificando la catena ma non i nomi (il certificato copre i nomi dei nodi e quelli pubblici, di rado
  `127.0.0.1`); con `--url` esplicito la verifica TLS è completa.
- Segreti supportati tramite `NEXO_ROOT_USER_FILE` / `NEXO_ROOT_PASSWORD_FILE` (Docker/Swarm secrets).

### Singolo nodo
```bash
docker run -d --name nexo -p 9000:9000 -p 9001:9001 \
  -e NEXO_ROOT_USER=admin -e NEXO_ROOT_PASSWORD=supersegreta \
  -v nexo-data:/data ghcr.io/r3n3r0/nexo server /data

# SNMD: più volumi
docker run -d -p 9000:9000 -p 9001:9001 \
  -v /mnt/d1:/data1 -v /mnt/d2:/data2 -v /mnt/d3:/data3 -v /mnt/d4:/data4 \
  --env-file nexo.env ghcr.io/r3n3r0/nexo server /data{1...4}

# CLI dal container
docker run --rm -it --network host ghcr.io/r3n3r0/nexoctl ls local
```

### Cluster con Docker Compose (`deploy/docker/compose.cluster.yml`)
```yaml
x-nexo: &nexo
  image: ghcr.io/r3n3r0/nexo:latest
  command: server --console-address :9001 http://nexo{1...4}:9000/data{1...2}
  env_file: nexo.env
  healthcheck:
    test: ["CMD", "/nexo", "healthcheck"]
    interval: 10s
services:
  nexo1: { <<: *nexo, hostname: nexo1, volumes: [d1-1:/data1, d1-2:/data2] }
  nexo2: { <<: *nexo, hostname: nexo2, volumes: [d2-1:/data1, d2-2:/data2] }
  nexo3: { <<: *nexo, hostname: nexo3, volumes: [d3-1:/data1, d3-2:/data2] }
  nexo4: { <<: *nexo, hostname: nexo4, volumes: [d4-1:/data1, d4-2:/data2] }
  lb:                       # nginx davanti ai 4 nodi per S3 (9000) e console (9001)
    image: nginx:1.27-alpine
    ports: ["9000:9000", "9001:9001"]
    volumes: [./nginx.conf:/etc/nginx/nginx.conf:ro]
volumes: { d1-1: {}, d1-2: {}, d2-1: {}, d2-2: {}, d3-1: {}, d3-2: {}, d4-1: {}, d4-2: {} }
```

File forniti in `deploy/docker/`:
| File | Scopo |
|------|-------|
| `compose.single.yml` | SNSD con volume nominato |
| `compose.snmd.yml` | SNMD con 4 volumi |
| `compose.cluster.yml` + `nginx.conf` | Cluster 4 nodi × 2 dischi con load balancer (sticky non necessario) |
| `compose.cluster-tls.yml` | Cluster con TLS (certificati montati in `/certs`) |
| `compose.multi-proxy.yml` + `multi-proxy/` | Cluster 4 nodi × 2 dischi dietro **due proxy** (nginx con `X-Forwarded-*`, HAProxy con PROXY protocol v2), domini e origin secret diversi, volume `/cache` per nodo (§8.5) |
| `compose.observability.yml` | Prometheus + Grafana con dashboard Nexo preconfigurate |

### Altri orchestratori
- **Docker Swarm** ([`deploy/swarm/stack.yml`](../deploy/swarm/README.md)): 4 nodi × 2 dischi, ogni servizio
  legato al suo nodo Swarm con `deploy.placement` (`node.labels.nexo.node`), così i volumi nominati lo seguono;
  `endpoint_mode: dnsrr` (indirizzo del task, niente VIP IPVS sulle connessioni internode), credenziali come
  Docker secret (`NEXO_ROOT_*_FILE`), rete overlay cifrata, due nginx con la configurazione di
  `compose.cluster.yml`, aggiornamenti un nodo alla volta.
- **Kubernetes** ([`deploy/helm/nexo`](../deploy/helm/nexo/README.md)): `mode: standalone` (Deployment con un
  PVC per disco: SNSD o SNMD) oppure `mode: distributed` (StatefulSet `Parallel` con `replicas × pools` pod e
  `drivesPerNode` PVC ciascuno, headless service che pubblica i pod non ancora pronti, endpoint con ellissi
  generati dal template, PodDisruptionBudget, anti-affinity, probe su `/nexo/health/live|ready`, utente non
  root e file system in sola lettura, Secret per credenziali e KMS, TLS da Secret `kubernetes.io/tls`,
  ServiceMonitor, Ingress per S3/console/distribuzioni, volume della cache, `helm test` con health del
  cluster e round trip S3).
- **systemd** ([`deploy/systemd`](../deploy/systemd/README.md)): unit con sandbox (`ProtectSystem=strict`,
  nessuna capability, filtro delle syscall: esposizione 1,5 "OK" per `systemd-analyze security`),
  `/etc/default/nexo`, istruzioni per il cluster su più macchine.

## 8.5 Reverse proxy e URL pubblici
Fondamentale perché gli **URL presigned** restino validi dietro proxy/load balancer:
- la firma SigV4 include l'header `Host`: il proxy deve inoltrarlo invariato, porta inclusa
  (`proxy_set_header Host $http_host`); lo stesso header sceglie la distribuzione (docs/10 §10.2);
- `NEXO_SERVER_URL` (es. `https://s3.example.com`) è l'URL pubblico usato da console e CLI per generare link
  condivisibili; `NEXO_BROWSER_REDIRECT_URL` è l'URL pubblico della console;
- il client reale (indirizzo e HTTPS) arriva da proxy fidati (`NEXO_TRUSTED_PROXIES`) in **una** famiglia di
  header, scelta con `NEXO_FORWARDED_HEADERS`: `x-forwarded` (default: `X-Forwarded-For/Proto/Host`) oppure
  `forwarded` (RFC 7239); l'altra famiglia è ignorata, perché un proxy la inoltra così come l'ha mandata il
  client. `X-Forwarded-For` è letto da destra: le voci aggiunte dal client a sinistra non contano;
- in alternativa il **PROXY protocol** v1/v2 sui listener elencati in `NEXO_PROXY_PROTOCOL` (`s3`, `console`,
  `web`): l'header è letto prima del TLS, usato solo se la connessione viene da un proxy fidato (letto e
  ignorato altrimenti) e facoltativo (un proxy fidato che manda `X-Forwarded-*`, le RPC internode e gli
  health check si connettono senza); il TLV SSL di `send-proxy-v2-ssl` rende la richiesta HTTPS;
- body illimitato, niente buffering, timeout lunghi; `/nexo/rpc/` mai esposto sul dominio pubblico;
- un nodo che si sta fermando risponde `503` (anche `/nexo/health/ready`) finché gli health check non lo tolgono
  dal giro: il proxy ritenta le letture su un altro nodo (nginx `proxy_next_upstream ... http_503`, HAProxy
  `retry-on ... 503` limitato a `GET/HEAD/OPTIONS`), gli SDK S3 ritentano le scritture.

Configurazioni complete e commentate in `deploy/proxy/` (nginx, Caddy, Traefik, HAProxy, Cloudflare) per S3,
console e distribuzioni, con l'**origin secret** di ogni proxy (docs/10 §10.2).

### Più proxy davanti allo stesso cluster (`compose.multi-proxy.yml`)
| Servizio | Proxy | Domini | Client verso Nexo |
|----------|-------|--------|-------------------|
| `proxy-a` | nginx (`multi-proxy/nginx.conf.template`) | `s3.alpha.example` (S3), `console.alpha.example`, `*.alpha.example` (distribuzioni) | `X-Forwarded-*` |
| `proxy-b` | HAProxy (`multi-proxy/haproxy.cfg`) | `*.beta.example` (distribuzioni) | PROXY protocol v2 con TLV SSL |

- I nodi si fidano solo dei due proxy (indirizzi fissi sulla rete compose, fuori dall'`ip_range` assegnato ai
  nodi) e accettano il PROXY protocol sulla porta S3 (`NEXO_PROXY_PROTOCOL=s3`).
- Ogni proxy aggiunge il proprio `X-Origin-Secret`; le distribuzioni lo referenziano per nome
  (`${secret:proxy-a}`, `${secret:proxy-b}`), risolto su ogni nodo da `NEXO_SECRET_PROXY_A/B` (valori in `.env`):
  una richiesta che arriva a un nodo senza uno dei due segreti è rifiutata (403).
- Volume `/cache` per nodo (cache delle distribuzioni, docs/11).
- `multi-proxy/gen-certs.sh` crea CA e certificati di sviluppo dei due proxy, `multi-proxy/site.yaml` due
  distribuzioni d'esempio (website e s3) servite da entrambi.

Verifica: `make test-proxy` (`test/proxy`, build tag `proxy`, job CI `proxy`) esegue TCD-01..03, TCC-C-05 e
TPS-19 sui domini custom con due proxy Go in-process (uno `X-Forwarded-*`, uno PROXY protocol v2) in SNSD,
SNMD e cluster, e gli **stessi file** nginx/HAProxy di `compose.multi-proxy.yml` con i binari reali davanti a un
cluster di 4 nodi (`TestRealProxies`; job CI con `NEXO_PROXY_REAL=1`). Il job `deploy-files` valida inoltre
la sintassi di tutte le configurazioni di `deploy/proxy` e del compose (`nginx -t`, `haproxy -c`,
`caddy validate`, `docker compose config`).

## 8.6 Test specifici di modalità e rilascio

| ID | Test | Atteso |
|----|------|--------|
| TM-01 | Suite integrazione S3 + IAM + presigned eseguita come matrice `{SNSD, SNMD 4 dischi, Cluster 4×2}` | Tutto verde nelle 3 modalità |
| TM-02 | Suite Playwright dashboard e testscript CLI in matrice `{SNSD, Cluster}` | Verde |
| TM-03 | Rilevamento modalità da argomenti; topologia incompatibile con `format.json` | Modalità corretta / errore chiaro |
| TM-04 | Migrazione SNSD → Cluster con `mirror` + `admin cluster export/import` | Oggetti, versioni, IAM, policy, lifecycle identici |
| TM-05 | `docker run` single-node: healthcheck `healthy` entro 10 s, console raggiungibile, login OK | — |
| TM-06 | `docker compose -f compose.cluster.yml up`: cluster pronto, 1 container fermato → nessun errore client | — |
| TM-07 | Container eseguito come non-root con volume host: permessi corretti o errore chiaro | — |
| TM-08 | `*_FILE` secrets | Credenziali lette da file |
| TM-09 | Presigned URL generato dietro nginx (compose) scaricabile da fuori | 200 |
| TM-10 | Binari di release: avvio smoke su ogni OS/arch in CI (QEMU per arm64), `nexo version` coerente con il tag | — |
| TM-11 | Immagini: firma cosign verificabile, scansione trivy senza vulnerabilità High/Critical | — |
| TM-12 | `nexo server --dry-run` su configurazioni valide/invalide | Exit code e messaggi attesi |
| TM-13 | Upgrade dalla release precedente (`make test-upgrade`, `test/upgrade`): SNMD con dati di ogni tipo, rolling upgrade di un cluster 4×2 sotto carico, downgrade e rollback di un nodo ([06 §6.1](06-piano-test.md)) | Nessuna perdita di dati, nessun errore client |

**Aggiornamenti e downgrade.** Una release legge i dischi scritti dalle release precedenti della stessa major
(`format.json` v1, `xl.meta` 1.x, protocollo internodo v1): l'aggiornamento di un nodo singolo è un riavvio con
il nuovo binario, quello di un cluster un nodo alla volta (SIGTERM, avvio, attesa di `/nexo/health/cluster`) senza
interrompere i client dietro il load balancer; il bootstrap rifiuta solo nodi di un'altra major. Il downgrade
(e il rollback di un nodo durante un rolling upgrade) è supportato fra release con gli stessi formati, come oggi
fra M7 e la release corrente: verificato da TM-13. Una release che cambia un formato ne alza la versione e la
release precedente rifiuta i dischi senza toccarli: in quel caso il downgrade non è supportato e va annunciato
nelle note di rilascio.

Prima di aggiornare il primo nodo da `v0.7.0` (sezione «Aggiornamento» del [CHANGELOG](../CHANGELOG.md)):

1. con `--tls-internode-verify`, copiare la CA che firma i certificati dei nodi in `CAs/` della directory dei
   certificati su **tutti** i nodi: i certificati client dei peer sono verificati solo con `CAs/` (e con la foglia
   di default), non più con la catena di `public.crt`; senza, ogni nodo aggiornato rifiuta l'RPC dei peer fino alla
   perdita del quorum. `nexo server --dry-run` lo segnala con un avviso;
2. chi serve distribuzioni sulla porta S3 senza `--server-url`/`--domain` imposta `NEXO_S3_DISTRIBUTIONS=on` (o
   `NEXO_SERVER_URL`/`NEXO_DOMAIN`), altrimenti i domini smettono di rispondere sulla porta 9000;
3. durante il rolling upgrade le GET coordinate da un nodo aggiornato con dischi su nodi `v0.7.0` (senza
   `storage/open-file`) tengono il lock di lettura fino alla fine, come in `v0.7.0` (TestMixedClusterPinnedRead).

## 8.7 Rilascio (T-9.5)

Un tag `v*` avvia [`.github/workflows/release.yml`](../.github/workflows/release.yml): GoReleaser v2.18.3 con
[`.goreleaser.yaml`](../.goreleaser.yaml), poi i job di verifica. Nessun segreto a lunga scadenza: le firme sono
**cosign keyless** (Sigstore, certificato emesso per l'identità OIDC del workflow).

### Cosa produce una release

| Artefatto | Dettaglio |
|-----------|-----------|
| Binari | `nexo` (con la console, `-tags embedui`) per linux/darwin/windows × amd64/arm64 (non windows/arm64); `nexoctl` anche per windows/arm64. Statici (`CGO_ENABLED=0`), `-trimpath`, data e mtime = commit (riproducibili); `nexo version` stampa il tag |
| Archivi | `nexo_X.Y.Z_<os>_<arch>.tar.gz` con LICENSE, NOTICE, COMMERCIAL-LICENSE.md, README, unit e file env di systemd (`.zip` su Windows, con LICENSE, NOTICE, COMMERCIAL-LICENSE.md e README soli: build e archivio `nexo-windows`); `nexoctl_…` con i completamenti bash/zsh/fish; archivio dei sorgenti `nexo_X.Y.Z_source.tar.gz` |
| Pacchetti | `.deb` e `.rpm` per amd64/arm64: `nexo` (binario in `/usr/bin`, unit in `/usr/lib/systemd/system`, `/etc/default/nexo` come conffile `0640 root:nexo` con la password vuota, utente `nexo` creato da `preinstall` con `useradd` (deb: `Pre-Depends: passwd`) e dichiarato in `/usr/lib/sysusers.d/nexo.conf`, `/var/lib/nexo/data`; mai avviato né riavviato dagli script; la rimozione ferma il servizio e conserva i dati) e `nexoctl` (binario e completamenti); `nexo` raccomanda `nexoctl` |
| Checksum | `checksums.txt` (SHA-256 di archivi, pacchetti, SBOM, sorgenti) + `checksums.txt.sigstore.json` (bundle cosign: firma, certificato, voce del log di trasparenza Rekor) |
| SBOM | Un documento SPDX JSON per archivio (`<archivio>.sbom.json`, syft v1.54.1): moduli Go e toolchain di ogni binario. I pacchetti contengono gli stessi binari linux degli archivi |
| Immagini | `ghcr.io/r3n3r0/nexo` (nexo + nexoctl) e `ghcr.io/r3n3r0/nexoctl`, indice multi-arch linux/amd64+arm64 costruito da buildx con i binari già compilati (`deploy/docker/Dockerfile*.goreleaser`, equivalenti ai Dockerfile della radice), attestazioni SBOM e provenance (`mode=max`) di BuildKit, firma cosign; tag `vX.Y.Z` e, per le release finali, `vX.Y` e `latest` |
| Helm | `nexo-X.Y.Z.tgz` allegato alla release e pubblicato in `oci://ghcr.io/r3n3r0/charts/nexo` (firmato con cosign) |
| Homebrew | Cask `nexo` e `nexoctl` in `r3n3r0/homebrew-tap` (`brew install r3n3r0/tap/nexoctl`), solo se il segreto `HOMEBREW_TAP_GITHUB_TOKEN` esiste; i binari macOS non sono notarizzati: il cask rimuove l'attributo di quarantena |

### Verificare una release

```bash
TAG=v1.0.0
# Identità delle firme: il workflow di release del tag, con le maiuscole del repository
# su GitHub (cosign la confronta esattamente).
ID="https://github.com/R3n3r0/Nexo/.github/workflows/release.yml@refs/tags/$TAG"
ISSUER=https://token.actions.githubusercontent.com

# 1. firma di checksums.txt (cosign >= 2.4), poi i file scaricati
cosign verify-blob checksums.txt --bundle checksums.txt.sigstore.json \
  --certificate-identity "$ID" --certificate-oidc-issuer "$ISSUER"
sha256sum --ignore-missing -c checksums.txt          # macOS: shasum -a 256 --ignore-missing -c

# 2. SBOM di un archivio (coperto dai checksum)
jq -r '.packages[] | "\(.name) \(.versionInfo)"' nexo_${TAG#v}_linux_amd64.tar.gz.sbom.json | head

# 3. immagini: firma, SBOM e provenance attestate nell'indice multi-arch
cosign verify ghcr.io/r3n3r0/nexo:$TAG --certificate-identity "$ID" --certificate-oidc-issuer "$ISSUER"
docker buildx imagetools inspect ghcr.io/r3n3r0/nexo:$TAG --format '{{ json .SBOM }}'
docker buildx imagetools inspect ghcr.io/r3n3r0/nexo:$TAG --format '{{ json .Provenance }}'

# 4. chart Helm
cosign verify ghcr.io/r3n3r0/charts/nexo:${TAG#v} --certificate-identity "$ID" --certificate-oidc-issuer "$ISSUER"
```

Il job `verify` della release esegue gli stessi controlli (TM-11) e la scansione trivy delle immagini
(High/Critical con fix disponibile: il job fallisce); il job `smoke` (TM-10) estrae gli archivi su
linux/amd64, linux/arm64 (QEMU), darwin/arm64, darwin/amd64 (Rosetta) e windows/amd64 ed esegue
[`packaging/smoke.sh`](../packaging/smoke.sh): versione uguale al tag, `--dry-run`, server su un disco,
console servita, round trip S3 con `nexoctl`, arresto con SIGTERM (su Windows solo versione e `--dry-run`).

### Riprodurre e controllare in locale (senza pubblicare)

| Comando | Cosa fa |
|---------|---------|
| `make release-tools` | Installa in `bin/tools` GoReleaser, Helm, kubeconform e syft alle versioni fissate nel Makefile |
| `make release-check` | `goreleaser check` |
| `make release-snapshot` | Tutta la release in `dist/` (versione `X.Y.Z-next`), senza immagini né firme (`RELEASE_SKIP=docker,sign`; con un demone Docker `RELEASE_SKIP=sign` costruisce anche le immagini) |
| `make release-smoke` | `packaging/smoke.sh` sui binari linux/amd64 dello snapshot e, con `qemu-aarch64-static`, linux/arm64 |
| `make helm-lint` | `helm lint --strict`, `helm template` e `kubeconform -strict` per ciascuna versione di Kubernetes (1.27 e 1.34, `--kube-version` esplicito: un helm compilato con `go install` assume la 1.20), CRD ServiceMonitor, con i valori di default e `deploy/helm/nexo/ci/*-values.yaml`; poi gli endpoint generati per ogni modalità (SNSD, SNMD, distribuito, due pool, TLS) e gli errori dei valori invalidi |
| `make systemd-verify` | `systemd-analyze verify` e `security` (esposizione ≤ 2,0) della unit |
| `make hadolint` / `make packaging` | Dockerfile; tutti i controlli che non richiedono Docker |

Il job CI `packaging` esegue a ogni pull request lo snapshot con le immagini, TM-10 su amd64 e arm64,
l'installazione dei `.deb` con il systemd del runner (servizio che rifiuta di partire senza password, poi
parte, risponde, round trip S3, `reload`, `stop`, rimozione che conserva i dati), i `.rpm` in un container
Fedora, il chart, la unit e i Dockerfile; il job `deploy-files` valida lo stack Swarm (`docker stack config`).

### Validato in locale e solo configurato

Ultima verifica (GoReleaser v2.18.3, Helm v3.22.0 ufficiale e `go install`, kubeconform v0.7.0,
syft v1.54.1, hadolint v2.14.0, actionlint v1.7.7 con shellcheck 0.9.0, systemd 255, QEMU user,
Docker CLI senza demone):

| Elemento | Esito |
|----------|-------|
| `goreleaser check` | Valido, nessuna deprecazione |
| `goreleaser release --snapshot --clean --skip=publish,sign,docker` | 11 binari (`nexo` con la console, `nexoctl`), 11 archivi + sorgenti, 8 pacchetti deb/rpm, 11 SBOM SPDX 2.3, `checksums.txt` (31 voci, tutte verificate con `sha256sum -c`), cask Homebrew `nexo` e `nexoctl` |
| Binari | `nexo version` (linux/amd64) = versione dello snapshot e commit; `file`: ELF statici x86-64/aarch64, Mach-O x86_64/arm64, PE32+ x86-64/Aarch64 |
| TM-10 (`make release-smoke`) | Verde su linux/amd64 e linux/arm64 (`qemu-aarch64-static`): versione, `--dry-run`, server, console servita, round trip S3, SIGTERM |
| Pacchetti | `dpkg-deb -I/-c`, `rpm -qip/-qlvp/--scripts`: file, permessi e proprietari (`root:nexo 0640` per `/etc/default/nexo`, `nexo:nexo 0750` per `/var/lib/nexo/data`), conffile, script e `Recommends` |
| Helm (`make helm-lint`) | `lint --strict`, `template` e `kubeconform -strict` verdi per default, standalone, SNMD, distribuito e distribuito TLS su Kubernetes 1.27 e 1.34; asserzioni sugli endpoint e sui valori invalidi |
| Compose e Swarm | `docker compose config` di tutti i `compose.*.yml`, `docker stack config -c deploy/swarm/stack.yml` |
| systemd | `systemd-analyze verify` pulito, `security`: esposizione 1,5 "OK" |
| Dockerfile | hadolint senza segnalazioni sui quattro Dockerfile |
| Workflow e script | actionlint su `release.yml` e `ci.yml` (con shellcheck: restano due SC2046 voluti nei job `lint-go` e `test-go`), shellcheck degli script di `packaging/` e `deploy/helm/check.sh` |
| Healthcheck TLS | Server reale con `NEXO_CERTS_DIR`: la sonda HTTP precedente riceveva `400`, quella nuova è `ok` anche con un certificato che nomina solo `nexo1` (ricaricato a caldo) e fallisce con una CA estranea |

Solo configurato (richiede un demone Docker, l'identità OIDC del workflow o la pubblicazione): immagini
multi-arch `dockers_v2` con attestazioni SBOM/provenance, firme cosign keyless di checksum, immagini e
chart, push su ghcr.io e nel tap Homebrew, job `verify` (TM-11) e `smoke` su darwin/windows della
release, installazione dei pacchetti con systemd e in Fedora (job CI `packaging`).

Licenza: AGPL-3.0-only con i termini di attribuzione di `NOTICE` (AGPL §7(b)) e licenze commerciali
(`COMMERCIAL-LICENSE.md` nella radice del repository); `LICENSE` è in archivi, pacchetti, immagini e chart, `NOTICE` in
archivi e pacchetti. Decisioni aperte del rilascio: repository `r3n3r0/homebrew-tap` con il segreto `HOMEBREW_TAP_GITHUB_TOKEN`, visibilità
pubblica dei pacchetti ghcr.io (privati alla prima pubblicazione). The toolchain is Go 1.27.2
(docs/security/threat-model.md §10.1): govulncheck finds no reachable vulnerabilities, so trivy in the `verify` job
has no known standard library vulnerabilities left to report.
