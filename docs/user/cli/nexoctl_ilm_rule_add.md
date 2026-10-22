# nexoctl ilm rule add

Add a lifecycle rule to a bucket

```
nexoctl ilm rule add [flags] ALIAS/BUCKET
```

## Examples

```
  nexoctl ilm rule add --prefix logs/ --expire-days 30 local/app
  nexoctl ilm rule add --noncurrent-expire-days 7 --newer-noncurrent 3 local/docs
  nexoctl ilm rule add --abort-mpu-days 2 --expired-delete-marker local/docs
```

## Options

```
      --abort-mpu-days int               abort the incomplete multipart uploads this many days after their start
      --disable                          disable the rule
      --expire-date string               expire the current versions on this date (YYYY-MM-DD)
      --expire-days int                  expire the current versions this many days after their creation
      --expired-delete-marker            remove the delete markers without noncurrent versions
  -h, --help                             help for add
      --id string                        rule ID (default: rule-N)
      --newer-noncurrent int             keep this many newest noncurrent versions
      --noncurrent-expire-days int       remove the noncurrent versions this many days after they became noncurrent
      --noncurrent-transition-days int   move the noncurrent versions to the tier this many days after they became noncurrent (-1 removes)
      --prefix string                    apply to the objects under this prefix
      --size-gt string                   apply to the objects larger than this size (e.g. 1MiB)
      --size-lt string                   apply to the objects smaller than this size
      --tags string                      apply to the objects with these tags: "key1=value1&key2=value2"
      --transition-days int              move the current versions to the tier this many days after their creation (-1 removes)
      --transition-tier string           remote tier (nexoctl admin tier) the transitions move the data to ("" removes them)
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

* [nexoctl ilm rule](nexoctl_ilm_rule.md)	 - Add, edit, list, remove, export and import lifecycle rules

