## nexoctl alias set

Add or update an alias

### Synopsis

Add or update an alias. The credentials are checked against the server
(ListBuckets) before the alias is saved: an unknown, disabled or expired access
key or a wrong secret key exits with code 4 and saves nothing. Credentials the
server authenticates but may not list the buckets (a policy limited to some
buckets) are accepted, and the message says that the listing was denied.
--no-verify saves the alias without contacting the server (offline setup).

The admin and managed share commands talk to the Admin API of a Nexo server,
served by its console listener: by default the URL of the alias with the next
port (http://host:9000 -> http://host:9001), or the URL itself when it has no
port; --admin-url sets it explicitly.

```
nexoctl alias set NAME URL ACCESS_KEY SECRET_KEY [flags]
```

### Examples

```
  nexoctl alias set local http://localhost:9000 admin supersecret
  nexoctl alias set later https://s3.example.com AKIA... secret --no-verify
```

### Options

```
      --admin-url string   URL of the Admin API of a Nexo server (default: the alias URL with the next port)
      --api string         signature version (s3v4) (default "s3v4")
  -h, --help               help for set
      --no-verify          save without contacting the server
      --path string        URL style: auto, on (path-style), off (virtual-host) (default "auto")
      --region string      signing region (default us-east-1)
      --verify             check the connection and the credentials before saving (default true)
```

### Options inherited from parent commands

```
      --config-dir string   configuration directory (default $NEXOCTL_CONFIG_DIR or ~/.nexoctl)
      --debug               dump the HTTP requests and responses on stderr, with the secrets redacted
      --json                print JSON Lines (one object per line with a "status" member)
      --no-color            disable colors (also with the NO_COLOR environment variable)
  -q, --quiet               hide confirmations and progress (results and errors are still printed)
```

### SEE ALSO

* [nexoctl alias](nexoctl_alias.md)	 - Manage server aliases (saved in ~/.nexoctl/config.json)

