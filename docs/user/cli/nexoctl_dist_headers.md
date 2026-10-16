# nexoctl dist headers

Set the response headers and the CORS policy of a distribution

```
nexoctl dist headers ALIAS ID [--set 'Name: value']... [--unset NAME]... [--security] [--cors-origin O]... [--no-cors] [flags]
```

## Examples

```
  nexoctl dist headers local website --security
  nexoctl dist headers local website --set 'Cache-Control: no-cache' --cors-origin https://app.example.com --cors-method GET
```

## Options

```
      --cors-max-age int          CORS: preflight cache duration in seconds
      --cors-method stringArray   CORS: allowed method (repeatable)
      --cors-origin stringArray   CORS: allowed origin (repeatable)
  -h, --help                      help for headers
      --no-cors                   remove the CORS policy
      --security                  add the recommended security headers (HSTS, nosniff, frame options, referrer policy)
      --set stringArray           response header 'Name: value' (repeatable)
      --unset stringArray         remove a response header (repeatable)
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

* [nexoctl dist](nexoctl_dist.md)	 - Manage the distributions: custom domains of the buckets

