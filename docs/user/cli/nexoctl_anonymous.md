# nexoctl anonymous

Manage the anonymous access of buckets and prefixes

## Synopsis

Grant anonymous (unauthenticated) access to a bucket or to a prefix with a
preset, implemented as statements of the bucket policy:

  private   no anonymous access
  download  list and read
  upload    write (and abort multipart uploads)
  public    download + upload + delete

The statements of other prefixes and the custom statements are kept.

```
nexoctl anonymous [flags]
```

## Options

```
  -h, --help   help for anonymous
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
* [nexoctl anonymous get](nexoctl_anonymous_get.md)	 - Show the anonymous access of a bucket or prefix
* [nexoctl anonymous get-json](nexoctl_anonymous_get-json.md)	 - Print the bucket policy
* [nexoctl anonymous list](nexoctl_anonymous_list.md)	 - List the prefixes of a bucket with anonymous access
* [nexoctl anonymous set](nexoctl_anonymous_set.md)	 - Set the anonymous access of a bucket or prefix
* [nexoctl anonymous set-json](nexoctl_anonymous_set-json.md)	 - Replace the bucket policy with a JSON document

