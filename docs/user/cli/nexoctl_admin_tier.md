# nexoctl admin tier

Manage the remote tiers of the lifecycle transitions

## Synopsis

Manage the remote tiers: buckets of other S3 servers (Nexo, a cloud provider) that the
Transition actions of the bucket lifecycle move the data of old versions to. The name of a tier is
the storage class the transitions name (nexoctl ilm rule add --transition-days 30 --transition-tier COLD).

A transitioned version keeps its metadata here and is read from the tier, transparently; its
storage class becomes the name of the tier. A copy of the object onto itself brings its data back.
The objects of the tier that no version refers to any more are removed by a periodic sweep. A tier
that holds data cannot be removed.

The remote bucket is given as REMOTE\_ALIAS/BUCKET: the URL and the credentials of that alias are
stored, sealed, by the server (read, write, list and delete in the bucket).

```
nexoctl admin tier [flags]
```

## Options

```
  -h, --help   help for tier
```

## Options inherited from parent commands

```
      --config-dir string   configuration directory (default $NEXOCTL_CONFIG_DIR or ~/.nexoctl)
      --debug               dump the HTTP requests and responses on stderr, with the secrets redacted
      --json                print JSON Lines (one object per line with a "status" member)
      --no-color            disable colors (also with the NO_COLOR environment variable)
  -q, --quiet               hide confirmations and progress (results and errors are still printed)
```

## SEE ALSO

* [nexoctl admin](nexoctl_admin.md)	 - Administer a Nexo server (Admin API)
* [nexoctl admin tier add](nexoctl_admin_tier_add.md)	 - Add a remote tier (its bucket is checked with the credentials of the remote alias)
* [nexoctl admin tier edit](nexoctl_admin_tier_edit.md)	 - Replace the credentials of a tier with those of the remote alias
* [nexoctl admin tier ls](nexoctl_admin_tier_ls.md)	 - List the tiers
* [nexoctl admin tier rm](nexoctl_admin_tier_rm.md)	 - Remove a tier that holds no data

