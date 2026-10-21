# nexoctl media subtitles ls

List the subtitle tracks of a video

```
nexoctl media subtitles ls ALIAS/BUCKET/KEY [--version-id V] [flags]
```

## Aliases

```
nexoctl media subtitles list
```

## Examples

```
  nexoctl media subtitles ls local/videos/courses/intro.mp4
```

## Options

```
  -h, --help                help for ls
      --version-id string   the version (default: the latest)
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

* [nexoctl media subtitles](nexoctl_media_subtitles.md)	 - List, download, upload or remove the subtitle tracks of a video

