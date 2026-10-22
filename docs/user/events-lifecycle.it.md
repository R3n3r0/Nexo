# Eventi e lifecycle

## Notifiche degli eventi

Nexo invia una notifica, nel formato degli eventi S3, quando gli oggetti di un bucket vengono creati, letti
o eliminati. Le notifiche vanno a dei **target** configurati sul server (webhook, Kafka, NATS, AMQP, Redis,
PostgreSQL) e ogni bucket sceglie quali eventi mandare a quale target.

### 1. Configurare un target

Un target si configura con il sottosistema `notify_<tipo>` (o `notify_<tipo>:<id>` per averne più di uno
dello stesso tipo); la configurazione vale subito su tutti i nodi.

**Webhook**: una `POST` con il JSON dell'evento, con `Authorization: Bearer <auth_token>` se impostato.

```console
$ nexoctl admin config set local notify_webhook:app enable=on endpoint=https://app.example.com/eventi auth_token=s3cr3t
Updated notify_webhook:app: auth_token, enable, endpoint.
```

Il webhook deve rispondere `2xx`; per un endpoint HTTPS con una CA privata aggiungi la CA al trust store del
sistema, per il TLS mutuo `client_cert` e `client_key`.

**Kafka**: un messaggio per evento sul topic, con chiave `<bucket>/<oggetto>`.

```bash
nexoctl admin config set local notify_kafka:audit enable=on \
  brokers=kafka1:9092,kafka2:9092 topic=nexo-eventi \
  tls=on sasl=on sasl_mechanism=sha512 sasl_username=nexo sasl_password='…'
```

`sasl_mechanism` è `plain` (default), `sha256` o `sha512`.

**Altri tipi**: `notify_nats` (`address`, `subject`), `notify_amqp` (`url`, `exchange`, `routing_key`),
`notify_redis` (`address`, `key`), `notify_postgres` (`connection_string`, `table`). Le chiavi di ogni tipo
sono elencate con `nexoctl admin config get local notify_nats` e nella pagina **Configurazione** della console.

Chiavi comuni a tutti i target:

| Chiave | Significato |
|--------|-------------|
| `enable` | `on`/`off` |
| `queue_dir` | sottodirectory della coda persistente (relativa alla radice delle code sul primo disco locale) |
| `queue_limit` | eventi in coda al massimo (default 100000) |
| `comment` | testo libero |

Ogni nodo mette in coda su disco gli eventi delle richieste che riceve e li consegna ritentando
finché il target non li accetta: un target irraggiungibile non blocca le scritture e non perde eventi finché
la coda non è piena. Le credenziali (`auth_token`, `sasl_password`, …) sono mascherate in `admin config get`
e nei log.

### 2. Verificare il target

```console
$ nexoctl admin notify targets list local
ARN                                      TYPE     STATE    QUEUED  ENDPOINT
arn:nexo:sqs:us-east-1:app:webhook       webhook  online   0       https://app.example.com/eventi
$ nexoctl admin notify targets test local arn:nexo:sqs:us-east-1:app:webhook
Test event delivered to app:webhook.
```

L'**ARN** di un target è `arn:nexo:sqs:<regione>:<id>:<tipo>`, con id `_` per un sottosistema senza id
(`notify_webhook` → `arn:nexo:sqs:us-east-1:_:webhook`). `QUEUED` mostra gli eventi in attesa sul nodo che
risponde: un numero che cresce indica un target che non accetta le consegne.

### 3. Iscrivere un bucket

```console
$ nexoctl event add local/foto arn:nexo:sqs:us-east-1:app:webhook --event put --suffix .jpg
Subscribed `arn:nexo:sqs:us-east-1:app:webhook` to the s3:ObjectCreated:* events of `local/foto`.
$ nexoctl event add local/foto arn:nexo:sqs:us-east-1:app:webhook --event delete --prefix 2026/
Subscribed `arn:nexo:sqs:us-east-1:app:webhook` to the s3:ObjectRemoved:* events of `local/foto`.
$ nexoctl event list local/foto
arn:nexo:sqs:us-east-1:app:webhook   s3:ObjectCreated:*   Filter: suffix=".jpg"   ID: nexo-a88d586aef2bdffa
arn:nexo:sqs:us-east-1:app:webhook   s3:ObjectRemoved:*   Filter: prefix="2026/"   ID: nexo-336c095ebad866cb
$ nexoctl event rm local/foto arn:nexo:sqs:us-east-1:app:webhook
```

