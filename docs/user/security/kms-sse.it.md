# KMS e cifratura lato server (SSE)

Nexo cifra gli oggetti sui dischi con tre modalità compatibili con S3:

| Modalità | Chiave | Richiede |
|----------|--------|----------|
| **SSE-S3** | chiave dati per oggetto, sigillata dalla chiave di default del KMS | un KMS |
| **SSE-KMS** | chiave dati sigillata da una chiave **nominata** del KMS | un KMS |
| **SSE-C** | chiave fornita dal client a ogni richiesta, mai salvata | HTTPS |

I dati sono cifrati a blocchi (formato DARE, AES-256-GCM), le range GET funzionano; i dischi non contengono
mai il contenuto in chiaro. Con SSE-KMS e SSE-C anche l'ETag è opaco; con SSE-S3, come in S3, l'ETag resta
l'MD5 del contenuto in chiaro (per dati che non devono rivelare alcun digest usa SSE-KMS o SSE-C).

## Configurare il KMS

### KMS integrato

Una master key da 32 byte, con un nome che diventa la chiave di default di SSE-S3:

```bash
sudo sh -c 'umask 027 && echo "nexo-key:$(openssl rand -base64 32)" > /etc/nexo/kms-key'
sudo chgrp nexo /etc/nexo/kms-key
# /etc/default/nexo (uguale su tutti i nodi)
NEXO_KMS_SECRET_KEY_FILE=/etc/nexo/kms-key
```

```console
$ nexoctl admin kms status local
KMS: builtin (online)
Default key: nexo-key
$ nexoctl admin kms key create local app-key
Created the KMS key "app-key".
$ nexoctl admin kms key list local
KEY                              CREATED                 DEFAULT
app-key                          2026-10-15 12:01:09 UTC
nexo-key                         -                       yes
```

Le chiavi create con `kms key create` derivano dalla master key: basta conservare quella. **Senza la master
key gli oggetti cifrati sono irrecuperabili**: tienine una copia sicura fuori dal deployment.

### KMS esterno (KES)

```bash
NEXO_KMS_ENDPOINT=https://kes1.example.net:7373,https://kes2.example.net:7373
NEXO_KMS_KEY_NAME=nexo-default
NEXO_KMS_CLIENT_CERT=/etc/nexo/kes/client.crt
NEXO_KMS_CLIENT_KEY=/etc/nexo/kes/client.key
NEXO_KMS_CAPATH=/etc/nexo/kes/ca.crt
```

Solo endpoint `https://` (le chiavi dati viaggiano nelle risposte di KES), autenticazione mTLS, failover
nell'ordine indicato, nessun redirect seguito. Si configura uno solo dei due KMS.

### In cluster

Tutti i nodi devono avere la stessa configurazione KMS: all'avvio i nodi confrontano un'impronta (mai i
segreti) e un nodo diverso dalla maggioranza non parte (`ErrKMSMismatch`). Lo stato è in
`nexoctl admin kms status`, nella pagina **Info** della console e nella metrica `nexo_kms_online`
(alert `NexoKMSOffline`).

## Cifratura di default di un bucket

```console
$ nexoctl encrypt set sse-s3 local/docs
Default encryption of `local/docs`: SSE-S3 (AES256).
$ nexoctl encrypt set sse-kms app-key local/segreti
$ nexoctl encrypt info local/docs
`local/docs`: SSE-S3 (AES256).
$ nexoctl encrypt clear local/docs
```

Ogni oggetto scritto dopo, senza header di cifratura propri, viene cifrato. Gli oggetti esistenti non
cambiano.

## Cifrare per richiesta

Con `nexoctl` (o con gli header S3 `x-amz-server-side-encryption*` da qualsiasi SDK):

```bash
nexoctl cp --enc-s3 local/docs report.pdf local/docs/
nexoctl cp --enc-kms "local/segreti=app-key" report.pdf local/segreti/
nexoctl stat local/segreti/report.pdf          # Encryption : SSE-KMS (app-key)
```

### SSE-C

La chiave (32 byte in base64) è fornita dal client a ogni lettura e scrittura; il server non la conserva.
S3 richiede HTTPS per SSE-C e Nexo rifiuta le chiavi su HTTP in chiaro (`400 InvalidRequest`), salvo
`NEXO_SSEC_ALLOW_INSECURE=on`, **solo per test**.

```bash
KEY=$(openssl rand -base64 32)
export NEXOCTL_ENC_C="local/privato=$KEY"        # meglio della riga di comando
nexoctl cp report.pdf local/privato/
nexoctl cat local/privato/report.pdf > copia.pdf
```

Oltre a `NEXOCTL_ENC_C` (più voci separate da virgola) si può usare `--enc-c-file chiavi.txt`, un file con
una voce `ALIAS/BUCKET[/PREFIX]=BASE64KEY` per riga e permessi 0600. Evita `--enc-c` sulla riga di comando:
la chiave sarebbe visibile agli altri utenti locali e resterebbe nella history. **Perdere la chiave SSE-C
significa perdere l'oggetto.**

## Rotazione della master key (KMS integrato)

1. Riavvia **tutti** i nodi con la nuova chiave e la precedente:

   ```bash
   NEXO_KMS_SECRET_KEY="nexo-key-2027:$(openssl rand -base64 32)"   # meglio in un file (_FILE)
   NEXO_KMS_SECRET_KEY_PREVIOUS="nexo-key:<base64 della chiave precedente>"
   ```

   Le chiavi dati sigillate dalla vecchia master key restano leggibili; i nuovi oggetti usano la nuova.
2. Ri-sigilla le chiavi dati di tutti gli oggetti SSE-S3 e SSE-KMS (e degli upload multipart in corso) con
   la chiave corrente. I dati non vengono riscritti:

   ```console
   $ nexoctl admin kms reseal local
   Re-sealed 2 of 2 data keys (2 versions, 0 pending uploads) in 2 buckets.
   ```

   `--bucket NOME` limita l'operazione a un bucket. Il comando esce con errore se qualche chiave non può
   essere ri-sigillata: ripetilo finché non riporta errori.
3. Rimuovi `NEXO_KMS_SECRET_KEY_PREVIOUS` e riavvia i nodi.

Con KES la rotazione delle chiavi avviene su KES; `nexoctl admin kms reseal` porta le chiavi dati sulla
versione corrente. Richiede `admin:KMSCreateKey`.

## Cosa resta in chiaro

- nomi di bucket e chiavi degli oggetti, dimensioni, date, metadati utente e tag;
- l'MD5 (ETag) degli oggetti SSE-S3, come in S3.

I checksum S3 (`x-amz-checksum-*`) degli oggetti cifrati sono sigillati con una chiave derivata da quella
dell'oggetto. Le credenziali degli utenti IAM sono cifrate nei documenti di sistema del deployment.
