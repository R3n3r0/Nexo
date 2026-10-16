# nexoctl cache purge status

Show the state of an invalidation on every node

```
nexoctl cache purge status ALIAS ID [--dist DIST] [--wait] [flags]
```

## Examples

```
  nexoctl cache purge status local inv-18a2b3c4d5e6f-0a1b2c3d
```

## Options

```
      --dist string        distribution of the invalidation (default: found by its id)
  -h, --help               help for status
      --timeout duration   how long --wait waits (default 1m0s)
      --wait               wait until every node applied the invalidation
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

* [nexoctl cache purge](nexoctl_cache_purge.md)	 - Invalidate cached content

