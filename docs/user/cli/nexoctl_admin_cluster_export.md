# nexoctl admin cluster export

Export the state of the deployment as a zip archive (stdout or --output)

```
nexoctl admin cluster export [flags] ALIAS
```

## Examples

```
  nexoctl admin cluster export local --output backup.zip
  nexoctl admin cluster export local > backup.zip
```

## Options

```
  -h, --help            help for export
  -o, --output string   write the archive to this file (mode 0600)
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

* [nexoctl admin cluster](nexoctl_admin_cluster.md)	 - Export and import the state of a deployment (IAM, policies, configuration, bucket settings)

