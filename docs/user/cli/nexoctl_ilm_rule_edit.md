## nexoctl ilm rule edit

Change a lifecycle rule of a bucket

### Synopsis

Change the given settings of a lifecycle rule, keeping the others. A zero number
of days or an empty date or filter removes that setting.

```
nexoctl ilm rule edit --id ID [flags] ALIAS/BUCKET
```

### Examples

```
  nexoctl ilm rule edit --id rule-1 --expire-days 90 local/app
  nexoctl ilm rule edit --id rule-1 --disable local/app
```

### Options

```
      --abort-mpu-days int           abort the incomplete multipart uploads this many days after their start
      --disable                      disable the rule
      --enable                       enable the rule
      --expire-date string           expire the current versions on this date (YYYY-MM-DD)
      --expire-days int              expire the current versions this many days after their creation
      --expired-delete-marker        remove the delete markers without noncurrent versions
  -h, --help                         help for edit
      --id string                    ID of the rule to edit (required)
      --newer-noncurrent int         keep this many newest noncurrent versions
      --noncurrent-expire-days int   remove the noncurrent versions this many days after they became noncurrent
      --prefix string                apply to the objects under this prefix
      --size-gt string               apply to the objects larger than this size (e.g. 1MiB)
      --size-lt string               apply to the objects smaller than this size
      --tags string                  apply to the objects with these tags: "key1=value1&key2=value2"
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

* [nexoctl ilm rule](nexoctl_ilm_rule.md)	 - Add, edit, list, remove, export and import lifecycle rules

