# Distributions and cache

A **distribution** exposes a bucket (or one of its prefixes) on one or more of your domains, CloudFront
style: a static site on `www.example.com`, downloads on `download.example.it`, an S3 endpoint dedicated to a
customer on `s3.customer.it`. Every domain can have its own TLS certificate and its own cache
settings; changes reach every node without restarts.

They are managed from the console (**Distributions**, with a creation wizard), with `nexoctl dist` and
`nexoctl cache`, or from the Admin API (`/api/v1/distributions`, `/api/v1/cache`). For a complete end-to-end
example follow the [two-domain static site tutorial](tutorial-static-site.md).

## Modes

| `--mode` | What it serves | Use |
|----------|----------------|-----|
| `website` (default) | GET/HEAD/OPTIONS with HTML responses: index document, SPA, error pages, redirects, headers, per-path behaviors, signed URLs | Static sites, assets, downloads |
| `s3` | The full S3 API of the bucket on the domain (SigV4 signature verified against the public host) | S3 endpoints dedicated to a customer or an application |

In `s3` mode IAM, bucket policies and the bucket's access rules apply; of the distribution only the
domains, the certificate, the restrictions and the default access (`public`, `authenticated` or `deny`)
matter. The website options (per-path behaviors, error pages, redirects, headers) are rejected when saved.

```bash
nexoctl dist create local site --bucket web --prefix /prod --mode website \
  --domain www.example.com --domain example.com --index index.html --spa /index.html
nexoctl dist create local customer --bucket customer --mode s3 --domain s3.customer.it --access authenticated
nexoctl dist list local
nexoctl dist info local site
nexoctl dist disable local site            # the domains answer 403
```

## Domains and routing

A domain is **exact** (`www.example.com`) or **wildcard** (`*.preview.example.com`) and belongs to a single
distribution. For every request Nexo looks at the `Host` header and picks, in order:

1. an exact domain of a distribution;
2. the most specific wildcard domain;
3. `<bucket>.<NEXO_DOMAIN>` (virtual-host style buckets);
4. the main S3 and console domains (`NEXO_SERVER_URL`, `NEXO_BROWSER_REDIRECT_URL`);
5. otherwise the path-style S3 API, or `421 Misdirected Request` with `--unknown-host misdirected`
   (`NEXO_UNKNOWN_HOST`); the dedicated web listeners always answer `421`.

The names the deployment answers on cannot be used as aliases: the main domains, `NEXO_DOMAIN` and the
virtual hosts of other buckets, the hosts of the node endpoints, `localhost` and the machine name (`409`). A
domain under the wildcard of another distribution (`app.example.com` under `*.example.com`) also requires
permissions on that distribution's bucket (`403` otherwise).

```console
$ nexoctl dist domain add local site '*.preview.example.com'
$ nexoctl dist resolve local www.example.it
www.example.it: distribution `site` (alias www.example.it, website), bucket site
```

