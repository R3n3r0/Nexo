# nexoctl cache policy

Manage the reusable cache policies

```
nexoctl cache policy [flags]
```

## Options

```
  -h, --help   help for policy
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
* [nexoctl cache policy create](nexoctl_cache_policy_create.md)	 - Define a cache policy
* [nexoctl cache policy edit](nexoctl_cache_policy_edit.md)	 - Change a defined cache policy
* [nexoctl cache policy export](nexoctl_cache_policy_export.md)	 - Export the defined cache policies as YAML
* [nexoctl cache policy import](nexoctl_cache_policy_import.md)	 - Create or replace the cache policies of a YAML file
* [nexoctl cache policy info](nexoctl_cache_policy_info.md)	 - Show a cache policy and the Cache-Control it sends
* [nexoctl cache policy list](nexoctl_cache_policy_list.md)	 - List the cache policies, built-in and defined
* [nexoctl cache policy remove](nexoctl_cache_policy_remove.md)	 - Remove a defined cache policy no setting names

