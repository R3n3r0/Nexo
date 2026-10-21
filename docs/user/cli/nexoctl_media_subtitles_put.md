# nexoctl media subtitles put

Upload a subtitle track from a WebVTT or SubRip file

## Synopsis

Upload a WebVTT or SubRip document (FILE, or the standard input with "-" or no
FILE; at most 2 MiB) as the manual subtitle track of a language (a BCP 47 tag:
it, en, pt-BR...), replacing the track of that language: an automatic one too,
which the media worker then leaves alone. The text of the cues is sanitized:
only the WebVTT tags b, i, u, c, v, lang and ruby are kept.

```
nexoctl media subtitles put ALIAS/BUCKET/KEY LANG [FILE] [flags]
```

## Examples

```
  nexoctl media subtitles put local/videos/courses/intro.mp4 it intro.it.srt
  nexoctl media subtitles get local/videos/a.mp4 en | nexoctl media subtitles put local/videos/b.mp4 en
```

## Options

```
  -h, --help                help for put
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

