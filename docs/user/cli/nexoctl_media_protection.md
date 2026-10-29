# nexoctl media protection

Show or change the download protection of the videos of a bucket

## Synopsis

Show or change the download protection of the videos of a bucket.

A protected video streams encrypted (HLS AES-128, a key every 10 segments): its
keys go only to a playback token of the video ("nexoctl media token") or to a
console session or a signed request allowed to stream it, at most PACE times
faster than the video plays and to MAX-SESSIONS simultaneous streams of a
viewer (a newer stream replaces the oldest). Its original cannot be downloaded
without nexo:DownloadProtectedMedia. The Nexo player draws the viewer's
watermark over the picture.

```
nexoctl media protection [flags]
```

## Options

```
  -h, --help   help for protection
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
* [nexoctl media protection get](nexoctl_media_protection_get.md)	 - Show the download protection of the videos of a bucket
* [nexoctl media protection set](nexoctl_media_protection_set.md)	 - Change the download protection of the videos of a bucket

