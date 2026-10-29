# nexoctl media protection get

Show the download protection of the videos of a bucket

```
nexoctl media protection get ALIAS/BUCKET [flags]
```

## Aliases

```
nexoctl media protection info
```

## Examples

```
  nexoctl media protection get local/courses
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

* [nexoctl media protection](nexoctl_media_protection.md)	 - Show or change the download protection of the videos of a bucket

