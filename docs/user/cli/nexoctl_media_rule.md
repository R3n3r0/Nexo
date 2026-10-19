# nexoctl media rule

Add or remove one video rule of a bucket

```
nexoctl media rule [flags]
```

## Options

```
  -h, --help   help for rule
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

* [nexoctl media](nexoctl_media.md)	 - Manage the video rules of buckets and the media worker (Admin API)
* [nexoctl media rule add](nexoctl_media_rule_add.md)	 - Add a video rule to a bucket, or replace the rule with the same ID
* [nexoctl media rule remove](nexoctl_media_rule_remove.md)	 - Remove a video rule of a bucket

