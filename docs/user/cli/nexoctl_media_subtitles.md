# nexoctl media subtitles

List, download, upload or remove the subtitle tracks of a video

## Synopsis

List, download, upload or remove the subtitle tracks of a video (default its
latest version).

A track is manual (uploaded, or edited in the console) or automatic: a media
rule with "subtitles" makes the media worker transcribe the speech of the video
and translate the transcription. An automatic track applies only to the content
it derives from (the video overwritten, it is no longer served until the worker
runs again); a manual track always applies and the worker never overwrites it.
The master playlist of the video lists the tracks: a player shows them as
captions.

```
nexoctl media subtitles [flags]
```

## Aliases

```
nexoctl media subs
```

## Options

```
  -h, --help   help for subtitles
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
* [nexoctl media subtitles get](nexoctl_media_subtitles_get.md)	 - Download a subtitle track (WebVTT or SubRip)
* [nexoctl media subtitles ls](nexoctl_media_subtitles_ls.md)	 - List the subtitle tracks of a video
* [nexoctl media subtitles put](nexoctl_media_subtitles_put.md)	 - Upload a subtitle track from a WebVTT or SubRip file
* [nexoctl media subtitles rm](nexoctl_media_subtitles_rm.md)	 - Remove a subtitle track of a video

