## nexoctl dist

Manage the distributions: custom domains of the buckets

### Synopsis

Manage the distributions of a Nexo server (Admin API, admin:ManageDistributions):
a bucket, or a prefix of it, exposed on custom domains as a website (index
document, SPA fallback, error pages, redirects, behaviors per path, signed URLs)
or as an S3 endpoint. The changes reach every node of a cluster without restart.

Every change reads the distribution, changes it and writes it back guarded by
its ETag: a concurrent change is retried. Point the DNS of the domains (CNAME,
A/AAAA) to the cluster or to the proxies in front of it.

```
nexoctl dist [flags]
```

### Options

```
  -h, --help   help for dist
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

* [nexoctl](nexoctl.md)	 - Nexo command-line client
* [nexoctl dist behavior](nexoctl_dist_behavior.md)	 - Manage the behaviors per path of a distribution
* [nexoctl dist cache](nexoctl_dist_cache.md)	 - Manage the cache settings of a distribution, its domains and paths
* [nexoctl dist cert](nexoctl_dist_cert.md)	 - Manage the TLS certificates of the domains of the distributions
* [nexoctl dist create](nexoctl_dist_create.md)	 - Create a distribution
* [nexoctl dist disable](nexoctl_dist_disable.md)	 - Disable a distribution (its domains answer 403)
* [nexoctl dist domain](nexoctl_dist_domain.md)	 - Add and remove the domains (aliases) of a distribution
* [nexoctl dist edit](nexoctl_dist_edit.md)	 - Change the origin, the mode or the default behavior of a distribution
* [nexoctl dist enable](nexoctl_dist_enable.md)	 - Enable a distribution
* [nexoctl dist error-page](nexoctl_dist_error-page.md)	 - Replace the error answers of a distribution with pages
* [nexoctl dist export](nexoctl_dist_export.md)	 - Export distributions as YAML
* [nexoctl dist headers](nexoctl_dist_headers.md)	 - Set the response headers and the CORS policy of a distribution
* [nexoctl dist import](nexoctl_dist_import.md)	 - Create or replace the distributions of a YAML file
* [nexoctl dist info](nexoctl_dist_info.md)	 - Show a distribution
* [nexoctl dist invalidate](nexoctl_dist_invalidate.md)	 - Invalidate cached content of a distribution
* [nexoctl dist keygroup](nexoctl_dist_keygroup.md)	 - Manage the key groups of the signed URLs and cookies
* [nexoctl dist list](nexoctl_dist_list.md)	 - List the distributions
* [nexoctl dist origin-secret](nexoctl_dist_origin-secret.md)	 - Manage the origin secret of the proxies in front of the cluster
* [nexoctl dist redirect](nexoctl_dist_redirect.md)	 - Manage the redirections of a distribution
* [nexoctl dist remove](nexoctl_dist_remove.md)	 - Remove a distribution
* [nexoctl dist resolve](nexoctl_dist_resolve.md)	 - Show what serves a host (distribution, virtual host, S3 API)
* [nexoctl dist restrict](nexoctl_dist_restrict.md)	 - Restrict the clients of a distribution (source IP, origin secret, rate limit)
* [nexoctl dist sign-url](nexoctl_dist_sign-url.md)	 - Sign a URL (or cookies) of a signed behavior
* [nexoctl dist test](nexoctl_dist_test.md)	 - Show how a distribution serves a URL: behavior, access, headers

