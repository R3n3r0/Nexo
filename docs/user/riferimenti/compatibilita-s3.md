# Compatibilità S3

Nexo implementa l'API di Amazon S3 usata dagli SDK AWS, dalla AWS CLI e dagli strumenti più diffusi
(rclone, restic, s3cmd, Terraform, Velero, …). Questa pagina elenca le API supportate, le differenze note e i
risultati della suite di compatibilità.

## Endpoint e autenticazione

| Aspetto | Supporto |
|---------|----------|
| Indirizzamento | path-style (`https://s3.example.com/bucket/chiave`) sempre; virtual-host (`https://bucket.s3.example.com/chiave`) con `NEXO_DOMAIN=s3.example.com` |
| Regione | una sola, `us-east-1` per default (`--region`, `NEXO_REGION`) |
| Firme | AWS Signature V4: header, query (URL presigned, fino a 7 giorni), payload chunked (`aws-chunked`, anche con trailer), POST policy dei form HTML |
| Firme non supportate | Signature V2 (deprecata da AWS) |
| Credenziali temporanee | STS `AssumeRole` (15 minuti – 12 ore) |
| Errori | XML S3 standard, con `x-amz-request-id` e `x-amz-id-2` in ogni risposta; le API non supportate rispondono `501 NotImplemented` |

## API supportate

**Bucket**: `ListBuckets`, `CreateBucket`, `HeadBucket`, `DeleteBucket`, `GetBucketLocation`,
`Get/Put/DeleteBucketPolicy`, `GetBucketPolicyStatus`, `Get/PutBucketVersioning`, `Get/Put/DeleteBucketTagging`,
`Get/Put/DeleteBucketCors`, `Get/Put/DeleteBucketLifecycleConfiguration`, `Get/PutObjectLockConfiguration`,
`Get/Put/DeleteBucketEncryption`, `Get/PutBucketNotificationConfiguration`, `ListObjects`, `ListObjectsV2`,
`ListObjectVersions`, `ListMultipartUploads`, `Get/PutBucketAcl` (solo `private`).

**Oggetti**: `PutObject`, `GetObject` (anche range e richieste condizionali), `HeadObject`, `DeleteObject`,
`DeleteObjects`, `CopyObject`, `GetObjectAttributes`, `Get/Put/DeleteObjectTagging`, `Get/PutObjectRetention`,
`Get/PutObjectLegalHold`, `PostObject`, `Get/PutObjectAcl` (solo `private`).

**Multipart**: `CreateMultipartUpload`, `UploadPart`, `UploadPartCopy`, `CompleteMultipartUpload`,
`AbortMultipartUpload`, `ListParts`.

**STS**: `AssumeRole`.

**Funzionalità**: versioning con delete marker, Object Lock (governance e compliance, retention di default,
legal hold), cifratura SSE-S3, SSE-KMS e SSE-C, checksum aggiuntivi (`CRC32`, `CRC32C`, `CRC64NVME`, `SHA1`,
`SHA256`, anche compositi per il multipart), tag, lifecycle (scadenze, versioni non correnti, delete marker,
upload incompleti), notifiche degli eventi, CORS, quote (estensione di Nexo).

### Non supportate

