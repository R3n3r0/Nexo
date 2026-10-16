# nexoctl cache webhook remove

Remove a purge webhook

```
nexoctl cache webhook remove ALIAS DIST ID [flags]
```

## Aliases

```
nexoctl cache webhook rm
```

## Examples

```
  nexoctl cache webhook rm local sito-web cloudflare
```

## Options

```
  -h, --help   help for remove
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

* [nexoctl cache webhook](nexoctl_cache_webhook.md)	 - Manage the purge webhooks of a distribution

