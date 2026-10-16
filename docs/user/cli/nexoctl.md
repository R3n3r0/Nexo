# nexoctl

Nexo command-line client

## Synopsis

nexoctl manages Nexo servers and any S3-compatible endpoint: aliases,
buckets, objects, sharing, anonymous access and administration.

Remote locations are written ALIAS[/BUCKET[/PREFIX]] (see "nexoctl alias");
anything else is a local path.

## Options

```
      --config-dir string   configuration directory (default $NEXOCTL_CONFIG_DIR or ~/.nexoctl)
      --debug               dump the HTTP requests and responses on stderr, with the secrets redacted
  -h, --help                help for nexoctl
      --json                print JSON Lines (one object per line with a "status" member)
      --no-color            disable colors (also with the NO_COLOR environment variable)
  -q, --quiet               hide confirmations and progress (results and errors are still printed)
```

## SEE ALSO

* [nexoctl access](nexoctl_access.md)	 - Manage the access rules of the buckets (prefix, glob, regex)
* [nexoctl admin](nexoctl_admin.md)	 - Administer a Nexo server (Admin API)
* [nexoctl alias](nexoctl_alias.md)	 - Manage server aliases (saved in ~/.nexoctl/config.json)
* [nexoctl anonymous](nexoctl_anonymous.md)	 - Manage the anonymous access of buckets and prefixes
* [nexoctl cache](nexoctl_cache.md)	 - Manage the cache of the distributions
* [nexoctl cat](nexoctl_cat.md)	 - Print the content of objects and files
* [nexoctl completion](nexoctl_completion.md)	 - Generate the autocompletion script for the specified shell
* [nexoctl cors](nexoctl_cors.md)	 - Manage the CORS configuration of buckets
* [nexoctl cp](nexoctl_cp.md)	 - Copy files and objects
* [nexoctl diff](nexoctl_diff.md)	 - List the differences between two directories or prefixes
* [nexoctl dist](nexoctl_dist.md)	 - Manage the distributions: custom domains of the buckets
* [nexoctl du](nexoctl_du.md)	 - Summarize the disk usage of prefixes and directories
* [nexoctl encrypt](nexoctl_encrypt.md)	 - Manage the default encryption of buckets
* [nexoctl event](nexoctl_event.md)	 - Manage the event notifications of buckets
* [nexoctl find](nexoctl_find.md)	 - Find objects and files by name, size and age
* [nexoctl head](nexoctl_head.md)	 - Print the first lines of objects and files
* [nexoctl ilm](nexoctl_ilm.md)	 - Manage the lifecycle rules of buckets (expiration, noncurrent versions, uploads)
* [nexoctl legalhold](nexoctl_legalhold.md)	 - Manage the Object Lock legal hold of objects
* [nexoctl ls](nexoctl_ls.md)	 - List buckets, objects and object versions
* [nexoctl mb](nexoctl_mb.md)	 - Make buckets
* [nexoctl mirror](nexoctl_mirror.md)	 - Synchronize a directory or prefix to another one
* [nexoctl mv](nexoctl_mv.md)	 - Move files and objects
* [nexoctl pipe](nexoctl_pipe.md)	 - Stream the standard input to an object
* [nexoctl presign](nexoctl_presign.md)	 - Generate a raw presigned URL for any method
* [nexoctl put](nexoctl_put.md)	 - Upload local files
* [nexoctl quota](nexoctl_quota.md)	 - Manage the hard quota of buckets (Admin API)
* [nexoctl rb](nexoctl_rb.md)	 - Remove buckets
* [nexoctl retention](nexoctl_retention.md)	 - Manage the Object Lock retention of objects and buckets
* [nexoctl rm](nexoctl_rm.md)	 - Remove objects, object versions and incomplete uploads
* [nexoctl share](nexoctl_share.md)	 - Share objects with links that need no credentials
* [nexoctl stat](nexoctl_stat.md)	 - Show the metadata of objects, buckets and files
* [nexoctl tag](nexoctl_tag.md)	 - Manage the tags of buckets and objects
* [nexoctl tree](nexoctl_tree.md)	 - Show the objects and prefixes as a tree
* [nexoctl version](nexoctl_version.md)	 - Print the nexoctl version, or manage bucket versioning

