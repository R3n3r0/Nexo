## nexoctl admin cluster import

Import an archive of "admin cluster export" (FILE, or - for stdin)

### Synopsis

Import an archive of "admin cluster export". The whole archive is validated first;
--dry-run stops there and shows what the import would do. --mode merge (default) keeps
what the archive does not hold; --mode replace also replaces the whole configuration and
removes the users, groups and named policies the archive does not hold (buckets are never
removed).

```
nexoctl admin cluster import [flags] ALIAS FILE
```

### Examples

```
  nexoctl admin cluster import cluster backup.zip --dry-run
  nexoctl admin cluster import cluster backup.zip
```

### Options

```
      --dry-run       validate the archive and show what would change, without importing
  -h, --help          help for import
      --mode string   merge (keep what the archive does not hold) or replace (default "merge")
```

### Options inherited from parent commands

```
      --config-dir string   configuration directory (default $NEXOCTL_CONFIG_DIR or ~/.nexoctl)
      --debug               dump the HTTP requests and responses on stderr, with the secrets redacted
      --json                print JSON Lines (one object per line with a "status" member)
      --no-color            disable colors (also with the NO_COLOR environment variable)
  -q, --quiet               hide confirmations and progress (results and errors are still printed)
```

### SEE ALSO

* [nexoctl admin cluster](nexoctl_admin_cluster.md)	 - Export and import the state of a deployment (IAM, policies, configuration, bucket settings)

