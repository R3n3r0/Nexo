# nexoctl admin replicate

Keep several deployments in sync (site replication)

## Synopsis

Group several Nexo deployments (sites) so that they stay in sync: their users, groups,
access keys, named policies and buckets with their settings are the same everywhere, and every
bucket replicates its objects to the same bucket of every other site (active-active, the most
recent version wins). A change made on any site reaches the others within seconds; a site that
was unreachable catches up when it comes back.

The sites must share the root credentials: they call each other's Admin API with them and seal
what they exchange with a key derived from them. The configuration, the KMS keys, the
notification targets and the distributions stay per site.

```
nexoctl admin replicate [flags]
```

## Options

```
  -h, --help   help for replicate
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
* [nexoctl admin replicate add](nexoctl_admin_replicate_add.md)	 - Group the deployments of the aliases (the alias names name the sites)
* [nexoctl admin replicate info](nexoctl_admin_replicate_info.md)	 - Show the sites of the group and their synchronization
* [nexoctl admin replicate remove](nexoctl_admin_replicate_remove.md)	 - Dissolve the group of the site (on every site reached)
* [nexoctl admin replicate sync](nexoctl_admin_replicate_sync.md)	 - Run a synchronization round now

