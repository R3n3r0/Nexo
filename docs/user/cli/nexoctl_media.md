# nexoctl media

Manage the video rules of buckets and the media worker (Admin API)

## Synopsis

Manage the video rules of buckets and follow the work of the media worker.

The rules select videos by key prefix and suffix (default .mp4, .mov and .m4v,
matched without case); the first rule that selects a key applies. For each new
version of a selected video, the nexo-media-worker derives what its rule asks
for: lower-quality renditions (H.264 with the audio of the source; presets
2160p, 1440p, 1080p, 720p, 480p, 360p and 240p, those taller than the source
are skipped), a JPEG poster and timeline thumbnails (a sprite with its WebVTT
map). It writes them in the hidden sidecar prefix .nexo-media/ of the bucket:
ls, find and tree hide it unless --media is given (mirror and cp copy it, so
that the backups keep it). The worker receives its jobs from the notification
target notify\_media\_worker of the server.

The configuration is the JSON document of the Admin API and of the console
({"rules": [{"id": "videos", "prefix": "videos/", "renditions": ["720p"],
"poster": true, "thumbnails": {"interval": "10s"}}]}), the form of "media get".
Streaming a video (HLS packaged on the fly) needs no rule.

```
nexoctl media [flags]
```

## Options

```
  -h, --help   help for media
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

* [nexoctl](nexoctl.md)	 - Nexo command-line client
* [nexoctl media get](nexoctl_media_get.md)	 - Print the video rules of a bucket (JSON)
* [nexoctl media remove](nexoctl_media_remove.md)	 - Remove the video rules of a bucket
* [nexoctl media reprocess](nexoctl_media_reprocess.md)	 - Queue the processing of a video again
* [nexoctl media rule](nexoctl_media_rule.md)	 - Add or remove one video rule of a bucket
* [nexoctl media set](nexoctl_media_set.md)	 - Replace the video rules of a bucket (JSON)
* [nexoctl media status](nexoctl_media_status.md)	 - Show the processing state of a video by the media worker

