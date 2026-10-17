# Migrating from MinIO

Nexo speaks the same S3 API as MinIO, so applications, SDKs and tools such as `rclone`, `restic` or `aws s3`
keep working once they point at the new endpoint. This guide copies a MinIO deployment into a new
Nexo deployment: buckets and their settings, every object version, policies, users and access keys. The
MinIO deployment stays untouched until you switch the clients.

Every command below was run against a MinIO server with versioned buckets, delete markers, tags, user
metadata, lifecycle rules, an anonymous bucket policy, a named policy and a user, and the result was checked
with `nexoctl diff`.

## Before you start

| MinIO | Nexo |
|-------|------|
| `minio server /data{1...4}` | `nexo server /data{1...4}` (same ellipsis syntax, same erasure-coded layout on several drives) |
| `MINIO_ROOT_USER` / `MINIO_ROOT_PASSWORD` | `NEXO_ROOT_USER` / `NEXO_ROOT_PASSWORD` |
| S3 API on `:9000`, console on `:9001` | the same default ports |
| `/minio/health/live`, `/minio/health/ready` | `/nexo/health/live`, `/nexo/health/ready` (update probes and load balancers) |
| `mc` for data, `mc admin` for administration | `nexoctl` for both; `mc` keeps working for the S3 commands, `mc admin` does not |
| `MINIO_KMS_*` / KES | built-in KMS (`NEXO_KMS_SECRET_KEY`) or KES (`NEXO_KMS_ENDPOINT`, ...): see [KMS and SSE](security/kms-sse.md) |

The on-disk format is **not** compatible: Nexo cannot start on MinIO drives. The migration copies the data
through the S3 API, so you need room for the new deployment next to the old one (other drives or other
machines).

1. Install Nexo and start it on new drives ([Installation](installation.md),
   [Deployment modes](deployment-modes.md)).
2. Install `nexoctl` on a machine that reaches both deployments, and `jq` for the loops below.
3. Create one alias for each side. `nexoctl` talks S3 to MinIO, so any MinIO access key with read
   access to every bucket works; on the Nexo side use the root user or an administrator.

   ```bash
   nexoctl alias set minio https://minio.example.com:9000 "$MINIO_ACCESS_KEY" "$MINIO_SECRET_KEY"
   nexoctl alias set nexo https://nexo.example.com:9000 admin "$NEXO_ROOT_PASSWORD"
   ```

   The admin commands (`nexoctl admin ...`) are used only on the `nexo` alias.

## 1. Policies

Named IAM policies use the same JSON grammar (`Version`, `Statement`, `Effect`, `Action`, `Resource`,
`Condition`). Export them with `mc` and create them on Nexo; the built-in ones (`readonly`, `readwrite`,
`writeonly`, `diagnostics`, `consoleAdmin`) already exist in Nexo.

```bash
for p in $(mc admin policy ls minio --json | jq -r '.policy' \
           | grep -vE '^(consoleAdmin|diagnostics|readonly|readwrite|writeonly)$'); do
  mc admin policy info minio "$p" --json | jq '.policyInfo.Policy' > "policy-$p.json"
  nexoctl admin policy create --update nexo "$p" "policy-$p.json"
done
```

A document that uses an action or condition key Nexo does not know is rejected with the line and the
column of the error: fix it and run the command again. `nexoctl admin policy simulate` shows what a policy
allows before you hand it to users.

## 2. Users, groups and access keys

MinIO never returns secret keys, so users cannot be copied as they are. Recreate them with the secret you
choose (or the one the application already has in its configuration) and attach the same policies:

```bash
mc admin user ls minio --json | jq -r '[.accessKey, (.policyName // "")] | @tsv'

nexoctl admin user add nexo alice "$ALICE_SECRET"
nexoctl admin policy attach nexo photos-ro --user alice
```

Groups: `nexoctl admin group add nexo GROUP MEMBER...` and `nexoctl admin policy attach nexo POLICY --group
GROUP`.

Service accounts (access keys of a user) can keep the **same key pair**, so the applications do not need new
credentials:

```bash
nexoctl admin accesskey create nexo --user alice --name backup-job \
  --access-key "$OLD_ACCESS_KEY" --secret-key "$OLD_SECRET_KEY"
```

