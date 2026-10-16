# nexoctl dist resolve

Show what serves a host (distribution, virtual host, S3 API)

```
nexoctl dist resolve ALIAS HOST [flags]
```

## Examples

```
  nexoctl dist resolve local www.example.com
```

## Options

```
  -h, --help   help for resolve
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

* [nexoctl dist](nexoctl_dist.md)	 - Manage the distributions: custom domains of the buckets

