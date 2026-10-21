# nexoctl media subtitles rm

Remove a subtitle track of a video

## Synopsis

Remove the subtitle track of a language. An automatic track comes back when the
media worker processes the video again ("media reprocess").

```
nexoctl media subtitles rm ALIAS/BUCKET/KEY LANG [--version-id V] [flags]
```

## Aliases

```
nexoctl media subtitles remove
```

## Examples

```
  nexoctl media subtitles rm local/videos/courses/intro.mp4 de
```

## Options

```
  -h, --help                help for rm
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