`--event` accetta `put`, `delete`, `get`, `replica`, `ilm` o i tipi S3 (`s3:ObjectCreated:Put`,
`s3:ObjectRemoved:DeleteMarkerCreated`, …), separati da virgola. La stessa configurazione si imposta con
`PutBucketNotificationConfiguration` da qualsiasi SDK S3 (`QueueConfiguration` con l'ARN del target) o dalla
scheda **Eventi** del bucket in console.

### Il messaggio

Il corpo è il formato degli eventi S3 (`eventVersion` 2.1), con in più `EventName` e `Key` al primo livello:

```json
{
  "EventName": "s3:ObjectCreated:Put",
  "Key": "foto/2026/gita.jpg",
  "Records": [
    {
      "eventVersion": "2.1",
      "eventSource": "nexo:s3",
      "awsRegion": "us-east-1",
      "eventTime": "2026-10-15T12:14:35.656Z",
      "eventName": "ObjectCreated:Put",
      "userIdentity": { "principalId": "admin" },
      "requestParameters": { "sourceIPAddress": "127.0.0.1" },
      "responseElements": { "x-amz-request-id": "…" },
      "s3": {
        "s3SchemaVersion": "1.0",
        "configurationId": "nexo-a88d586aef2bdffa",
        "bucket": { "name": "foto", "arn": "arn:aws:s3:::foto" },
        "object": { "key": "2026/gita.jpg", "size": 3, "eTag": "…", "sequencer": "…" }
      }
    }
  ]
}
```

La chiave dentro `Records` è codificata come in S3 (`+` per gli spazi, `%XX` per i caratteri speciali, `/` invariato). La consegna è **almeno una volta**: un
consumatore deve tollerare i duplicati (per esempio usando `sequencer` e l'ETag).

### Esempio: miniature automatiche

1. Un servizio HTTP riceve gli eventi `s3:ObjectCreated:*` con suffisso `.jpg` del bucket `foto`.
2. Per ogni evento legge l'oggetto con un utente che ha solo `s3:GetObject` su `foto` e scrive la miniatura in
   `miniature/` con `s3:PutObject` su quel bucket.
3. Il bucket `miniature` **non** ha notifiche verso lo stesso servizio, così non si creano cicli.

## Lifecycle (ILM)

Le regole di lifecycle eliminano automaticamente gli oggetti scaduti, le versioni vecchie, i delete marker
rimasti soli e gli upload multipart abbandonati. Il server le applica in background (lo scanner percorre i
bucket a ogni ciclo, `nexoctl admin config set local scanner cycle=1m`); `nexoctl ilm apply` le applica
subito. Le versioni protette da Object Lock non vengono mai eliminate.

### Regole comuni

```console
$ nexoctl ilm rule add --prefix logs/ --expire-days 30 local/app
Lifecycle rule "rule-1" added to `local/app`.
$ nexoctl ilm rule add --abort-mpu-days 2 --expired-delete-marker local/app
$ nexoctl ilm rule add --noncurrent-expire-days 7 --newer-noncurrent 3 --id versioni local/app
$ nexoctl ilm rule add --prefix tmp/ --size-gt 100MiB --expire-days 1 --id grandi local/app
$ nexoctl ilm rule add --tags 'stato=bozza' --expire-days 14 --id bozze local/app
$ nexoctl ilm rule list local/app
ID               STATUS   FILTER                         ACTIONS
rule-1           Enabled  prefix=logs/                   expire after 30d
rule-2           Enabled  -                              remove expired delete markers, abort uploads after 2d
versioni         Enabled  -                              noncurrent versions expire after 7d (keep 3 newest)
grandi           Enabled  prefix=tmp/ size>100.0MiB      expire after 1d
bozze            Enabled  tags=stato=bozza               expire after 14d
```

| Obiettivo | Regola |
|-----------|--------|
| Log conservati 30 giorni | `--prefix logs/ --expire-days 30` |
| Pulizia degli upload multipart interrotti | `--abort-mpu-days 2` (consigliata su ogni bucket con upload grandi) |
| Bucket con versioning: tenere 3 versioni precedenti, al massimo per 7 giorni | `--noncurrent-expire-days 7 --newer-noncurrent 3` |
| Eliminare i delete marker rimasti senza versioni | `--expired-delete-marker` |
| Scadenza a una data | `--expire-date 2027-01-01` |
| Solo oggetti grandi o piccoli | `--size-gt 100MiB`, `--size-lt 1KiB` |
| Solo oggetti con un tag | `--tags 'stato=bozza'` (più tag: `'a=1&b=2'`) |

Con il **versioning** attivo (`nexoctl version enable local/app`), la scadenza di una versione corrente crea
un delete marker e la versione diventa non corrente: abbina sempre una regola `--noncurrent-expire-days`,
altrimenti lo spazio non viene liberato.

### Modificare, applicare, esportare

```console
$ nexoctl ilm rule edit --id rule-1 --expire-days 90 local/app
$ nexoctl ilm rule edit --id bozze --disable local/app
$ nexoctl ilm rule remove --id grandi local/app       # --all rimuove tutte le regole
$ nexoctl ilm apply local/app
Lifecycle of `local/app` applied: 2 versions checked, 0 deleted, 0 delete markers created, 0 uploads aborted.
$ nexoctl ilm rule export local/app > lifecycle.json
$ nexoctl ilm rule export --xml local/app > lifecycle.xml
$ nexoctl ilm rule import local/app-copia lifecycle.json
```

`ilm rule import` accetta il JSON dell'export e della console o l'XML `LifecycleConfiguration` di S3; un
documento senza regole rimuove la configurazione. Le stesse regole si gestiscono con
`PutBucketLifecycleConfiguration` da qualsiasi SDK e dalla scheda **Lifecycle** del bucket in console.
`ilm apply` richiede `s3:PutLifecycleConfiguration` e `admin:ApplyLifecycle`.

Le azioni `Transition` e `NoncurrentVersionTransition` spostano i dati delle versioni vecchie su un tier remoto:
vedi [Tier remoti](tiering.md). Una transizione che non indica alcun tier viene rifiutata (`InvalidStorageClass`).

### Eventi del lifecycle

Le eliminazioni eseguite dal lifecycle generano gli eventi `s3:LifecycleExpiration:Delete` e
`s3:LifecycleExpiration:DeleteMarkerCreated` (non `s3:ObjectRemoved:*`): iscrivi un target con `--event ilm`
per riceverle, per esempio per tenere un registro di ciò che è stato rimosso.
