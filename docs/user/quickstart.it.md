# Quickstart

In cinque minuti: un server Nexo su un disco, la console nel browser, `nexoctl` configurato, un bucket,
un file caricato e un link di condivisione revocabile. Scegli il **binario** o **Docker**: i passi dal 3
in poi sono identici.

## 1. Avvia il server

### Con il binario

Scarica gli archivi `nexo_X.Y.Z_<os>_<arch>.tar.gz` e `nexoctl_X.Y.Z_<os>_<arch>.tar.gz` dalla
[pagina delle release](https://github.com/R3n3r0/Nexo/releases) (oppure compila dai sorgenti con
`make build`, vedi [Installazione](installation.md#dai-sorgenti)), poi:

```bash
tar xzf nexo_*_linux_amd64.tar.gz nexo
tar xzf nexoctl_*_linux_amd64.tar.gz nexoctl
mkdir -p ~/nexo-data

export NEXO_ROOT_USER=admin
export NEXO_ROOT_PASSWORD=supersegreta     # almeno 8 caratteri
./nexo server ~/nexo-data
```

Il percorso del disco deve essere **assoluto** (la shell espande `~`; un percorso relativo come `./data`
è rifiutato). Il server scrive log JSON su stderr; l'ultima riga dell'avvio è:

```json
{"level":"INFO","msg":"nexo started","version":"…","mode":"snsd","s3":"0.0.0.0:9000","console":"0.0.0.0:9001"}
```

Negli esempi seguenti `nexoctl` è nel `PATH` (altrimenti usa `./nexoctl`).

### Con Docker

```bash
docker run -d --name nexo -p 9000:9000 -p 9001:9001 \
  -e NEXO_ROOT_USER=admin -e NEXO_ROOT_PASSWORD=supersegreta \
  -v nexo-data:/data ghcr.io/r3n3r0/nexo:latest server /data

docker ps --filter name=nexo     # STATUS diventa "healthy" in pochi secondi
```

L'immagine contiene anche `nexoctl` (`docker exec nexo /nexoctl …`). Per costruirla dai sorgenti:
`make docker` (immagine locale `ghcr.io/r3n3r0/nexo:latest`).

Le credenziali root si passano **solo** da variabile d'ambiente (o da file con `NEXO_ROOT_USER_FILE` e
`NEXO_ROOT_PASSWORD_FILE`): non esiste un flag, per non esporle nella lista dei processi.

## 2. Controlla che risponda

```bash
curl -s http://localhost:9000/nexo/health/live
# {"ok":true,"status":"ok"}
curl -s http://localhost:9000/nexo/health/ready
# {"ok":true,"status":"ok","checks":{"storage":"ok"}}
```

## 3. Entra nella console

Apri <http://localhost:9001>: nella pagina **Accedi a Nexo** inserisci **Access key** `admin` e
**Secret key** `supersegreta`. La sessione dura 12 ore. Dalla console puoi creare bucket, caricare file
(anche cartelle, con drag & drop), condividerli e gestire utenti e policy.

## 4. Configura nexoctl

Un **alias** associa un nome a un endpoint e a delle credenziali (salvati in `~/.nexoctl/config.json`,
permessi 0600). Le credenziali vengono verificate prima del salvataggio:

```console
$ nexoctl alias set local http://localhost:9000 admin supersegreta
Added alias "local" for http://localhost:9000 (credentials verified).

$ nexoctl admin info local
local  Nexo v1.0.0 (commit d52c0f1, built 2026-10-08T12:00:00Z)
Mode: snsd  Region: us-east-1  Deployment: db114646-23ae-4596-966e-a3ed2ef8248f
Uptime: 8s  Buckets: 0  Objects: 0  Usage: 0B
Pool 1: 1 set(s) of 1 drive(s), parity 0, quorum read 1 write 1
  set 1  ok  1 of 1 drive(s) online
Nodes: 1 online, 0 offline
  local  online  version v1.0.0  uptime 8s  1 drive(s)  (this node)
    /data  online  used 1.2GiB of 50.0GiB (free 48.8GiB)
```

Con Docker usa la CLI del container: `docker exec nexo /nexoctl alias set local http://localhost:9000 admin supersegreta`
e prefissa i comandi seguenti con `docker exec nexo /nexoctl` invece di `nexoctl`.

I comandi di amministrazione (`nexoctl admin …`, i link gestiti) parlano con l'Admin API sulla porta
successiva a quella dell'alias (9000 → 9001); `--admin-url` la imposta esplicitamente.

## 5. Crea un bucket e carica un file

```console
$ echo "ciao Nexo" > hello.txt
$ nexoctl mb local/foto
Bucket created successfully `local/foto`.
$ nexoctl cp hello.txt local/foto/
`hello.txt` -> `local/foto/hello.txt` (10B)
Copied 1 object (10B).
$ nexoctl ls local/foto
[2026-10-15 11:42:10 UTC]     10B STANDARD hello.txt
```

`cp -r` copia cartelle intere, `mirror` sincronizza una directory con un bucket, `cat`, `stat`, `rm`
funzionano come ci si aspetta: l'elenco completo è nella [reference CLI](reference/cli.md).

## 6. Condividi con un link

```console
$ nexoctl share download --expire 2h local/foto/hello.txt
URL: local/foto/hello.txt
Expire: 2h0m0s
Share: http://localhost:9000/foto/hello.txt?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=share-2h5zvraevd7j%2F…
ID: 2h5zvraevd7j (revoke with: nexoctl share revoke local 2h5zvraevd7j)

$ curl -s "http://localhost:9000/foto/hello.txt?X-Amz-Algorithm=…"
ciao Nexo
```

Il link è **gestito**: è firmato da un service account dedicato e si revoca in qualsiasi momento, con
effetto immediato:

```console
$ nexoctl share list local
ID             OWNER            ACCESS   EXPIRES                 TARGET
2h5zvraevd7j   admin            download 2026-10-15 13:42:10 UTC local/foto/hello.txt
$ nexoctl share revoke local 2h5zvraevd7j
Revoked share 2h5zvraevd7j.
```

Dopo la revoca lo stesso URL risponde `403`. Per un URL presigned classico (non revocabile) usa
`nexoctl share download --raw` o `nexoctl presign`; i dettagli sono in
[Condivisione e accesso anonimo](security/sharing.md).

## 7. Arresta il server

`Ctrl-C` (o `docker stop nexo`) ferma il server in modo ordinato: le nuove richieste ricevono `503`, quelle
in corso terminano. I dati restano nella directory (o nel volume) e il prossimo avvio li ritrova.

## Prossimi passi

- un utente non root per le applicazioni: [Utenti, policy e credenziali](security/users-policies.md);
- più dischi o più nodi: [Modalità di deployment](deployment-modes.md);
- TLS: [TLS e certificati](security/tls.md);
- metriche e alert: [Monitoraggio](operations/monitoring.md);
- un SDK S3 qualsiasi funziona con endpoint `http://localhost:9000`, regione `us-east-1` e le
  credenziali dell'utente (path-style; virtual-host con `NEXO_DOMAIN`, vedi
  [Configurazione](configuration.md)).
