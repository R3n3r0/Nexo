# nexoctl media trace

Find the playback token a leaked copy of a video came from

## Synopsis

Find the playback token a leaked copy of a video came from, for a video
protected with the forensic watermark: every stream of a playback token
carries an invisible code, segment by segment. nexo-media-worker trace reads
the copy and writes its votes; this command sends them to Nexo, which reads
the code and lists the tokens whose code agrees, with the viewer they were
issued to (admin:TracePlaybackLeaks). --bits gives the code directly: one
character per segment (0, 1 or ? when unknown), segment n being bit n mod 64.

A full copy names its token with certainty (64 bits); a short clip of a few
segments agrees with many tokens: the more segments, the fewer candidates.

```
nexoctl media trace ALIAS/BUCKET/KEY (--votes FILE | --bits BITS) [flags]
```

## Examples

```
  nexo-media-worker trace --leak copy.mp4 --bucket courses --key lessons/intro.mp4 --out votes.json
  nexoctl media trace local/courses/lessons/intro.mp4 --votes votes.json
  nexoctl media trace local/courses/lessons/intro.mp4 --bits 0110?1
```

## Options

```
      --bits string         the code read from the copy: 0, 1 or ? per segment
  -h, --help                help for trace
      --limit int           most candidates listed (1 to 100) (default 10)
      --version-id string   version of the video (default: the current one)
      --votes string        the votes of nexo-media-worker trace (a JSON file)
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

