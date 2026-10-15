## nexoctl dist behavior

Manage the behaviors per path of a distribution

```
nexoctl dist behavior [flags]
```

### Options

```
  -h, --help   help for behavior
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

* [nexoctl dist](nexoctl_dist.md)	 - Manage the distributions: custom domains of the buckets
* [nexoctl dist behavior add](nexoctl_dist_behavior_add.md)	 - Add a behavior for the paths of a pattern
* [nexoctl dist behavior list](nexoctl_dist_behavior_list.md)	 - List the behaviors of a distribution, in evaluation order
* [nexoctl dist behavior move](nexoctl_dist_behavior_move.md)	 - Move a behavior (1-based positions): the first match wins
* [nexoctl dist behavior remove](nexoctl_dist_behavior_remove.md)	 - Remove the behavior N (1-based, see behavior ls)

