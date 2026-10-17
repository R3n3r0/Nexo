# Migrare da MinIO

Nexo parla la stessa API S3 di MinIO: applicazioni, SDK e strumenti come `rclone`, `restic` o `aws s3`
continuano a funzionare una volta puntati al nuovo endpoint. Questa guida copia un deployment MinIO in un
nuovo deployment Nexo: i bucket con le loro impostazioni, ogni versione degli oggetti, le policy, gli utenti e le
access key. Il deployment MinIO resta intatto finché non sposti i client.

Ogni comando qui sotto è stato eseguito contro un server MinIO con bucket con versioning, delete marker, tag,
metadati utente, regole di lifecycle, una bucket policy anonima, una policy con nome e un utente, e il
risultato è stato verificato con `nexoctl diff`.

## Prima di iniziare

| MinIO | Nexo |
|-------|------|
| `minio server /data{1...4}` | `nexo server /data{1...4}` (stessa sintassi con i puntini, stesso layout con erasure coding su più dischi) |
| `MINIO_ROOT_USER` / `MINIO_ROOT_PASSWORD` | `NEXO_ROOT_USER` / `NEXO_ROOT_PASSWORD` |
| API S3 su `:9000`, console su `:9001` | le stesse porte predefinite |
| `/minio/health/live`, `/minio/health/ready` | `/nexo/health/live`, `/nexo/health/ready` (aggiorna probe e load balancer) |
| `mc` per i dati, `mc admin` per l'amministrazione | `nexoctl` per entrambi; `mc` continua a funzionare per i comandi S3, `mc admin` no |
| `MINIO_KMS_*` / KES | KMS integrato (`NEXO_KMS_SECRET_KEY`) o KES (`NEXO_KMS_ENDPOINT`, ...): vedi [KMS e SSE](security/kms-sse.md) |

Il formato su disco **non** è compatibile: Nexo non può partire sui dischi di MinIO. La migrazione copia i dati
attraverso l'API S3, quindi serve spazio per il nuovo deployment accanto al vecchio (altri dischi o altre
macchine).

1. Installa Nexo e avvialo su dischi nuovi ([Installazione](installation.md),
   [Modalità di deployment](deployment-modes.md)).
2. Installa `nexoctl` su una macchina che raggiunge entrambi i deployment, e `jq` per i cicli qui sotto.
3. Crea un alias per ciascun lato. Verso MinIO `nexoctl` parla S3, quindi va bene qualsiasi access key MinIO
   con accesso in lettura a tutti i bucket; dal lato Nexo usa l'utente root o un amministratore.

   ```bash
   nexoctl alias set minio https://minio.example.com:9000 "$MINIO_ACCESS_KEY" "$MINIO_SECRET_KEY"
   nexoctl alias set nexo https://nexo.example.com:9000 admin "$NEXO_ROOT_PASSWORD"
   ```

   I comandi di amministrazione (`nexoctl admin ...`) si usano solo sull'alias `nexo`.

## 1. Policy

Le policy IAM con nome usano la stessa grammatica JSON (`Version`, `Statement`, `Effect`, `Action`,
`Resource`, `Condition`). Esportale con `mc` e creale su Nexo; quelle predefinite (`readonly`, `readwrite`,
`writeonly`, `diagnostics`, `consoleAdmin`) esistono già in Nexo.

```bash
for p in $(mc admin policy ls minio --json | jq -r '.policy' \
           | grep -vE '^(consoleAdmin|diagnostics|readonly|readwrite|writeonly)$'); do
  mc admin policy info minio "$p" --json | jq '.policyInfo.Policy' > "policy-$p.json"
  nexoctl admin policy create --update nexo "$p" "policy-$p.json"
done
```

Un documento che usa un'azione o una condition key che Nexo non conosce viene rifiutato con la riga e la
colonna dell'errore: correggilo e riesegui il comando. `nexoctl admin policy simulate` mostra cosa permette una
policy prima di assegnarla agli utenti.

## 2. Utenti, gruppi e access key

