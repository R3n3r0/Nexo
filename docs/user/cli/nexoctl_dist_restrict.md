# nexoctl dist restrict

Restrict the clients of a distribution (source IP, origin secret, rate limit)

## Synopsis

Restrict the clients of a distribution: --allow-ip/--deny-ip filter the client
behind the trusted proxies; --origin-secret-header requires the origin secret of
the proxies in that header (generate the secret with "nexoctl dist origin-secret
rotate"); --rate-limit admits N requests per second on each node (--burst at
once), per client address or shared by the whole distribution
(--rate-limit-per): beyond it a website answers 429, an s3 distribution 503
SlowDown, with Retry-After; --no-rate-limit removes it.

```
nexoctl dist restrict ALIAS ID [--allow-ip CIDR]... [--deny-ip CIDR]... [--origin-secret-header NAME] [--rate-limit N [--burst N] [--rate-limit-per ip|distribution]] [flags]
```

## Examples

```
  nexoctl dist restrict local website --deny-ip 203.0.113.0/24
  nexoctl dist restrict local website --origin-secret-header X-Origin-Secret
  nexoctl dist restrict local website --rate-limit 50 --burst 100
```

## Options

```
      --allow-ip stringArray          allow only the clients of this CIDR (repeatable)
      --burst int                     requests admitted at once above the rate (0: the rate)
      --clear-ip                      remove the source IP restrictions
      --deny-ip stringArray           refuse the clients of this CIDR (repeatable)
  -h, --help                          help for restrict
      --no-origin-secret              remove the origin secret requirement
      --no-rate-limit                 remove the rate limit
      --origin-secret-header string   require the origin secret in this header
      --rate-limit int                requests per second admitted by each node (0: no limit)
      --rate-limit-per string         rate limit per client address (ip) or for the whole distribution (distribution)
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

