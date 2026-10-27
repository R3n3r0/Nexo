# Remote tiers

A remote tier is an S3 bucket of another server (another Nexo deployment, MinIO, AWS S3, any S3-compatible
store) where the bucket lifecycle moves the data of old versions to free the local drives. A transitioned
version keeps its metadata on the drives (its key, version id, size, ETag, checksums, tags, retention,
encryption) and is read from its tier, transparently: GET, HEAD, ranges, copies and replication behave as
before. Its storage class becomes the name of the tier.

## Add a tier

Create an alias for the remote server, with credentials that can read, write, list and delete in the bucket,
then add the tier:

```console
$ nexoctl alias set archive https://s3.archive.example.com ARCHIVEKEY ARCHIVESECRET
$ nexoctl admin tier add local COLD archive/cold-data --prefix nexo/
Tier COLD added: https://s3.archive.example.com/cold-data/nexo.
$ nexoctl admin tier ls local
NAME             ENDPOINT                                 BUCKET                   CLASS          ACCESS KEY
COLD             https://s3.archive.example.com           cold-data/nexo           -              ARCHIVEKEY
```

The name of the tier (upper case letters, digits, `-` and `_`) is the storage class the lifecycle rules name.
The remote bucket is checked with the credentials, which the server stores sealed. `--storage-class` sets the
storage class of the remote objects: use a class readable at once (`STANDARD_IA`, `GLACIER_IR`), not an
archive class that needs a restore. The tiers are also managed in the console, on the **Cluster** page.

`admin tier edit local COLD archive2` replaces the credentials with those of another alias (the endpoint, the
bucket and the prefix of a tier never change). The actions are `admin:ListTier` and `admin:SetTier`.

## Transition rules

A `Transition` moves the current versions a number of days after their creation, a
`NoncurrentVersionTransition` the noncurrent ones a number of days after they became noncurrent:

```console
$ nexoctl ilm rule add --prefix logs/ --transition-days 30 --transition-tier COLD local/app
$ nexoctl ilm rule add --id old-versions --noncurrent-transition-days 7 --transition-tier COLD local/app
```

The same rules are set with `PutBucketLifecycleConfiguration` (`StorageClass` is the name of the tier) and in
the **Lifecycle** tab of the bucket. A rule naming no tier is refused (`InvalidStorageClass`). The scanner applies
the rules in the background; `nexoctl ilm apply local/app` applies them at once.

When an expiration and a transition are both due, the expiration wins. The versions stored inline (small
objects, 128 KiB by default) and the empty ones stay on the drives.

## How the data is kept safe

- The stored bytes of the version (encrypted and compressed as on the drives: the tier never sees plaintext
  of an encrypted object) are written to the tier as one object, under `<prefix><deployment id>/`, and their
  size is checked there.
- Only then does the version record its tier, under its write lock, if it is still the same version; then the
  local data is released. A transition interrupted between the two steps is completed by the healing.
- A version replaced or deleted leaves its tier object behind: the leader removes, every hour, the objects of
  the tier that no version refers to any more and that are older than one hour. The objects of other
  deployments sharing the bucket are never touched.
- A tier holding the data of transitioned versions cannot be removed.

To bring the data of an object back to the drives, copy it onto itself with any S3 client:

```console
$ aws --endpoint-url http://localhost:9000 s3 cp s3://app/logs/2025-01.log s3://app/logs/2025-01.log \
    --storage-class STANDARD
```

The remote tier is part of the durability of the transitioned versions: protect it like a backup (versioning,
replication or the guarantees of the provider). A version whose tier is unreachable cannot be read until the
tier comes back.