Distributions answer on two **dedicated listeners**, when configured: `--web-address` (`NEXO_WEB_ADDRESS`,
HTTP and ACME HTTP-01 challenges) and `--web-tls-address` (`NEXO_WEB_TLS_ADDRESS`, HTTPS with the
distributions' certificates chosen by SNI), useful to expose the sites on ports 80/443 without exposing the
S3 API. On the S3 port (9000) they answer according to `--s3-distributions` (`NEXO_S3_DISTRIBUTIONS`): with
`auto` (default) only if the deployment declares the names of the S3 endpoint with `NEXO_SERVER_URL` or
`NEXO_DOMAIN` (as in the configurations of
[`deploy/proxy`](https://github.com/R3n3r0/Nexo/tree/main/deploy/proxy)), with `on` always, with `off` never.
Without declared names the S3 port answers any name that reaches it (load balancers, DNS aliases, service
names): an alias could coincide with the name the S3 clients use. On the S3 port the `/nexo/` paths (health
probes, metrics, RPC between nodes) always belong to the server, whatever the domain.

If a distribution prevents you from using the deployment, start the nodes with `--no-distributions`
(`NEXO_NO_DISTRIBUTIONS=true`): no domain is routed, the Admin API works and the distribution can be
fixed or removed (`nexoctl dist remove`); then restart without the option.

## Behaviors and access

The **default behavior** applies to every path; additional **behaviors** change the behavior of the
paths that match a pattern (`--prefix`, `--glob`, `--regex`, the first match wins) and
inherit the rest from the default.

| `--access` | Who can read |
|------------|--------------|
| `public` | anyone, except for the `deny` access rules and the block public access of the bucket or the cluster |
| `signed` | only with URLs or cookies signed by the keys of a key group (`--key-group`) |
| `authenticated` | only with S3 credentials (SigV4) or the console session |
| `deny` | nobody (403) |

```bash
nexoctl dist behavior add local site --glob '/assets/**' --cache-control 'public, max-age=31536000, immutable'
nexoctl dist behavior add local site --regex '^/downloads/.*\.zip$' --access signed --key-group releases
nexoctl dist behavior add local site --prefix /internal/ --access deny --position 1
nexoctl dist behavior list local site
nexoctl dist test local site /downloads/v1.zip         # which behavior, access and headers
```

A `public` behavior of a `website` distribution answers `403` as long as the
[block public access](../security/sharing.md#public-access-rules-and-block-public-access) of the
bucket or the cluster is on. The origin bucket can stay private: the distribution reads the objects on
behalf of the visitors.

## Website features

| Feature | Command |
|---------|---------|
| Index document | `--index index.html` on `create`, `edit` or a behavior |
| Single page application | `--spa /index.html`: serves that file (200) in place of missing objects |
| Error pages | `nexoctl dist error-page set local site 404 /404.html` (`--response-status` to change the status) |
| Redirects | `dist redirect add` with `--https-only`, `--host-from example.com --to-host www.example.com` or `--from '^/old/(.*)$' --to '/new/$1'` (`--status 301/302/303/307/308`) |
| Response headers | `dist headers --set 'Name: value'`, `--unset`, `--security` (HSTS, nosniff, frame options, referrer policy) |
| CORS | `dist headers --cors-origin https://app.example.com --cors-method GET` |
| Access log | `logging: {accessLog: true, format: combined}` in the YAML of the distribution (`json` by default: a `distribution access` record in the server log, with `event_time` the start of the request and `time` the time of the record) |

## Signed URLs and cookies

For `--access signed` behaviors Nexo verifies URLs in the CloudFront format (`?Expires=&Signature=&Key-Pair-Id=`,
or `?Policy=` for a policy with a wildcard resource, IP and start date) and the `CloudFront-*` cookies, with
Ed25519 or RSA keys collected in **key groups**:

```bash
nexoctl dist keygroup create local releases                   # generates the key, saves the private one
nexoctl dist keygroup key add local releases --public-key my.pub.pem   # or upload your own public key
nexoctl dist sign-url local site /downloads/v1.zip --expire 1h --ip 203.0.113.0/24
nexoctl dist sign-url local site /premium/ --expire 12h --cookie --resource 'https://www.example.com/premium/*'
```

The signature is verified before the cache and is not part of the cache key. Several keys in the same key
group allow rotating them without interruptions. Server-side signing is also available with
`POST /api/v1/distributions/{id}/sign` (permission `admin:SignDistributionURL`) and from the console.

## Certificates

| Command | Certificate |
|---------|-------------|
| `nexoctl dist cert set local ID --auto` | obtained and renewed through ACME (Let's Encrypt by default) for all the exact domains |
| `nexoctl dist cert set local ID --cert crt.pem --key key.pem` | uploaded by you (PEM chain, leaf first) |
| `nexoctl dist cert set local ID --none` | none: TLS terminated by a proxy |

ACME configuration of the server: `NEXO_ACME_EMAIL`, `NEXO_ACME_DIRECTORY` (default Let's Encrypt),
`NEXO_ACME_CHALLENGE` (`http-01` on the web listener, `tls-alpn-01` on the web TLS listener), `NEXO_ACME_CA_CERT`
for a private ACME CA. The certificates are stored encrypted in the deployment, distributed to every node and
presented by SNI; in a cluster the renewal is performed by a single node. `nexoctl dist cert list` shows status
and expiry; the metric `nexo_tls_certificate_expiry_seconds{certificate}` feeds the alerts. Wildcard domains
require an uploaded certificate.

## Behind one or more proxies

- `NEXO_TRUSTED_PROXIES`: only from these addresses does Nexo accept `X-Forwarded-For/Proto` (or `Forwarded`,
  with `NEXO_FORWARDED_HEADERS=forwarded`) and the **PROXY protocol** on the listeners listed in
  `NEXO_PROXY_PROTOCOL` (`s3`, `console`, `web`). The real client IP feeds the restrictions, the access rules
  and the signed URLs with `--ip`.
- The `Host` must arrive unchanged; `NEXO_TRUST_FORWARDED_HOST=true` only for proxies that rewrite it.
- **Origin secret**: every proxy adds a secret header; requests that reach the nodes without it
  (bypassing the proxy) get `403`.

  ```bash
  nexoctl dist origin-secret rotate local site              # prints the new secret only once
  nexoctl dist restrict local site --origin-secret-header X-Origin-Secret
  ```

  With several proxies, one secret per proxy as a `${secret:proxy-a}` reference in the YAML, resolved on every
  node from `NEXO_SECRET_PROXY_A`.
- **Restrictions by address**: `nexoctl dist restrict local site --deny-ip 203.0.113.0/24` (or
  `--allow-ip`). `--rate-limit` is saved but not applied yet: limit the requests on the proxy.

Ready-made configurations for nginx, Caddy, Traefik, HAProxy and Cloudflare:
[`deploy/proxy`](https://github.com/R3n3r0/Nexo/tree/main/deploy/proxy).

## Cache

Nexo keeps the responses of the distributions in an in-memory cache (`NEXO_CACHE_MEMORY`, default 256 MiB)
and, with `NEXO_CACHE_DIR`, on disk (`NEXO_CACHE_SIZE`, preferably an SSD separate from the data drives).
The `X-Cache` header tells how each response was served:

| `X-Cache` | Meaning |
|-----------|---------|
| `HIT` | from the cache, fresh |
| `MISS` | from the origin (and cached if possible) |
| `STALE` | expired copy served while it is refreshed or because the origin does not answer |
| `EXPIRED` | expired copy read again from the origin |
| `REVALIDATED` | expired copy confirmed by the origin (same ETag) |
| `BYPASS` | cache skipped (cache disabled, request with credentials, bypass cookie or header) |

### Levels and policies

Settings are inherited: **cluster** (`nexoctl cache settings`) → **distribution** → **domain**
(`--domain`) → **path** (`--behavior`). Every level can choose a **policy** and change single fields of it.

| Built-in policy | TTL | Use |
|-----------------|-----|-----|
| `standard` | 1h (maximum 7d), honors the objects' `Cache-Control` | default |
| `aggressive` | 1d (minimum 1h, maximum 30d), long stale windows | content that rarely changes |
| `immutable-assets` | 365d, in browsers too | assets with a hash in the name |
| `respect-origin` | only what the objects declare | control left to the publisher |
| `disabled` | no cache | always-fresh content |

```bash
nexoctl dist cache set local site --policy standard
nexoctl dist cache set local site --domain www.example.com --policy aggressive --quota 20GiB
nexoctl dist cache set local site --domain '*.preview.example.com' --disable
nexoctl dist cache set local site --behavior '/assets/**' --ttl-default 365d
nexoctl dist cache set local site --behavior '/api/' --behavior-type prefix --disable
nexoctl cache policy create local long --ttl-default 1d --ttl-max 30d --query allowlist:v --stale-while-revalidate 5m
nexoctl dist cache show local site
```

Other useful fields: `--query` (query parameters in the key), `--key-header`/`--key-cookie`,
`--bypass-cookie`, `--negative-ttl` (404/403), `--stale-if-error`, `--compression`, `--range-caching` (video and
large files in chunks), `--warm-on-write`. Responses to authenticated requests are never shared between
different users.

### Invalidation

- **Automatic** (default): when an object changes (PUT, DELETE, copy, metadata, tags) its entries are
  discarded on every domain and every node.
- **Manual**, by exact path, glob, prefix, regex, tag, domain or everything, with `--soft` to mark entries
  as expired instead of deleting them:

  ```bash
  nexoctl dist invalidate local site '/assets/**' --wait
  nexoctl cache purge local site --tag product-42           # objects with x-amz-meta-cache-tag: product-42
  nexoctl cache purge local site --domain static.example.it --all
  nexoctl cache purge list local site
  ```

- **CDN in front of Nexo**: the purge webhooks (`nexoctl cache webhook add`, types `cloudflare`, `fastly`,
  `akamai`, `nginx`, `varnish`, `generic`) propagate every invalidation, with retries and a persistent queue.
  The destinations must be public addresses, except for the networks in `NEXO_CACHE_WEBHOOK_ALLOW`.

Invalidations and warm-ups require only `admin:InvalidateCache`: a deploy pipeline needs no
other permissions.

### Warm-up, diagnostics, statistics

```bash
nexoctl cache warm local site --prefix /assets/ --bandwidth 20MiB --wait
nexoctl cache inspect local https://www.example.com/assets/app.js    # key, policy and level, status on every node
nexoctl cache stats local                                            # hit ratio, bytes, top URLs per domain
nexoctl cache settings local --node-header                           # adds X-Cache-Node to the responses
```

With `nexoctl cache settings local --debug-secret-file debug.secret`, requests with the header
`X-Nexo-Cache-Debug: <secret>` also get `X-Cache-Key` and `X-Cache-Policy`.

### In a cluster

`NEXO_CACHE_MODE` chooses how the nodes share the cache:

| Mode | Behavior |
|------|----------|
| `local` (default) | every node has its own cache |
| `shared` | every key has an owner node (consistent hashing): a single copy in the cluster, capacity adds up |
| `shared+local` | like `shared` plus a small local cache for the most requested objects |

## Permissions

| Permission | For |
|------------|-----|
| `admin:ManageDistributions` | creating and changing distributions and key groups |
| `admin:ManageCertificates` | uploading and removing certificates |
| `admin:SignDistributionURL` | signing URLs and cookies server side |
| `admin:ManageCache` | cache settings, policies and webhooks |
| `admin:InvalidateCache` | invalidations and warm-ups |

## Metrics

`nexo_distribution_requests_total{distribution,host,status}`, `nexo_distribution_bytes_served_total`,
`nexo_distribution_request_duration_seconds`, `nexo_cache_requests_total{distribution,host,result}`,
`nexo_cache_hit_ratio`, `nexo_cache_bytes_served_total{source}`, `nexo_cache_size_bytes`,
`nexo_cache_invalidations_total`, `nexo_cache_webhook_calls_total`. See [Monitoring](../operations/monitoring.md).

## GitOps

Distributions, cache settings and policies are exported and imported as YAML:

```bash
nexoctl dist export local > distributions.yaml
nexoctl dist import local distributions.yaml
nexoctl dist cache export local site > site-cache.yaml
nexoctl dist cache import local site-cache.yaml
nexoctl cache policy export local > policies.yaml
nexoctl cache policy import local policies.yaml
```

Secrets (origin secrets, webhook tokens) are written as `${secret:name}` references, resolved on the nodes
from the `NEXO_SECRET_*` variables: the YAML files contain no secrets.
