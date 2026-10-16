# Backup and migration

A Nexo deployment has two kinds of content to protect:

- the **state** of the deployment: users, groups, access keys, policies, dynamic configuration and bucket
  metadata (versioning, policies, lifecycle, notifications, CORS, tags, quota, object lock, encryption);
- the **data**: the objects, with all their versions.

`nexoctl admin cluster export/import` handles the state, `nexoctl mirror` the data. Together they let you
take backups and migrate between modes (SNSD → SNMD → cluster) or between clusters.

## Exporting the state

```console
$ nexoctl admin cluster export local --output backup.zip
Exported the state of "local" to backup.zip (2.3KiB).
```

The archive (zip, written with permissions 0600) contains:

| File | Contents |
|------|----------|
| `manifest.json` | format, server version, list of users, groups, policies, subsystems and buckets |
| `iam.bin` | users, groups, access keys and STS sessions, **encrypted** (AES-256-GCM with a key derived with Argon2id from the root credentials) |
| `policies.json` | named policies |
| `config.json` | dynamic configuration |
| `buckets/<name>.json` | metadata of every bucket |

The deployment that imports `iam.bin` must have **the same root credentials** as the one that exported it.
The root user is not in the archive (it comes from the environment). Requires `admin:ExportCluster`.

For a periodic backup, just schedule the export (cron, systemd timer) and keep the archives outside the
deployment.

## Importing the state

```console
$ nexoctl admin cluster import new backup.zip --dry-run
Would import into "new" (merge): 1 user(s), 0 group(s), 0 polic(ies), 0 configuration subsystem(s), 2 bucket(s).
Buckets created: docs, photos

$ nexoctl admin cluster import new backup.zip
Imported into "new" (merge): 1 user(s), 0 group(s), 0 polic(ies), 0 configuration subsystem(s), 2 bucket(s).
Buckets created: docs, photos
```

- The whole archive is validated before anything is applied; `--dry-run` stops there and shows
  what would change.
- `--mode merge` (default) adds and updates, keeping whatever the archive does not contain.
- `--mode replace` also replaces the whole configuration and removes the users, groups and named policies
  missing from the archive. Buckets are never removed.
- Missing buckets are created with their settings (versioning included). Requires
  `admin:ImportCluster`.

## Copying the data

`nexoctl mirror` synchronizes any two locations (local directories, buckets of the same alias or of another
one, even of another S3 server):

```bash
nexoctl mirror local/photos ./backup/photos            # backup to disk: copies what is missing or changed
nexoctl mirror --overwrite --remove local/photos ./backup/photos   # exact copy (deletes what no longer exists)
nexoctl mirror --preserve old/docs new/docs            # every version and delete marker, with metadata and tags
```

- A second run transfers only the differences.
- `--preserve` copies, between two buckets, **every version** and every delete marker from the oldest to the
  newest, with content type, metadata and tags. The destination bucket must have versioning enabled if there
  are several versions (importing the state already creates it that way). The version IDs in the target are
  new; order and content are preserved.
- `--dry-run` shows what it would do; `--limit-upload`/`--limit-download` limit the bandwidth.

## Migrating to another mode

Example: from an SNSD server (alias `old`) to a new cluster (alias `new`), with the **same root
credentials**.

1. Install and start the new deployment ([Deployment modes](../deployment-modes.md)) and create the aliases:

   ```bash
   nexoctl alias set old http://old.lan:9000 admin "$ROOT_PASSWORD"
   nexoctl alias set new http://nexo.lan:9000 admin "$ROOT_PASSWORD"
   ```

2. Export and import the state **before** the data, so the buckets already exist with their versioning:

   ```bash
   nexoctl admin cluster export old --output state.zip
   nexoctl admin cluster import new state.zip --dry-run
   nexoctl admin cluster import new state.zip
   ```

3. Copy the data bucket by bucket with every version:

   ```bash
   for b in $(nexoctl ls --json old | jq -r '.name | rtrimstr("/")'); do
     nexoctl mirror --preserve "old/$b" "new/$b"
   done
   ```

4. Stop the writes on the old deployment (or make it read-only by removing the write permissions of the
   applications), repeat step 3 to copy the latest changes, then move the clients to the new
   endpoint (DNS or load balancer).
5. Verify: `nexoctl diff old/docs new/docs` prints nothing if the two buckets match (otherwise it lists the
   objects present on one side only or different); `nexoctl admin info new`.

This procedure is verified in CI (test TM-04: identical objects, versions, IAM, policies and lifecycle after
the migration from SNSD to cluster).

## What is not in the archive

- the object data (copy it with `mirror`);
- the KMS keys: the master key of the built-in KMS (`NEXO_KMS_SECRET_KEY`) must be kept separately, because
  without it the objects encrypted with SSE-S3 or SSE-KMS cannot be read; with `mirror` the data is
  read in clear text and rewritten according to the default encryption of the destination bucket;
- distributions, key groups, certificates and cache: they are exported with `nexoctl dist export`/`import` and
  `nexoctl cache policy export`/`import` (see [Distributions](../distributions/README.md));
- the access rules of the buckets: `nexoctl access export`/`import` per bucket.