Rispondono `501 NotImplemented`: website del bucket (`?website`, al suo posto ci sono le
[distribuzioni](../distribuzioni/README.md)), `PublicAccessBlock` (al suo posto il
[block public access](../sicurezza/condivisione.md#regole-di-accesso-pubbliche-e-block-public-access) di
Nexo), replica (`?replication`), server access logging, accelerate, analytics, inventory, metrics,
intelligent tiering, ownership controls, request payment, `RestoreObject`, `SelectObjectContent`,
`GetObjectTorrent`. Le transizioni di lifecycle verso altre classi di storage sono rifiutate. Sono previste
dopo la v1.0: ACL complete, replica tra cluster, tiering verso S3 remoto, `AssumeRoleWithWebIdentity`.

## Differenze note

| Area | Comportamento di Nexo |
|------|-----------------------|
| Chiavi e prefissi | Una chiave non può essere anche il prefisso di altre chiavi (come in MinIO): con `a/b` presente, la scrittura di `a/b/c` è rifiutata con `XNexoParentIsObject` (400); con `a/b/c` presente, quella di `a/b` con `XNexoObjectExistsAsDirectory` (400) |
| ACL | Ogni bucket si comporta come un bucket S3 con Object Ownership `BucketOwnerEnforced` (il default di AWS dal 2023): ACL diverse da `private` e grant espliciti sono rifiutati; l'accesso si concede con policy, accesso anonimo e regole di accesso |
| Account | Un solo account: gli utenti hanno i diritti delle loro policy, non quelli di "proprietario" dei bucket che creano; un bucket esistente risponde sempre `BucketAlreadyOwnedByYou` |
| Autorizzazione | Una richiesta su un oggetto è autorizzata prima di cercare la chiave: con solo `s3:ListBucket` una chiave assente dà 403; con `s3:GetObject` dà 404 anche senza `s3:ListBucket` |
| Metadati non ASCII | I valori `x-amz-meta-*` sono restituiti con i byte ricevuti (UTF-8), senza codifica RFC 2047 |
| CORS | Un bucket **senza** configurazione CORS segue la politica globale `api.cors_allow_origin` (default `*`): la preflight risponde 200 invece del 403 di S3. Un bucket con configurazione CORS si comporta come in S3 |
| Notifiche | Record nel formato S3 con `eventSource` `nexo:s3`; il payload aggiunge `EventName` e `Key` al primo livello (come MinIO); le modifiche di retention e legal hold non generano eventi, come in S3 |
| Download lenti | Un client che smette di leggere il corpo di una GET per più di 20 s (`NEXO_SEND_TIMEOUT`) vede la risposta interrotta |
| Errori specifici | `XNexoQuotaExceeded` e `XNexoStorageFull` (507), `XNexoParentIsObject` e `XNexoObjectExistsAsDirectory` (400); un set senza quorum risponde `503 SlowDownWrite`/`SlowDownRead` (`XNexoLockTimeout` se non ottiene il lock, `ServiceUnavailable` per un disco o nodo offline); gli SDK ritentano i `503`, `SlowDown` resta per le richieste rallentate |
| Block public access | Agisce sulle regole di accesso e sulle distribuzioni, non sulle bucket policy anonime (vedi [Condivisione](../sicurezza/condivisione.md)) |

Per il resto le risposte seguono S3 anche nei dettagli che gli SDK controllano: per esempio nessun
`x-amz-version-id` per la versione `null`, `If-Match` su chiave assente → 404 `NoSuchKey`, `partNumber` oltre
le parti → 416, chiave SSE-C diversa → 403, ripetizione di una `CompleteMultipartUpload` riuscita → stesso
esito, parti in ordine strettamente crescente (`InvalidPartOrder`).

## Risultati di Ceph s3-tests

La suite [`ceph/s3-tests`](https://github.com/ceph/s3-tests) gira ogni notte in tutte e tre le modalità
(`make test-s3compat`, [`test/compat`](https://github.com/R3n3r0/Nexo/tree/main/test/compat)). Ultima
misura pubblicata (commit `5522d1c` della suite, `test_s3.py`, 838 test, 2026-10-09):

| Modalità | Test applicabili superati |
|----------|---------------------------|
| Single node, multi drive | 422/422 |
| Cluster | 422/422 |
| Single node, single drive | 421/422 |

- In SNSD `test_atomic_read_4mb` è fallito in 2 esecuzioni su 4: il client della suite scrive la chiave
  mentre non legge ancora la GET della stessa chiave, e il lock di lettura tenuto durante il trasferimento
  blocca la scrittura finché scade `NEXO_SEND_TIMEOUT`.
- Gruppi principali: versioning 28/28, Object Lock 39/39, multipart 31/31, tagging 17/17, SSE-C 18/18,
  checksum 11/11.
- 415 test sono esclusi, ognuno con il motivo in
  [`test/compat/excluded.txt`](https://github.com/R3n3r0/Nexo/blob/main/test/compat/excluded.txt):
  estensioni di Ceph RGW che AWS stesso non supera (marcate `fails_on_aws` dalla suite), ACL complete,
  Signature V2, `PublicAccessBlock`, server access logging e le differenze note sopra.

Lo stato aggiornato, con le eventuali nuove misure, è in
[docs/05](https://github.com/R3n3r0/Nexo/blob/main/docs/05-roadmap-task.md) (stato della compatibilità S3).

## Provare un client

```bash
aws configure set aws_access_key_id admin
aws configure set aws_secret_access_key supersegreta
aws --endpoint-url http://localhost:9000 s3 mb s3://prova
aws --endpoint-url http://localhost:9000 s3 cp file.txt s3://prova/
aws --endpoint-url http://localhost:9000 s3api list-object-versions --bucket prova
```

Con rclone:

```ini
[nexo]
type = s3
provider = Minio
endpoint = http://localhost:9000
access_key_id = admin
secret_access_key = supersegreta
```

Il provider `Minio` di rclone usa l'indirizzamento path-style, che funziona senza configurare `NEXO_DOMAIN`.
