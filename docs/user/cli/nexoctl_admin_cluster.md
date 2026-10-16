# nexoctl admin cluster

Export and import the state of a deployment (IAM, policies, configuration, bucket settings)

## Synopsis

Export and import the state of a deployment besides its objects: users, groups,
access keys and their policies, the named policies, the dynamic configuration and the settings
of every bucket (versioning, object lock, policy, lifecycle, encryption, quota, tags, CORS,
notifications), in one zip archive.

To migrate a deployment (e.g. single drive to cluster), export the source and import the
archive into the target first (it creates the buckets with their versioning), then copy the
objects with every version with "nexoctl mirror --preserve". The IAM part of the archive is
encrypted with the root credentials of the source: the target must use the same ones. The
archive holds the secrets of the configuration: protect it.

```
nexoctl admin cluster [flags]
```

## Options

```
  -h, --help   help for cluster
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
* [nexoctl admin cluster export](nexoctl_admin_cluster_export.md)	 - Export the state of the deployment as a zip archive (stdout or --output)
* [nexoctl admin cluster import](nexoctl_admin_cluster_import.md)	 - Import an archive of "admin cluster export" (FILE, or - for stdin)

