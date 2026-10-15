# Risoluzione dei problemi

Prima di tutto raccogli lo stato:

```bash
nexoctl admin info local                      # nodi, dischi, set, quorum, sfasamento orologi, healing
curl -s http://nexo1.lan:9000/nexo/health/cluster
nexoctl admin logs --level warn --last 200 local
nexoctl admin trace --errors local            # le richieste che falliscono, in tempo reale
nexo server --dry-run …                       # la configurazione di un nodo che non parte
```

Con `--debug` ogni comando di `nexoctl` stampa su stderr le richieste e le risposte HTTP (segreti redatti).

## Il server non parte

| Messaggio | Causa e rimedio |
|-----------|-----------------|
| `root user is required (NEXO_ROOT_USER, at least 3 characters)` / `root password is required (… at least 8 characters)` / `root password must be at least 8 characters (NEXO_ROOT_PASSWORD has 5)` | Credenziali root mancanti o troppo corte. Con systemd: `NEXO_ROOT_PASSWORD` in `/etc/default/nexo` (vuota dopo l'installazione del pacchetto) |
| `drive path must be absolute: "./data"` | Usa percorsi assoluti (`$PWD/data`, `/srv/nexo`) |
| `either all or none of the arguments must contain ellipsis patterns` | Non mescolare argomenti con `{1...4}` e senza |
| `… is not writable by this user (uid …)` | Proprietario o permessi del disco: `chown nexo:nexo` (systemd) o `chown 65532:65532` (Docker); con systemd anche `ReadWritePaths=` nel drop-in |
| `address and console address must differ` / `address already in use` | Porte: `--address` e `--console-address` diverse e libere |
| `invalid configuration: kms: …` | Formato della master key (`<nome>:<base64 di 32 byte>`) o configurazione KES incompleta |
| `tls internode verify needs TLS` | `--tls-internode-verify` richiede `--certs-dir` ed endpoint `https://` |
| `format mismatch` / `belongs to deployment …` | I dischi appartengono a un altro deployment o a un'altra topologia: non riusare dischi senza cancellarli, usa la stessa riga di comando dell'installazione |

`nexo server --dry-run` riporta tutti questi problemi insieme, senza scrivere sui dischi.

## Un nodo del cluster resta in attesa (bootstrap)

Nei log compare periodicamente:

```text
cluster: waiting for the peers: 2 of 4 nodes verified, 3 needed   offline=[…] different_topology=[…]
cluster: waiting for the drives   missing=[…]
```

- `offline`: il nodo non raggiunge quei peer sulla porta 9000. Verifica la risoluzione dei nomi **da questo
  nodo** (`getent hosts nexo3.lan`), firewall, che il peer sia avviato.
- `different_topology`: quei nodi sono stati avviati con argomenti diversi (ordine dei pool, nomi, porte).
  La riga di comando deve essere identica su tutti i nodi.
- `incompatible_version`: versione major diversa; aggiorna i nodi alla stessa release.
- Se **più della metà** dei nodi non concorda, il nodo esce con `topology mismatch` (o `ErrKMSMismatch` se
  differisce la configurazione KMS) e l'elenco delle differenze.
- `waiting for the drives`: mancano dischi per il quorum di qualche set (dischi non montati, nodi spenti).

Finché il bootstrap non termina il nodo risponde `503` a tutto: è normale durante l'avvio contemporaneo.

## Errori di quorum (`503 SlowDownWrite`, `SlowDownRead`, `XNexoLockTimeout`)

Una richiesta S3 che non raggiunge il quorum risponde `503` con codice `SlowDownWrite` (quorum di
scrittura), `SlowDownRead` (quorum di lettura), `XNexoLockTimeout` (lock non ottenuto entro 30 s:
risorsa bloccata da un'altra operazione o troppi nodi offline) o `ServiceUnavailable`
(un disco o un nodo necessario è offline); `SlowDown` resta solo per le richieste rallentate. Gli SDK
ritentano i `503`; `nexoctl` non ritenta i primi tre (il server ha già atteso) e stampa
`the deployment has no quorum (too many drives or nodes offline): check "nexoctl admin info"` per
`SlowDownWrite`/`SlowDownRead`, e `the object is locked by another operation (a slow or stalled read or write)
or too many nodes are offline` per `XNexoLockTimeout`: con tutti i nodi online è una contesa (per esempio un
download di più di 8 parti il cui client ha smesso di leggere, interrotto dopo `NEXO_SEND_TIMEOUT`). Quando i nodi
raggiungibili sono meno del quorum dei lock la richiesta fallisce subito, senza attendere i 30 s. Nell'Admin
API il `code` è `read_quorum`, `write_quorum`, `lock_timeout`, `node_offline` o `disk_not_found`, che la
console traduce.

1. `nexoctl admin info local`: quali set sono `degraded` o senza quorum, quali dischi o nodi sono offline.
2. Riporta online nodi e dischi (rete, servizio, mount). Le letture tornano appena ci sono D dischi
   coerenti, le scritture con D (o D+1) dischi e con N/2+1 nodi per i lock.
3. Al ritorno la coda MRF ripara gli oggetti scritti nel frattempo; verifica con
   `nexoctl admin heal -r --dry-run local`.

Un set senza quorum non blocca gli altri: gli oggetti degli altri set restano disponibili. I lock rimasti
a un nodo caduto scadono dopo `NEXO_LOCK_STALE_AFTER` (60 s); in casi estremi
`nexoctl admin lock list local` e `nexoctl admin lock clear local RESOURCE` (richiede `admin:ForceUnlock`).

## Orologi sfasati (clock skew)

Sintomi:

- `cluster: clock skew between nodes` nei log, sfasamento indicato in `nexoctl admin info`;
- richieste rifiutate con `RequestTimeTooSkewed` (il client ha più di 15 minuti di differenza dal server);
- URL presigned o share link che risultano scaduti o "non ancora validi".

Rimedio: sincronizza tutti i nodi e i client con NTP (chrony, systemd-timesyncd) e verifica con
`chronyc tracking` o `timedatectl`. La soglia di avviso tra i nodi è 2 secondi. I lock distribuiti non
dipendono dagli orologi; l'ordine delle versioni di un oggetto resta monotono anche con orologi sfasati.

## Dischi lenti o difettosi

- `nexoctl admin trace --min-duration 1s local`: quali operazioni sono lente e su quale nodo.
- Metriche `nexo_drive_operation_seconds_total{drive,op}` / `nexo_drive_operations_total` (latenza media
  per disco) e `nexo_drive_errors_total{drive,type}` (`timeout`, `faulty`, `corrupt`, `full`, …).
- `nexoctl admin speedtest local drive` misura scrittura e lettura dei dischi locali del nodo (fuori
  orario).
- Un disco che continua a dare errori o timeout va sostituito ([Healing e dischi](healing-dischi.md)): con
  la parità sufficiente il set continua a funzionare senza di lui.
- Controlla il file system: XFS, `noatime`, nessun RAID o LVM sotto Nexo, nessun altro processo che usi
  intensamente lo stesso disco; `dmesg` per errori di I/O.

## Permessi e accessi negati

| Sintomo | Verifica |
|---------|----------|
| `AccessDenied` (403) per un utente | `nexoctl admin policy simulate --user alice --action s3:GetObject local/foto/a.jpg -v` mostra la decisione e lo statement determinante |
| La console non mostra un menu o un'azione | la console mostra solo ciò che l'utente può fare: controlla le policy dell'utente e dei suoi gruppi (`nexoctl admin user info local alice`) |
| `alias set` esce con codice 4 | access key sconosciuta, disabilitata o scaduta, o secret sbagliato |
| "listing the buckets is denied" su `alias set` | credenziali valide ma senza `s3:ListAllMyBuckets`: normale per utenti limitati a un bucket |
| Login della console: "Troppi tentativi falliti" | attendi un minuto: i tentativi falliti sono limitati |
| Root rifiutato sull'API S3 | `api.root_access` è `off`: `nexoctl admin config set local api root_access=on` (la console continua a funzionare) |
| `SignatureDoesNotMatch` dietro un proxy | il proxy modifica l'header `Host` (o la porta): deve inoltrarlo invariato (`proxy_set_header Host $http_host`) |
| Anonimo rifiutato nonostante una regola pubblica | block public access attivo sul bucket o sul cluster (`nexoctl access block-public status local/bucket`) |

## Spazio, quote e oggetti

| Codice | Significato |
|--------|-------------|
| `XNexoQuotaExceeded` (507) | quota del bucket superata (`nexoctl quota info local/bucket`) |
| `XNexoStorageFull` (507) | dischi pieni: libera spazio o aggiungi un pool |
| `XNexoParentIsObject` (400) | una chiave non può essere anche il prefisso di altre: esiste `a/b` e si scrive `a/b/c` |
| `XNexoObjectExistsAsDirectory` (400) | il caso opposto: esiste `a/b/c` e si scrive `a/b` |
| `XNexoObjectLocked` | versione protetta da retention o legal hold |

## TLS

- Un client rifiuta il certificato: verifica la catena e i nomi
  (`openssl s_client -connect nexo.lan:9000 -servername nexo.lan`), che il certificato copra il nome usato
  dal client e, in cluster, i nomi degli endpoint.
- Certificato rinnovato ma non servito: la directory è osservata e ricaricata entro pochi secondi (o
  `systemctl reload nexo`); una coppia chiave/certificato non coerente viene scartata e restano i
  certificati precedenti (log e metrica `nexo_tls_server_certificate_reloads_total{result}`).
- Container `unhealthy` con TLS: l'healthcheck usa `NEXO_CERTS_DIR` per passare a HTTPS; verifica che la
  variabile sia impostata nel container.

## Notifiche ed eventi

- `nexoctl admin notify targets list local`: stato online/offline e coda di ogni target.
- `nexoctl admin notify targets test local arn:nexo:sqs:us-east-1:_:webhook`: invia un evento di prova.
- I webhook non seguono i redirect (un `3xx` è una consegna fallita); gli eventi restano in coda
  (`queue_limit`) finché il target non torna.

## Raccogliere informazioni per una segnalazione

```bash
nexo version
nexoctl --json admin info local > info.jsonl
nexoctl admin logs --last 1000 --timeout 5s --json local > logs.jsonl
nexoctl admin profile --type goroutine local          # se un nodo sembra bloccato
```

I log e i trace non contengono segreti, ma contengono nomi di bucket, chiavi e indirizzi IP: rivedili prima
di condividerli.
