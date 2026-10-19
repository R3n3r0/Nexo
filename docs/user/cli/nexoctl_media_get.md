# nexoctl media get

Print the media rules of a bucket (JSON)

```
nexoctl media get ALIAS/BUCKET [flags]
```

## Aliases

```
nexoctl media info
```

## Examples

```
  nexoctl media get local/videos > media.json
  nexoctl media get --json local/videos
```

## Options

```
  -h, --help   help for get
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

* [nexoctl media](nexoctl_media.md)	 - Manage the video and image rules of buckets and the media worker (Admin API)

