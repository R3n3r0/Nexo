## nexoctl access public

Apply and show the public access presets of a bucket

```
nexoctl access public [flags]
```

### Options

```
  -h, --help   help for public
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

* [nexoctl access](nexoctl_access.md)	 - Manage the access rules of the buckets (prefix, glob, regex)
* [nexoctl access public get](nexoctl_access_public_get.md)	 - Show whether a bucket is public through its access rules
* [nexoctl access public set](nexoctl_access_public_set.md)	 - Apply a public access preset to a bucket

