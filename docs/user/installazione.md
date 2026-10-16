# Installazione

Nexo si distribuisce come due binari statici, `nexo` (server con la console web incorporata) e `nexoctl`
(CLI), e come immagini Docker multi-arch. La modalità (singolo disco, più dischi, cluster) non dipende
dal pacchetto ma dagli argomenti di `nexo server`: vedi [Modalità di deployment](modalita.md).

| Piattaforma | `nexo` | `nexoctl` |
|-------------|--------|-----------|
| Linux amd64, arm64 | ✅ | ✅ |
| macOS amd64, arm64 | ✅ | ✅ |
| Windows amd64 | ✅ | ✅ |
| Windows arm64 | — | ✅ |

Ogni release contiene archivi, pacchetti `.deb`/`.rpm`, `checksums.txt` firmato con cosign (keyless) e
uno SBOM SPDX per archivio. La verifica delle firme è descritta in
[docs/08 §8.7](https://github.com/R3n3r0/Nexo/blob/main/docs/08-modalita-deploy.md#verificare-una-release).

## Binari (archivi)

```bash
VERSION=1.0.0
curl -LO https://github.com/R3n3r0/Nexo/releases/download/v$VERSION/nexo_${VERSION}_linux_amd64.tar.gz
curl -LO https://github.com/R3n3r0/Nexo/releases/download/v$VERSION/nexoctl_${VERSION}_linux_amd64.tar.gz
curl -LO https://github.com/R3n3r0/Nexo/releases/download/v$VERSION/checksums.txt
sha256sum --ignore-missing -c checksums.txt

tar xzf nexo_${VERSION}_linux_amd64.tar.gz
tar xzf nexoctl_${VERSION}_linux_amd64.tar.gz
sudo install -m 0755 nexo nexoctl /usr/local/bin/
nexo version
nexoctl --version
```

- L'archivio di `nexo` contiene anche `systemd/nexo.service`, `systemd/nexo.env.example` e il loro README
  (vedi [systemd](#systemd)).
- L'archivio di `nexoctl` contiene i completamenti della shell in `completions/`; si possono anche
  generare con `nexoctl completion bash|zsh|fish|powershell` (vedi
  [`nexoctl completion`](cli/nexoctl_completion.md)).
- Su Windows gli archivi sono `.zip`.

## Pacchetti deb e rpm

```bash
# Debian, Ubuntu
sudo apt install ./nexo_1.0.0_amd64.deb ./nexoctl_1.0.0_amd64.deb
# Fedora, RHEL, openSUSE
sudo dnf install ./nexo-1.0.0-1.x86_64.rpm ./nexoctl-1.0.0-1.x86_64.rpm
```

Il pacchetto `nexo`:

- installa `/usr/bin/nexo` e la unit `/usr/lib/systemd/system/nexo.service`;
- crea l'utente e il gruppo di sistema `nexo` e la directory `/var/lib/nexo/data` (disco di default);
- installa `/etc/default/nexo` (proprietario `root:nexo`, modo 0640) con la password root **vuota**: il
  servizio rifiuta di partire finché non la imposti;
- non abilita, non avvia e non riavvia mai il servizio da solo; la rimozione ferma il servizio e conserva
  i dati.

Il pacchetto `nexoctl` installa il binario e i completamenti di bash, zsh e fish. Dopo l'installazione:

```bash
sudoedit /etc/default/nexo          # NEXO_ROOT_PASSWORD, NEXO_ENDPOINTS
sudo systemctl enable --now nexo
journalctl -u nexo -f
```

## Homebrew

```bash
brew install r3n3r0/tap/nexoctl
brew install r3n3r0/tap/nexo
```

I cask installano i binari di macOS e Linux; i binari macOS sono firmati con cosign ma non notarizzati
da Apple (il cask rimuove l'attributo di quarantena).

## Docker

| Immagine | Contenuto |
|----------|-----------|
| `ghcr.io/r3n3r0/nexo` | `/nexo` e `/nexoctl`, base distroless, utente non root (UID 65532), `VOLUME /data`, porte 9000 e 9001, `HEALTHCHECK` con `nexo healthcheck` |
| `ghcr.io/r3n3r0/nexoctl` | solo la CLI, per pipeline CI e uso da container |

Tag: `vX.Y.Z`, e per le release finali `vX.Y` e `latest`. Le immagini sono multi-arch (linux/amd64,
linux/arm64), firmate con cosign e con attestazioni SBOM e provenance.

```bash
docker run -d --name nexo -p 9000:9000 -p 9001:9001 \
  -e NEXO_ROOT_USER=admin -e NEXO_ROOT_PASSWORD=supersegreta \
  -v nexo-data:/data ghcr.io/r3n3r0/nexo:latest server /data
```

- **Segreti**: `NEXO_ROOT_USER_FILE` e `NEXO_ROOT_PASSWORD_FILE` leggono le credenziali da file (Docker o
  Swarm secrets); la variabile `_FILE` vince su quella semplice. Lo stesso vale per
  `NEXO_KMS_SECRET_KEY_FILE` e per i segreti `NEXO_SECRET_<NOME>_FILE`.
- **Volumi dell'host**: il container gira come UID 65532, quindi una directory montata deve appartenergli:
  `sudo chown 65532:65532 /srv/nexo` prima di `-v /srv/nexo:/data`. I volumi nominati non richiedono nulla.
- **Più dischi**: un volume per disco e le ellissi negli argomenti:
  `-v /mnt/d1:/data1 … -v /mnt/d4:/data4 ghcr.io/r3n3r0/nexo server /data{1...4}`.
- **TLS**: monta la directory dei certificati e imposta `NEXO_CERTS_DIR`; anche l'healthcheck passa a
  HTTPS (vedi [TLS e certificati](sicurezza/tls.md)).
- **CLI da container**:
  `docker run --rm -it --network host -v nexoctl-config:/home/nonroot/.nexoctl ghcr.io/r3n3r0/nexoctl ls local`.

Per costruire le immagini dai sorgenti: `make docker` e `make docker-nexoctl`.

## Docker Compose

I file in [`deploy/docker`](https://github.com/R3n3r0/Nexo/tree/main/deploy/docker) coprono tutte le
modalità; si parte sempre da `nexo.env`:

```bash
cd deploy/docker
cp nexo.env.example nexo.env              # imposta NEXO_ROOT_USER e NEXO_ROOT_PASSWORD
docker compose -f compose.single.yml up -d
```

| File | Deployment |
|------|------------|
| `compose.single.yml` | SNSD, un volume nominato (più un volume `/cache` per la cache delle distribuzioni) |
| `compose.snmd.yml` | SNMD con 4 volumi (`server /data{1...4}`) |
| `compose.cluster.yml` + `nginx.conf` | Cluster di 4 nodi × 2 dischi dietro un nginx su 9000 e 9001 |
| `compose.cluster-tls.yml` + `nginx-tls.conf` | Lo stesso cluster con TLS (`./certs` montata in `/certs`; certificati di sviluppo con `gen-dev-certs.sh`) |
| `compose.multi-proxy.yml` + `multi-proxy/` | Cluster dietro due proxy (nginx e HAProxy) con domini e origin secret diversi |
| `compose.observability.yml` | Prometheus e Grafana con la dashboard di Nexo, da sovrapporre agli altri file |

Le porte pubblicate si cambiano con `NEXO_S3_PORT` e `NEXO_CONSOLE_PORT`, l'immagine con `NEXO_IMAGE`
(nella shell o in un `.env` accanto ai file). Il README della directory descrive ogni variante.

## Kubernetes (Helm)

Il chart [`deploy/helm/nexo`](https://github.com/R3n3r0/Nexo/tree/main/deploy/helm/nexo) (Kubernetes
≥ 1.27) ha due modalità:

```bash
# standalone: un Deployment, un PVC per disco (1 disco = SNSD, più dischi = SNMD)
helm install nexo oci://ghcr.io/r3n3r0/charts/nexo -n storage --create-namespace

# distributed: StatefulSet di 4 pod × 4 dischi da 100 GiB, un pod per nodo Kubernetes
helm install nexo oci://ghcr.io/r3n3r0/charts/nexo -n storage --create-namespace \
  --set mode=distributed --set distributed.replicas=4 --set distributed.drivesPerNode=4 \
  --set persistence.size=100Gi --set podAntiAffinity=hard

helm test nexo -n storage        # salute del cluster e round trip S3
```

- La password root vuota viene generata all'installazione e conservata negli aggiornamenti (Secret del
  chart, `auth.rootPassword` o `auth.existingSecret`).
- Il chart genera gli endpoint con le ellissi, un argomento per pool (`distributed.pools`), e rifiuta le
  combinazioni che non formano erasure set validi.
- TLS da un Secret `kubernetes.io/tls` (`tls.enabled`, `tls.secretName`), ricaricato a caldo al rinnovo;
  KMS da `kms.secretKey`/`kms.existingSecret` o KES (`kms.kes.*`).
- `serviceMonitor.enabled` crea un ServiceMonitor del Prometheus Operator; tre Ingress opzionali (S3,
  console, distribuzioni).
- Un aggiornamento di immagine procede un pod alla volta; il PodDisruptionBudget (`maxUnavailable: 1`)
  protegge il quorum durante il drain dei nodi. L'aggiunta di un pool, il cambio delle credenziali root o
  di TLS richiedono il riavvio di tutti i pod insieme.

Tutti i valori sono documentati in `values.yaml` e validati da `values.schema.json`.

## systemd

La unit [`deploy/systemd/nexo.service`](https://github.com/R3n3r0/Nexo/blob/main/deploy/systemd/nexo.service)
è la stessa per ogni modalità e ogni nodo: legge l'ambiente da `/etc/default/nexo` e gli endpoint da
`NEXO_ENDPOINTS`. Esegue `nexo server --dry-run` prima di partire, gira come utente `nexo` in una
sandbox (`ProtectSystem=strict`, nessuna capability; `systemd-analyze security` la valuta 1,5 "OK").

Installazione manuale da archivio:

```bash
sudo install -m 0755 nexo nexoctl /usr/local/bin/
sudo useradd --system --home-dir /var/lib/nexo --shell /usr/sbin/nologin nexo
sudo install -d -m 0750 -g nexo /etc/nexo
sudo install -m 0640 -g nexo systemd/nexo.env.example /etc/default/nexo   # poi modificalo
sudo install -m 0644 systemd/nexo.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now nexo
```

Il file system è in sola lettura per il servizio, tranne `/var/lib/nexo` e `/var/cache/nexo`. Dischi in
altri percorsi richiedono un drop-in e il proprietario giusto:

```bash
sudo install -d -o nexo -g nexo -m 0750 /mnt/disk1 /mnt/disk2 /mnt/disk3 /mnt/disk4
sudo systemctl edit nexo
#   [Service]
#   ReadWritePaths=/mnt/disk1 /mnt/disk2 /mnt/disk3 /mnt/disk4
# in /etc/default/nexo:  NEXO_ENDPOINTS=/mnt/disk{1...4}
sudo systemctl restart nexo
```

- `systemctl reload nexo` invia `SIGHUP`: ricarica i certificati TLS senza interrompere le connessioni.
- La unit è `Type=exec`: per attendere che il nodo sia pronto usa `nexo healthcheck --ready`.
- Porte sotto 1024 (`NEXO_WEB_ADDRESS=:80`) richiedono `AmbientCapabilities=CAP_NET_BIND_SERVICE` e
  `CapabilityBoundingSet=CAP_NET_BIND_SERVICE` in un drop-in.

## Docker Swarm

Lo stack [`deploy/swarm/stack.yml`](https://github.com/R3n3r0/Nexo/tree/main/deploy/swarm) avvia un cluster
di 4 nodi × 2 dischi con due nginx, credenziali come Docker secret e aggiornamenti un nodo alla volta:

```bash
docker node update --label-add nexo.node=1 swarm-a      # … fino a nexo.node=4
printf admin | docker secret create nexo_root_user -
openssl rand -base64 30 | tr -d '\n' | docker secret create nexo_root_password -
NEXO_TAG=v1.0.0 docker stack deploy -c stack.yml nexo
```

## Dai sorgenti

You need Go ≥ 1.27.2 (with `GOTOOLCHAIN=auto`, the default, an older toolchain downloads the one in `go.mod` by itself), Node.js 22 with npm, and `make`:

```bash
git clone https://github.com/R3n3r0/Nexo.git && cd Nexo
make build            # console React + bin/nexo (console incorporata) + bin/nexoctl
bin/nexo version
```

`make build-go` compila i due binari senza la console (utile per lo sviluppo del backend: sulla porta 9001
l'Admin API funziona, al posto della console c'è una pagina "console not included"). `make help` elenca
tutti i target.

## Verificare l'installazione

```bash
nexo version
# controlla configurazione, topologia, permessi dei dischi, porte e certificati senza avviare nulla
NEXO_ROOT_USER=admin NEXO_ROOT_PASSWORD=supersegreta nexo server --dry-run /srv/nexo
# configuration OK: mode=snsd s3=:9000 console=:9001 endpoints=/srv/nexo
#   node: local (1 node(s))
#   pool 1: 1 erasure set(s) of 1 drive(s), parity 0
#   local drives: 1 writable, 0 formatted
```

`--dry-run` non scrive sui dischi ed esce con un codice diverso da zero (e la lista dei problemi) se la
configurazione non è valida.