MinIO non restituisce mai le secret key, quindi gli utenti non si possono copiare così come sono. Ricreali con
la secret che scegli (o quella che l'applicazione ha già nella sua configurazione) e assegna le stesse policy:

```bash
mc admin user ls minio --json | jq -r '[.accessKey, (.policyName // "")] | @tsv'

nexoctl admin user add nexo alice "$ALICE_SECRET"
nexoctl admin policy attach nexo photos-ro --user alice
```

Gruppi: `nexoctl admin group add nexo GRUPPO MEMBRO...` e `nexoctl admin policy attach nexo POLICY --group
GRUPPO`.

I service account (access key di un utente) possono mantenere **la stessa coppia di chiavi**, così le
applicazioni non hanno bisogno di nuove credenziali:

```bash
nexoctl admin accesskey create nexo --user alice --name backup-job \
  --access-key "$OLD_ACCESS_KEY" --secret-key "$OLD_SECRET_KEY"
```

Una policy inline del service account MinIO va in `--policy FILE`.

## 3. Bucket e impostazioni

Crea ogni bucket con lo stesso versioning (e Object Lock) che ha su MinIO, poi copia le regole di lifecycle e
la bucket policy. `nexoctl` le legge da MinIO attraverso l'API S3.

```bash
for b in $(nexoctl ls --json minio | jq -r '.name | rtrimstr("/")'); do
  if ! nexoctl stat "nexo/$b" >/dev/null 2>&1; then
    if nexoctl version info "minio/$b" | grep -qi enabled; then
      nexoctl mb --with-versioning "nexo/$b"
    else
      nexoctl mb "nexo/$b"
    fi
  fi
  nexoctl ilm rule export "minio/$b" > "ilm-$b.json" 2>/dev/null \
    && nexoctl ilm rule import "nexo/$b" "ilm-$b.json"
  nexoctl anonymous get-json "minio/$b" > "anon-$b.json" 2>/dev/null \
    && nexoctl anonymous set-json "anon-$b.json" "nexo/$b"
done
```

- Bucket con **Object Lock**: creali invece con `nexoctl mb --with-lock nexo/BUCKET` (un bucket non può
  passare a Object Lock in seguito) e imposta la stessa retention predefinita con `nexoctl retention set
  --default --mode governance --validity 30d nexo/BUCKET`.
- **Quota**: `nexoctl quota set nexo/BUCKET --size 500GiB`.
- **Cifratura predefinita**: `nexoctl encrypt set sse-s3 nexo/BUCKET` (o `sse-kms CHIAVE`).
- **Notifiche**: i target sono sottosistemi di configurazione `notify_*` come in MinIO (`notify_webhook`,
  `notify_kafka`, ...), impostati con `nexoctl admin config set`; poi aggiungi le regole del bucket con
  `nexoctl event add`. Vedi [Eventi e lifecycle](events-lifecycle.md) per le chiavi di ogni target.
- **CORS**: `nexoctl cors set nexo/BUCKET cors.json` (JSON o XML S3).

## 4. Dati, con tutte le versioni

```bash
for b in $(nexoctl ls --json minio | jq -r '.name | rtrimstr("/")'); do
  nexoctl mirror --preserve "minio/$b" "nexo/$b"
done
```

`--preserve` copia ogni versione e ogni delete marker, dalla più vecchia, con content type, metadati utente e
tag. I version ID su Nexo sono nuovi; ordine, contenuto e versione più recente sono gli stessi.

- È incrementale: rieseguirlo copia solo ciò che è cambiato su MinIO dall'ultima esecuzione.
- `--limit-download 200MiB` / `--limit-upload 200MiB` limitano la banda se MinIO serve traffico di
  produzione; `--dry-run` mostra cosa verrebbe copiato.
- Gli oggetti cifrati su MinIO con SSE-S3 o SSE-KMS vengono letti in chiaro da `nexoctl` (li decifra MinIO) e
  scritti secondo la cifratura predefinita del bucket Nexo. Gli oggetti con **SSE-C** richiedono le loro
  chiavi: `--enc-c-file keys.txt` con una riga `minio/BUCKET=CHIAVEBASE64` e una `nexo/BUCKET=CHIAVEBASE64`.
- **Retention e legal hold** dei singoli oggetti non vengono copiati da `mirror`: riapplicali con
  `nexoctl retention set --mode governance --validity 30d -r nexo/BUCKET/PREFISSO/` e
  `nexoctl legalhold set nexo/BUCKET/OGGETTO`.

Funziona anche `mc mirror` (`mc alias set nexo ...` con l'endpoint di Nexo), ma copia solo l'ultima versione e
non crea i bucket mancanti: preferisci `nexoctl mirror --preserve`.

## 5. Verifica e passaggio

1. Confronta ogni bucket. `nexoctl diff` non stampa nulla quando le ultime versioni coincidono:

   ```bash
   for b in $(nexoctl ls --json minio | jq -r '.name | rtrimstr("/")'); do
     nexoctl diff "minio/$b" "nexo/$b"
   done
   nexoctl ls --versions nexo/BUCKET/PREFISSO/    # controlla a campione la storia di qualche chiave
   ```

2. Ferma le scritture su MinIO (ferma chi scrive, o assegna una policy di sola lettura ai suoi utenti),
   esegui ancora una volta il passo 4 per copiare le ultime modifiche e ripeti il diff.
3. Punta i client a Nexo: cambia il nome DNS o il backend del load balancer, o l'endpoint nella configurazione
   dell'applicazione. Le access key ricreate con la stessa coppia continuano a funzionare; gli URL presigned
   firmati per il vecchio host no.
4. Tieni MinIO in sola lettura per un po', poi dismettilo.

## Cos'altro guardare

- **Console e allarmi**: la console web gira sulla porta della console (`:9001`); le metriche Prometheus, la
  dashboard Grafana e gli allarmi sono descritti in [Monitoraggio](operations/monitoring.md).
- **Identity provider**: gli utenti di un provider OpenID Connect o LDAP esterno configurato in MinIO non
  vengono migrati; Nexo usa i propri utenti, gruppi e STS IAM (`AssumeRole`).
- **Site replication, bucket replication, tiering, batch job**: Nexo non ha un equivalente per queste
  funzionalità di MinIO; tienine conto prima di migrare. La pagina
  [Compatibilità S3](reference/s3-compatibility.md) elenca cosa supporta Nexo.
- **Dopo**: lo stato di Nexo stesso (utenti, policy, bucket) si può salvare e spostare con
  `nexoctl admin cluster export/import`; vedi [Backup e migrazione](operations/backup-migration.md).
