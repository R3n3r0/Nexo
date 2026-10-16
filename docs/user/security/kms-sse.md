# KMS and server-side encryption (SSE)

Nexo encrypts the objects on the drives with three S3-compatible modes:

| Mode | Key | Requires |
|------|-----|----------|
| **SSE-S3** | per-object data key, sealed by the default key of the KMS | a KMS |
| **SSE-KMS** | data key sealed by a **named** key of the KMS | a KMS |
| **SSE-C** | key provided by the client with every request, never stored | HTTPS |

The data is encrypted in blocks (DARE format, AES-256-GCM), range GETs work; the drives never contain the
plaintext content. With SSE-KMS and SSE-C the ETag is opaque too; with SSE-S3, as in S3, the ETag stays
the MD5 of the plaintext content (for data that must not reveal any digest use SSE-KMS or SSE-C).

## Configuring the KMS

### Built-in KMS

A 32-byte master key, with a name that becomes the default SSE-S3 key:

```bash
sudo sh -c 'umask 027 && echo "nexo-key:$(openssl rand -base64 32)" > /etc/nexo/kms-key'
sudo chgrp nexo /etc/nexo/kms-key
# /etc/default/nexo (the same on every node)
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

The keys created with `kms key create` are derived from the master key: keeping that one is enough.
**Without the master key the encrypted objects cannot be recovered**: keep a safe copy of it outside the
deployment.

### External KMS (KES)

```bash
NEXO_KMS_ENDPOINT=https://kes1.example.net:7373,https://kes2.example.net:7373
NEXO_KMS_KEY_NAME=nexo-default
NEXO_KMS_CLIENT_CERT=/etc/nexo/kes/client.crt
NEXO_KMS_CLIENT_KEY=/etc/nexo/kes/client.key
NEXO_KMS_CAPATH=/etc/nexo/kes/ca.crt
```

Only `https://` endpoints (the data keys travel in the KES responses), mTLS authentication, failover
in the given order, no redirects followed. Only one of the two KMS can be configured.

### In a cluster

Every node must have the same KMS configuration: at startup the nodes compare a fingerprint (never the
secrets) and a node that differs from the majority does not start (`ErrKMSMismatch`). The status is in
`nexoctl admin kms status`, on the **Info** page of the console and in the metric `nexo_kms_online`
(alert `NexoKMSOffline`).

## Default encryption of a bucket

```console
$ nexoctl encrypt set sse-s3 local/docs
Default encryption of `local/docs`: SSE-S3 (AES256).
$ nexoctl encrypt set sse-kms app-key local/secrets
$ nexoctl encrypt info local/docs
`local/docs`: SSE-S3 (AES256).
$ nexoctl encrypt clear local/docs
```

Every object written afterwards, without encryption headers of its own, is encrypted. Existing objects do
not change.

## Encrypting per request

With `nexoctl` (or with the S3 headers `x-amz-server-side-encryption*` from any SDK):

```bash
nexoctl cp --enc-s3 local/docs report.pdf local/docs/
nexoctl cp --enc-kms "local/secrets=app-key" report.pdf local/secrets/
nexoctl stat local/secrets/report.pdf          # Encryption : SSE-KMS (app-key)
```

### SSE-C

The key (32 bytes in base64) is provided by the client with every read and write; the server does not keep
it. S3 requires HTTPS for SSE-C and Nexo rejects keys over plain HTTP (`400 InvalidRequest`), unless
`NEXO_SSEC_ALLOW_INSECURE=on` is set, **for testing only**.

```bash
KEY=$(openssl rand -base64 32)
export NEXOCTL_ENC_C="local/private=$KEY"        # better than the command line
nexoctl cp report.pdf local/private/
nexoctl cat local/private/report.pdf > copy.pdf
```

Besides `NEXOCTL_ENC_C` (several comma-separated entries) you can use `--enc-c-file keys.txt`, a file with
one `ALIAS/BUCKET[/PREFIX]=BASE64KEY` entry per line and permissions 0600. Avoid `--enc-c` on the command
line: the key would be visible to the other local users and would stay in the shell history. **Losing the
SSE-C key means losing the object.**

## Master key rotation (built-in KMS)

1. Restart **every** node with the new key and the previous one:

   ```bash
   NEXO_KMS_SECRET_KEY="nexo-key-2027:$(openssl rand -base64 32)"   # better in a file (_FILE)
   NEXO_KMS_SECRET_KEY_PREVIOUS="nexo-key:<base64 of the previous key>"
   ```

   The data keys sealed by the old master key stay readable; new objects use the new one.
2. Re-seal the data keys of every SSE-S3 and SSE-KMS object (and of the multipart uploads in progress) with
   the current key. The data is not rewritten:

   ```console
   $ nexoctl admin kms reseal local
   Re-sealed 2 of 2 data keys (2 versions, 0 pending uploads) in 2 buckets.
   ```

   `--bucket NAME` limits the operation to one bucket. The command exits with an error if some key cannot
   be re-sealed: repeat it until it reports no errors.
3. Remove `NEXO_KMS_SECRET_KEY_PREVIOUS` and restart the nodes.

With KES the key rotation happens on KES; `nexoctl admin kms reseal` moves the data keys to the
current version. Requires `admin:KMSCreateKey`.

## What stays in clear text

- bucket names and object keys, sizes, dates, user metadata and tags;
- the MD5 (ETag) of SSE-S3 objects, as in S3.

The S3 checksums (`x-amz-checksum-*`) of encrypted objects are sealed with a key derived from that of the
object. The credentials of the IAM users are encrypted in the system documents of the deployment.
