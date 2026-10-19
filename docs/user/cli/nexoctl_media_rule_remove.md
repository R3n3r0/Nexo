# nexoctl media rule remove

Remove a video rule of a bucket

```
nexoctl media rule remove ALIAS/BUCKET ID [flags]
```

## Aliases

```
nexoctl media rule rm
```

## Examples

```
  nexoctl media rule rm local/videos hd
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

* [nexoctl media rule](nexoctl_media_rule.md)	 - Add or remove one video rule of a bucket

