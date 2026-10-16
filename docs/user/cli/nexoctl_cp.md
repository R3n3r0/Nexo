# nexoctl cp

Copy files and objects

## Synopsis

Copy local files to a bucket, objects to local files, or objects between
buckets (server-side within one alias). Large files are sent as multipart
uploads whose parts are uploaded in parallel; Ctrl-C aborts the upload on the
server.

A source ending with "/" copies the content of the directory or prefix,
otherwise the directory itself; directories and prefixes need --recursive.

```
nexoctl cp [flags] SOURCE... TARGET
```

## Examples

```
  nexoctl cp report.pdf local/docs/
  nexoctl cp --recursive photos/ local/photos/2026/
  nexoctl cp local/docs/report.pdf ./
  nexoctl cp --attr "owner=ops;env=prod" --tags "team=ops" app.log local/logs/
```

## Options

```
      --attr string              user metadata of the uploaded objects: "key1=value1;key2=value2"
      --checksum string          additional checksum of the uploaded objects, verified by the server: crc32, crc32c, crc64nvme, sha1, sha256
      --content-type string      content type of the uploaded objects (default: from the file extension)
      --enc-c stringArray        SSE-C key of the objects under a location: "ALIAS/BUCKET[/PREFIX]=BASE64KEY" (32 bytes; repeatable). Prefer --enc-c-file or NEXOCTL_ENC_C: a key in the arguments is visible to other local users and in the shell history
      --enc-c-file stringArray   file of SSE-C keys, one "ALIAS/BUCKET[/PREFIX]=BASE64KEY" per line ("#" comments; repeatable)
      --enc-kms stringArray      encrypt the objects written under a location with SSE-KMS: "ALIAS/BUCKET[/PREFIX][=KEY]" (repeatable)
      --enc-s3 stringArray       encrypt the objects written under a location with SSE-S3: "ALIAS/BUCKET[/PREFIX]" (repeatable)
  -h, --help                     help for cp
      --limit-download string    cap the download bandwidth (e.g. 10MiB, per second)
      --limit-upload string      cap the upload bandwidth (e.g. 10MiB, per second)
      --parallel int             parts of a multipart upload sent at once (default 4)
      --part-size string         part size of the multipart uploads, 5MiB-5GiB (default: computed from the size)
  -r, --recursive                copy directories and prefixes
      --tags string              tags of the uploaded objects: "key1=value1&key2=value2"
      --version-id string        copy this version of the source object
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

