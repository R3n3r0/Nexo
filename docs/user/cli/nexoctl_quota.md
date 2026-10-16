# nexoctl quota

Manage the hard quota of buckets (Admin API)

## Synopsis

Manage the hard quota of buckets: writes that would exceed it are refused.
The quota is a setting of the Nexo Admin API (admin:SetBucketQuota).

```
nexoctl quota [flags]
```

## Options

```
  -h, --help   help for quota
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
* [nexoctl quota clear](nexoctl_quota_clear.md)	 - Remove the quota of a bucket
* [nexoctl quota info](nexoctl_quota_info.md)	 - Show the quota of a bucket
* [nexoctl quota set](nexoctl_quota_set.md)	 - Set the hard quota of a bucket

