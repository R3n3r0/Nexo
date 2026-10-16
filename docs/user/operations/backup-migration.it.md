# Backup e migrazione

Un deployment Nexo ha due tipi di contenuto da proteggere:

- lo **stato** del deployment: utenti, gruppi, access key, policy, configurazione dinamica e metadati dei
  bucket (versioning, policy, lifecycle, notifiche, CORS, tag, quota, object lock, cifratura);
- i **dati**: gli oggetti, con tutte le loro versioni.

`nexoctl admin cluster export/import` gestisce lo stato, `nexoctl mirror` i dati. Insieme permettono di
fare backup e di migrare tra modalità (SNSD → SNMD → cluster) o tra cluster.

## Esportare lo stato

```console
$ nexoctl admin cluster export local --output backup.zip
Exported the state of "local" to backup.zip (2.3KiB).
```

L'archivio (zip, scritto con permessi 0600) contiene:

| File | Contenuto |
|------|-----------|
| `manifest.json` | formato, versione del server, elenco di utenti, gruppi, policy, sottosistemi e bucket |
| `iam.bin` | utenti, gruppi, access key e sessioni STS, **cifrati** (AES-256-GCM con chiave derivata con Argon2id dalle credenziali root) |
| `policies.json` | policy con nome |
| `config.json` | configurazione dinamica |
| `buckets/<nome>.json` | metadati di ogni bucket |

Il deployment che importa `iam.bin` deve avere **le stesse credenziali root** di quello che l'ha esportato.
L'utente root non è nell'archivio (viene dall'ambiente). Richiede `admin:ExportCluster`.

Per un backup periodico basta schedulare l'export (cron, timer systemd) e conservare gli archivi fuori dal
deployment.

## Importare lo stato

```console
$ nexoctl admin cluster import nuovo backup.zip --dry-run
Would import into "nuovo" (merge): 1 user(s), 0 group(s), 0 polic(ies), 0 configuration subsystem(s), 2 bucket(s).
Buckets created: docs, foto

$ nexoctl admin cluster import nuovo backup.zip
Imported into "nuovo" (merge): 1 user(s), 0 group(s), 0 polic(ies), 0 configuration subsystem(s), 2 bucket(s).
Buckets created: docs, foto
```

- L'archivio viene validato per intero prima di applicare qualsiasi cosa; `--dry-run` si ferma lì e mostra
  cosa cambierebbe.
- `--mode merge` (default) aggiunge e aggiorna, mantenendo ciò che l'archivio non contiene.
- `--mode replace` sostituisce anche l'intera configurazione e rimuove utenti, gruppi e policy con nome
  assenti dall'archivio. I bucket non vengono mai rimossi.
- I bucket mancanti vengono creati con le loro impostazioni (versioning compreso). Richiede
  `admin:ImportCluster`.

## Copiare i dati

`nexoctl mirror` sincronizza due posizioni qualsiasi (directory locali, bucket dello stesso o di un altro
alias, anche di un altro server S3):

```bash
nexoctl mirror local/foto ./backup/foto                # backup su disco: copia ciò che manca o è cambiato
nexoctl mirror --overwrite --remove local/foto ./backup/foto   # copia esatta (cancella ciò che non c'è più)
nexoctl mirror --preserve vecchio/docs nuovo/docs      # tutte le versioni e i delete marker, con metadati e tag
```

- Una seconda esecuzione trasferisce solo le differenze.
- `--preserve` copia, tra due bucket, **ogni versione** e ogni delete marker dal più vecchio al più recente,
  con content type, metadati e tag. Il bucket di destinazione deve avere il versioning abilitato se ci sono
  più versioni (l'import dello stato lo crea già così). Gli ID di versione nel target sono nuovi; ordine e
  contenuto sono conservati.
- `--dry-run` mostra cosa farebbe; `--limit-upload`/`--limit-download` limitano la banda.

## Migrare a un'altra modalità

Esempio: da un server SNSD (alias `vecchio`) a un cluster nuovo (alias `nuovo`), con le **stesse
credenziali root**.

1. Installa e avvia il nuovo deployment ([Modalità](../deployment-modes.md)) e crea gli alias:

   ```bash
   nexoctl alias set vecchio http://old.lan:9000 admin "$ROOT_PASSWORD"
   nexoctl alias set nuovo   http://nexo.lan:9000 admin "$ROOT_PASSWORD"
   ```

2. Esporta e importa lo stato **prima** dei dati, così i bucket esistono già con il loro versioning:

   ```bash
   nexoctl admin cluster export vecchio --output stato.zip
   nexoctl admin cluster import nuovo stato.zip --dry-run
   nexoctl admin cluster import nuovo stato.zip
   ```

3. Copia i dati bucket per bucket con tutte le versioni:

   ```bash
   for b in $(nexoctl ls --json vecchio | jq -r '.name | rtrimstr("/")'); do
     nexoctl mirror --preserve "vecchio/$b" "nuovo/$b"
   done
   ```

4. Ferma le scritture sul vecchio deployment (o mettilo in sola lettura togliendo i permessi di scrittura
   alle applicazioni), ripeti il passo 3 per copiare le ultime modifiche, poi sposta i client sul nuovo
   endpoint (DNS o load balancer).
5. Verifica: `nexoctl diff vecchio/docs nuovo/docs` non stampa nulla se i due bucket coincidono (altrimenti elenca gli oggetti presenti solo da una parte o diversi); `nexoctl admin info nuovo`.

Questa procedura è verificata in CI (test TM-04: oggetti, versioni, IAM, policy e lifecycle identici dopo la
migrazione da SNSD a cluster).

## Cosa non è nell'archivio

- i dati degli oggetti (copiali con `mirror`);
- le chiavi del KMS: la master key del KMS integrato (`NEXO_KMS_SECRET_KEY`) va conservata a parte, perché
  senza di essa gli oggetti cifrati con SSE-S3 o SSE-KMS non sono leggibili; con `mirror` i dati vengono
  letti in chiaro e riscritti secondo la cifratura di default del bucket di destinazione;
- distribuzioni, key group, certificati e cache: si esportano con `nexoctl dist export`/`import` e
  `nexoctl cache policy export`/`import` (vedi [Distribuzioni](../distributions/README.md));
- le regole di accesso dei bucket: `nexoctl access export`/`import` per bucket.
