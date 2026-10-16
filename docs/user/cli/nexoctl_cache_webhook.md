# nexoctl cache webhook

Manage the purge webhooks of a distribution

```
nexoctl cache webhook [flags]
```

## Options

```
  -h, --help   help for webhook
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

* [nexoctl cache](nexoctl_cache.md)	 - Manage the cache of the distributions
* [nexoctl cache webhook add](nexoctl_cache_webhook_add.md)	 - Add a purge webhook to a distribution
* [nexoctl cache webhook list](nexoctl_cache_webhook_list.md)	 - List the purge webhooks of a distribution
* [nexoctl cache webhook remove](nexoctl_cache_webhook_remove.md)	 - Remove a purge webhook
* [nexoctl cache webhook test](nexoctl_cache_webhook_test.md)	 - Call a purge webhook once (connection test)

