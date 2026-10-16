# nexoctl cache warm status

Show the progress of a warm-up on every node

```
nexoctl cache warm status ALIAS DIST JOB [--wait] [flags]
```

## Examples

```
  nexoctl cache warm status local sito-web warm-18a2b3c4d5e6f-0a1b2c3d
```

## Options

```
  -h, --help               help for status
      --timeout duration   how long --wait waits (default 10m0s)
      --wait               wait until the warm-up finishes
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

* [nexoctl cache warm](nexoctl_cache_warm.md)	 - Cache objects of a domain before they are requested