An inline policy of the MinIO service account goes in `--policy FILE`.

## 3. Buckets and their settings

Create each bucket with the same versioning (and Object Lock) as on MinIO, then copy the lifecycle rules and
the bucket policy. `nexoctl` reads them from MinIO through the S3 API.

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

- Buckets with **Object Lock**: create them with `nexoctl mb --with-lock nexo/BUCKET` instead (a bucket
  cannot be switched to Object Lock later), and set the same default retention with `nexoctl retention set
  --default --mode governance --validity 30d nexo/BUCKET`.
- **Quota**: `nexoctl quota set nexo/BUCKET --size 500GiB`.
- **Default encryption**: `nexoctl encrypt set sse-s3 nexo/BUCKET` (or `sse-kms KEY`).
- **Notifications**: the targets are `notify_*` configuration subsystems as in MinIO (`notify_webhook`,
  `notify_kafka`, ...), set with `nexoctl admin config set`; then add the bucket rules with `nexoctl event add`.
  See [Events and lifecycle](events-lifecycle.md) for the keys of each target.
- **CORS**: `nexoctl cors set nexo/BUCKET cors.json` (JSON or S3 XML).

## 4. Data, with every version

```bash
for b in $(nexoctl ls --json minio | jq -r '.name | rtrimstr("/")'); do
  nexoctl mirror --preserve "minio/$b" "nexo/$b"
done
```

`--preserve` copies every version and every delete marker, oldest first, with content type, user
metadata and tags. The version IDs on Nexo are new; order, content and the latest version are the same.

- It is incremental: running it again copies only what changed on MinIO since the last run.
- `--limit-download 200MiB` / `--limit-upload 200MiB` cap the bandwidth if MinIO is serving production
  traffic; `--dry-run` shows what would be copied.
- Objects encrypted on MinIO with SSE-S3 or SSE-KMS are read in clear by `nexoctl` (MinIO decrypts them) and
  written according to the default encryption of the Nexo bucket. Objects with **SSE-C** need their keys:
  `--enc-c-file keys.txt` with a line `minio/BUCKET=BASE64KEY` and one `nexo/BUCKET=BASE64KEY`.
- **Retention and legal hold** of single objects are not copied by `mirror`: apply them again with
  `nexoctl retention set --mode governance --validity 30d -r nexo/BUCKET/PREFIX/` and
  `nexoctl legalhold set nexo/BUCKET/OBJECT`.

`mc mirror` works too (`mc alias set nexo ...` with the Nexo endpoint), but it copies only the latest
version and does not create missing buckets: prefer `nexoctl mirror --preserve`.

## 5. Check and switch

1. Compare each bucket. `nexoctl diff` prints nothing when the latest versions match:

   ```bash
   for b in $(nexoctl ls --json minio | jq -r '.name | rtrimstr("/")'); do
     nexoctl diff "minio/$b" "nexo/$b"
   done
   nexoctl ls --versions nexo/BUCKET/PREFIX/    # spot-check the history of a few keys
   ```

2. Stop the writes to MinIO (stop the writers, or attach a read-only policy to their users), run step 4
   once more to copy the last changes, and run the diff again.
3. Point the clients at Nexo: change the DNS name or the load balancer backend, or the endpoint in the
   application configuration. Access keys recreated with the same pair keep working; presigned URLs signed
   for the old host do not.
4. Keep MinIO read-only for a while, then retire it.

## What else to look at

- **Console and alerts**: the web console runs on the console port (`:9001`); the Prometheus metrics, the
  Grafana dashboard and the alerts are described in [Monitoring](operations/monitoring.md).
- **Identity providers**: users of an external OpenID Connect or LDAP provider configured in MinIO are not
  migrated; Nexo uses its own IAM users, groups and STS (`AssumeRole`).
- **Site replication, bucket replication, tiering, batch jobs**: Nexo has no equivalent for these MinIO
  features; plan around them before migrating. The [S3 compatibility](reference/s3-compatibility.md) page lists
  what Nexo supports.
- **Afterwards**: the state of Nexo itself (users, policies, buckets) can be backed up and moved with
  `nexoctl admin cluster export/import`; see [Backup and migration](operations/backup-migration.md).
