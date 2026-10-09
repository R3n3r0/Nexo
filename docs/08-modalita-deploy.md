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
| Pacchetti | `.tar.gz`/`.zip`, `.deb`, `.rpm`, Homebrew tap, unit systemd, checksum SHA256 + firma cosign, SBOM |

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
  (sottocomando interno, perché l'immagine distroless non ha `curl`).
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
| `compose.observability.yml` | Prometheus + Grafana con dashboard Nexo preconfigurate |

### Altri orchestratori
- **Docker Swarm**: stack file con `deploy.placement` per legare ogni servizio al suo nodo/volume.
- **Kubernetes**: Helm chart con `mode: standalone | distributed` (StatefulSet + headless service per il cluster).

## 8.5 Reverse proxy e URL pubblici
Fondamentale perché gli **URL presigned** restino validi dietro proxy/load balancer:
- la firma SigV4 include l'header `Host`: il proxy deve inoltrarlo invariato (`proxy_set_header Host $http_host`);
- `NEXO_SERVER_URL` (es. `https://s3.example.com`) è l'URL pubblico usato da console e CLI per generare link
  condivisibili; `NEXO_BROWSER_REDIRECT_URL` è l'URL pubblico della console;
- supporto a `X-Forwarded-Proto/Host` solo da proxy fidati (`NEXO_TRUSTED_PROXIES`).

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
