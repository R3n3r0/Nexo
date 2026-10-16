# nexoctl access block-public off

Stop blocking the public access

```
nexoctl access block-public off ALIAS[/BUCKET] [flags]
```

## Examples

```
  nexoctl access block-public on local --mode reject
  nexoctl access block-public on local/media
  nexoctl access block-public off local
```

## Options

```
  -h, --help   help for off
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

* [nexoctl access block-public](nexoctl_access_block-public.md)	 - Block the public access of the cluster or of a bucket

