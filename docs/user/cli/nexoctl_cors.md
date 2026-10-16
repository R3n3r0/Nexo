# nexoctl cors

Manage the CORS configuration of buckets

## Synopsis

Manage the CORS (cross-origin resource sharing) rules of buckets: the origins,
methods and headers that browsers of other sites may use. The configuration is a
JSON document ({"rules": [{"allowedOrigins": [...], "allowedMethods": [...]}]},
the form of "cors get" and of the console; the {"CORSRules": [...]} form of the
AWS CLI is accepted too) or the S3 CORSConfiguration XML.

```
nexoctl cors [flags]
```

## Options

```
  -h, --help   help for cors
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
* [nexoctl cors get](nexoctl_cors_get.md)	 - Print the CORS configuration of a bucket (JSON or S3 XML)
* [nexoctl cors remove](nexoctl_cors_remove.md)	 - Remove the CORS configuration of a bucket
* [nexoctl cors set](nexoctl_cors_set.md)	 - Replace the CORS configuration of a bucket (JSON or S3 XML)

