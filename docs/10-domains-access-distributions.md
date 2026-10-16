# 10. Access rules (wildcard/regex), domains and distributions

Goal: define **who accesses what** with glob patterns and **regular expressions**, make buckets or parts of buckets
public, and associate **several domains** with a bucket through CloudFront-style **distributions**. Everything must
be configurable from the **dashboard, CLI and Admin API**, at runtime, in single-node and cluster mode.

## 10.1 Access rules

AWS-format policies (see [09](09-presigned-policy-sharing.md)) only support the `*` and `?` wildcards.
Nexo adds a layer of **per-bucket access rules**, simpler to use and with regex support.
The rules are evaluated by the **same policy engine** as an additional resource-based source.

### Model
```yaml
bucket: media
rules:
  - id: public-images
    priority: 10                 # evaluation order (lower = first)
    effect: allow                # allow | deny
    principals: ["*"]            # "*" = anonymous/everyone, user:mario, group:marketing, accesskey:AK..., distribution:website
    actions: [read]              # read (GET/HEAD), list, write (PUT/POST/multipart), delete, tag, all; or explicit s3:* actions
    match:
      type: regex                # prefix | glob | regex
      pattern: '^img/[0-9]{4}/.*\.(jpe?g|png|webp)$'
      caseInsensitive: true
    conditions:                  # optional
      sourceIp: ["10.0.0.0/8", "2001:db8::/32"]
      notSourceIp: []
      referer: ["https://*.example.com/*"]
      host: ["cdn.example.com"]  # domain the request arrived on
      secureTransport: true
      timeWindow: { from: "2026-01-01T00:00:00Z", to: "2026-12-31T23:59:59Z" }
      methods: [GET, HEAD]
      headers: { "X-Origin-Secret": "${secret:proxy-token}" }
  - id: block-private
    priority: 5
    effect: deny
    principals: ["*"]
    actions: [all]
    match: { type: glob, pattern: "**/private/**" }
```

### Pattern types
| Type | Syntax | Example | Notes |
|------|----------|---------|------|
| `prefix` | literal prefix | `public/` | Fastest (trie lookup) |
| `glob` | `*` (one segment), `**` (several segments), `?`, `[abc]`, `{a,b}` | `docs/**/*.pdf`, `{img,video}/*` | Compiled into an anchored regex |
| `regex` | **RE2** (Go's `regexp`) | `^users/[a-z0-9_-]+/avatar\.png$` | Guaranteed linear time: **no ReDoS** |

Safety rules for regexes:
- RE2 only (no backreferences/lookaround), maximum length 1024 characters, at most 200 rules per bucket (configurable);
- validation and compilation on save (error with position), cache of the compiled regexes per bucket;
- matching on the **decoded key** (URL decoding before the match, configurable Unicode NFC normalization),
  so `%2e%2e/` or double encodings do not bypass the rules;
- named groups usable in conditions (e.g. `(?P<user>[^/]+)` compared with `${aws:username}`):
  ```yaml
  match: { type: regex, pattern: '^home/(?P<user>[^/]+)/' }
  conditions: { captureEquals: { user: "${aws:username}" } }
  ```

### Evaluation
1. Collection of all sources: the identity's IAM policies, bucket policy, **access rules**, distribution rules (§10.2).
2. An applicable `deny` in any source → **deny** (rules cannot override object lock or the limits of service accounts' inline policies).
3. Otherwise an `allow` from at least one source → allow.
4. Otherwise deny (deny-by-default).
5. Within the access rules, for equal effect the `priority` decides; the simulator's response names the deciding rule.

### "Public" shortcuts
| Preset | Effect |
|--------|---------|
| `private` | No anonymous rule |
| `public-read` | `allow * read` on the whole bucket |
| `public-read-list` | `read` + `list` |
| `public-prefix` | `read` on a chosen prefix/glob/regex |
| `public-write` | Anonymous `write` (requires explicit confirmation, with a warning in the dashboard) |

### Global protections
- **Block public access** at cluster and bucket level (as in AWS): when active, rules with `principals: ["*"]` are
  rejected on save or ignored during evaluation (configurable choice), with a warning in the dashboard.
- Audit of every change to the rules (who, when, diff).

## 10.2 Distributions (CloudFront style)

A **distribution** exposes a bucket (or a prefix of a bucket) on one or more **custom domains**, with per-path
behaviors. Several proxies/access points can point to the same cluster using different domains or the same
distribution.

```
 users  ─▶ cdn.example.com ─┐                     ┌─ distribution "website" ─▶ bucket web, prefix /prod
 users  ─▶ www.example.com ─┼─▶ proxy A ─┐        │      behaviors: /api/* deny, /assets/* cache 1y, /* SPA
 users  ─▶ static.example.it ─▶ proxy B ─┼─▶ Nexo ─┤
 S3 SDK ─▶ s3.example.com ─────────────── ┘        └─ distribution "download" ─▶ bucket releases, signed URL
```

### Model
```yaml
id: website
enabled: true
aliases:                       # several domains for the same distribution
  - www.example.com
  - example.com
  - "*.preview.example.com"    # subdomain wildcard
origin:
  bucket: web
  pathPrefix: /prod            # optional: the distribution only sees this prefix
  versionPinning: false
mode: website                  # website | s3   (see below)
defaultBehavior:
  access: public               # public | signed | authenticated | deny
  allowedMethods: [GET, HEAD, OPTIONS]
  indexDocument: index.html
  spaFallback: /index.html     # returns index.html for paths not found (SPA)
  cache: { cacheControl: "public, max-age=300" }
behaviors:                     # evaluated in order; first match wins
  - pathPattern: { type: glob, pattern: "/assets/**" }
    cache: { cacheControl: "public, max-age=31536000, immutable" }
  - pathPattern: { type: regex, pattern: '^/downloads/.*\.(zip|iso)$' }
    access: signed             # requires a URL/cookie signed by the distribution
    signing: { keyGroup: releases }
  - pathPattern: { type: prefix, pattern: "/internal/" }
    access: deny
errorPages:
  - { status: 404, path: /404.html, responseStatus: 404 }
  - { status: 403, path: /404.html, responseStatus: 404 }   # hides the existence of objects
redirects:
  - { from: '^/old/(.*)$', to: '/new/$1', status: 301 }
  - { hostFrom: example.com, toHost: www.example.com, status: 301 }
  - { httpsOnly: true }        # HTTP → HTTPS redirect
headers:
  response:
    Strict-Transport-Security: "max-age=63072000; includeSubDomains"
    X-Content-Type-Options: nosniff
  cors: { allowedOrigins: ["https://app.example.com"], allowedMethods: [GET], maxAge: 3600 }
restrictions:
  sourceIp: { allow: [], deny: ["203.0.113.0/24"] }
  originSecretHeader: { name: X-Origin-Secret, value: "${secret:proxy-a}" }  # only accepts traffic from the proxy
  rateLimit: { requestsPerSecond: 200, burst: 400, per: ip }                 # P2
tls:
  certificate: auto            # auto (ACME/Let's Encrypt) | upload:<id> | none (TLS terminated by the proxy)
logging: { accessLog: true, target: webhook:audit }
```

### Distribution modes
| `mode` | Behavior | Use |
|--------|---------------|-----|
| `website` | Only GET/HEAD/OPTIONS (POST/PUT if the behavior allows them with `signed`), HTML/plain responses, index document, error pages, SPA fallback, redirects, no S3 XML | Static sites, assets, public downloads |
| `s3` | Full S3 API on the custom domain: the domain is an alias of the bucket (e.g. `s3.customer.example` → bucket `customer`), SigV4 signature verified with the public host; the generated presigned URLs use that domain | Dedicated S3 endpoints per customer/application |

### Content access
| `access` | Description |
|----------|-------------|
| `public` | Anonymous, but always subject to the `deny`s of the access rules and to block public access |
| `signed` | **Distribution-signed URLs** (CloudFront-style `?Expires=&Signature=&Key-Pair-Id=`, Ed25519/RSA) or **signed cookies** for a path/glob: useful for premium content spread over several files. "Custom" policy with a wildcard resource, start/end date, IP |
| `authenticated` | Requires S3 credentials (SigV4) or a console session |
| `deny` | Always 403/404 |

Access rules of the two modes (implementation):
- **website**: a `public` behavior is rejected (403) while the bucket's (or the cluster's) block public access is
  active, **whatever grants the read** (an access rule for `distribution:<id>`, a public bucket policy): the
  guardrail comes before the evaluation of the rules. The flag is read from the in-memory cache of the bucket
  metadata (invalidated by the Notifier), never with I/O on the path of each request; a decision that cannot be made
  (set without quorum, metadata never read) answers **503 + `Retry-After`**, never 403. An explicit `deny` (access
  rule, bucket policy) always rejects; an `authenticated` behavior requires an allow.
- **s3**: the distribution is the bucket's whole S3 API on the domain, governed by IAM, bucket policy and access rules
  (a private prefix is protected with a `deny` rule). Of the behavior, only `defaultBehavior.access` applies:
  `public` (default), `authenticated` (no anonymous requests) or `deny`, applied after SigV4 authentication.
  Per-path behaviors, `signed`, `allowedMethods`, index/SPA, error pages, redirects, response headers, CORS (the
  bucket's CORS configuration applies) and the behavior's cache settings are **rejected on save** (400): nothing is
  accepted and then ignored, and `dist test` reports the same decision as real requests. Admission (distribution
  enabled, address, origin secret) also comes before signature verification: a client that bypasses the proxy gets
  the admission 403, never the SigV4 errors.
- Regex redirects expand the captures of the **encoded path**; a target that the browser would take off-site
  (`//host`, `/\host`, control characters, backslashes) is not followed. The index document probe of a
  "directory" (`/dir` → `/dir/`) is authorized like the read of the index: a denied object does not produce the 302.
- CORS: `allowCredentials: true` with a `*` (or wildcard) origin is rejected on save; the origin is never reflected
  for wildcard rules.
- `If-Range` (RFC 9110 §13.1.5): the range applies only if the validator matches the strong ETag (a weak ETag never
  matches) or the exact last-modified date; otherwise `200` with the whole object.

Signing keys: **key groups** with several keys (rotation without downtime), private key generated by the dashboard
and downloadable only once, or a public key uploaded by the user. URL generation from the CLI/dashboard/SDK.

### Host resolution (routing)
For every request, the `Host` header (or the `X-Forwarded-Host` / `Forwarded host=` of a trusted proxy, only with
`--trust-forwarded-host` / `NEXO_TRUST_FORWARDED_HOST=true`) is resolved in this order:
1. exact alias of a distribution;
2. wildcard alias of a distribution (most specific match);
3. `<bucket>.<NEXO_DOMAIN>` (S3 virtual-host);
4. main S3/console domain (`NEXO_SERVER_URL`, `NEXO_BROWSER_REDIRECT_URL`);
5. otherwise: path-style S3 API (configurable: or 421 Misdirected Request).

- In-memory routing table (map + trie for wildcards), updated **at runtime** and propagated to all nodes via peer RPC.
- Alias conflicts (the same domain on two distributions) are rejected on save.
- **Reserved domains**: an alias (exact or wildcard) that matches or covers the names the deployment answers on is
  rejected (409): the main domains (`NEXO_SERVER_URL`, `NEXO_BROWSER_REDIRECT_URL`), a `NEXO_DOMAIN` or the
  `<bucket>.<NEXO_DOMAIN>` virtual hosts, the hosts of the nodes' endpoints (the RPC calls and the probes between
  peers), the hosts of `--address`/`--console-address`, `localhost` and the machine name (also without a domain); IP
  addresses are never valid aliases. Only the virtual host of the distribution's own origin bucket is allowed (and
  whoever saves it must be authorized on that bucket). The aliases of distributions saved before this rule that
  violate it are left out of the routing table on load (warning log), so an alias never hijacks the S3 endpoint, the
  cluster or other people's buckets.
- **Wildcards of other distributions**: an exact alias or a more specific wildcard that falls under the wildcard of
  another distribution (`app.corp.example` under `*.corp.example`) wins the routing and takes those hosts away from
  it: whoever saves it must also have `admin:ManageDistributions` on that distribution's origin bucket (403
  otherwise).
- **Reserved paths**: on the S3 listener the `/nexo/` paths (health probes, metrics, internode RPC) are never routed
  to a distribution, whatever the host: a load balancer or a peer that reaches the node with a name also used as an
  alias always gets the server. Only the `NEXO_DOMAIN` virtual hosts treat them as keys of their bucket. A site's
  `nexo/...` objects are served by the web listeners.
- Listeners: distributions answer on the dedicated listeners `--web-address :80` and `--web-tls-address :443`
  (distributions only, `421` for any other host) and on the S3 port (9000) according to `--s3-distributions`
  (`NEXO_S3_DISTRIBUTIONS`): `auto` (default) only when the deployment declares the names of the S3 endpoint
  (`NEXO_SERVER_URL` or `NEXO_DOMAIN`, which become reserved), `on` always (explicit opt-in: the operator guarantees
  that S3 clients use no other names), `off` never. The S3 port answers to every name that reaches it (load
  balancers, Kubernetes service names, DNS aliases): without declared names nothing tells an alias apart from a name
  used by S3 clients.
- **Recovery**: `nexo server --no-distributions` (`NEXO_NO_DISTRIBUTIONS=true`) routes no host to a distribution on
  any listener, while the distributions stay readable and editable from the Admin API: start the nodes this way to
  remove or fix a distribution that prevents the deployment from working, then restart them without the option.

### Behind one or more proxies
- `NEXO_TRUSTED_PROXIES` (CIDR): `X-Forwarded-For/Proto/Host`, `Forwarded` (RFC 7239) and **PROXY protocol v1/v2**
  (enabled per listener) are accepted only from these addresses.
- **Origin secret header** per distribution: each proxy sends a different secret header; requests without the header
  (direct access bypassing the proxy) are rejected. Several secrets can be active at the same time for rotation.
- The client IP used in conditions (`sourceIp`) is the real one derived from the trusted headers.
- Configuration examples for nginx, Caddy, Traefik, HAProxy and Cloudflare in `deploy/proxy/`.

Implementation choices (verified by `test/proxy`, see §10.6 "Cluster and Docker"):
- **Forwarded host not trusted by default**: even from a trusted proxy the forwarded address and scheme are used, but
  the domain stays the `Host` header (which all the `deploy/proxy` configurations preserve). Some proxies (Cloudflare,
  an nginx `location` that does not set it) forward the client's `X-Forwarded-Host` as is: a client could choose the
  distribution, the `nexo:host` conditions and the target of `httpsOnly` redirects. `--trust-forwarded-host` is only
  for proxies that rewrite the `Host`.
- **A single header family** per deployment (`NEXO_FORWARDED_HEADERS`: `x-forwarded`, the default, or `forwarded`):
  reading both would let a client forge its address through a proxy that sets only the other one.
  `X-Forwarded-For` is read from the right up to the first untrusted address.
- **PROXY protocol per listener** (`NEXO_PROXY_PROTOCOL=s3,console,web`), read before TLS: used only from peers in
  `NEXO_TRUSTED_PROXIES`, read and ignored from the others, optional even for trusted proxies (so a proxy with
  `X-Forwarded-*`, the internode RPCs and the health checks share the S3 port with an HAProxy
  `send-proxy-v2-ssl`); the SSL TLV makes the request HTTPS (`httpsOnly` redirects, `aws:SecureTransport`).
- **Origin secret**: `restrictions.originSecretHeader` has a name (default `X-Origin-Secret`) and up to 8 values
  active together, all accepted from all the distribution's proxies. A plain-text value is stored as a hash
  (`sha256:`); a `${secret:<name>}` reference is resolved on every node from `NEXO_SECRET_<NAME>` (or `_FILE`, with
  `-` and `_` equivalent) and never ends up in the configuration: it is the recommended form with several proxies
  (`values: ["${secret:proxy-a}", "${secret:proxy-b}"]`, one secret per proxy, secret-free YAML for GitOps). An
  unresolvable reference accepts no request (fail closed). `dist origin-secret rotate` generates a new value (shown
  once) while keeping the previous ones active (`--keep`). The check applies in both modes (website and s3) and comes
  before any other evaluation; path-style S3 requests and the console on the main domains are not distributions and
  do not require it (they are protected by the network).
- Distribution URL signing (`dist sign-url`, `POST /distributions/{id}/sign`) and `dist test` report the path's
  behavior even when the test request would be rejected by admission (origin secret, address, disabled
  distribution): signing for a distribution behind a proxy finds its key group.
- A cluster node that is stopping answers `503` until the health checks take it out of rotation: the proxies must
  retry reads on 502/503/504 (the `deploy/proxy` configurations do).

### TLS and per-domain certificates
- Uploaded certificates (PEM) or **automatic ACME** (Let's Encrypt/ZeroSSL, HTTP-01 or TLS-ALPN-01 challenge; DNS-01 P2 for wildcards).
- In a cluster: certificates and ACME state stored encrypted in `.nexo.sys`, renewal performed by the leader, distribution to all nodes.
- Selection via **SNI**; expiry visible in the dashboard with an alert at 21 days.
- A certificate is presented only for the aliases of the **enabled** distributions that reference it
  (`tls.certificate: upload:<id>`, or the ACME certificate of `auto`) and only if it covers the name and has not
  expired; otherwise the listener uses its own certificate (the server's static one for the S3/console domains). A
  certificate uploaded with a name of the main domains, of `NEXO_DOMAIN` or `*.<NEXO_DOMAIN>` (or a wildcard covering
  them) is rejected: whoever has `admin:ManageCertificates` cannot replace the S3 endpoint's certificate.

### Cache and invalidation
Full cache management **per distribution, per domain and per path**: see [11 — Domain cache](11-domain-cache.md).

### Per-distribution observability
Metrics `nexo_distribution_requests_total{distribution,host,status}`, bytes served, cache hit ratio, latency;
access log in combined or JSON format; dedicated charts in the dashboard.

## 10.3 Dashboard

| Code | Feature |
|--------|--------------|
| D-ACC-01 | Bucket → **Access rules** tab: sortable rule table (drag & drop of the priority), editor with a choice of prefix/glob/regex, principals, actions, conditions; YAML/JSON view |
| D-ACC-02 | **Pattern tester**: enter one or more keys → highlights which rules match and the final outcome (allow/deny) for a chosen identity/IP/host; examples of keys existing in the bucket |
| D-ACC-03 | Public preset (private / public-read / public-prefix…) with clear warnings and a "Public" status visible in the bucket list |
| D-ACC-04 | Block public access at cluster and bucket level |
| D-DST-01 | **Distributions** section: list with domains, buckets, certificate status, traffic |
| D-DST-02 | Creation wizard: bucket/prefix → domains → mode (website/s3) → access → TLS; DNS instructions (CNAME/A/AAAA) and a check that the domain points to the cluster |
| D-DST-03 | Behavior editor: ordered list with pattern (prefix/glob/regex), methods, access, cache, headers; "which behavior does this URL use?" tester |
| D-DST-04 | Error pages, redirects, SPA fallback, CORS, security headers (recommended preset) |
| D-DST-05 | Key groups and signed URL/cookie generator |
| D-DST-06 | Restrictions: IP allow/deny, origin secret per proxy (generation and rotation), rate limit |
| D-DST-07 | Certificates: upload, ACME, status and expiry dates |
| D-DST-08 | Cache invalidation and history; metrics and logs per distribution |

## 10.4 CLI

```bash
# Access rules
nexoctl access rule add local/media --id img-pub --allow --principal '*' --action read \
        --regex '^img/[0-9]{4}/.*\.(jpe?g|png)$' --ignore-case
nexoctl access rule add local/media --id no-private --deny --principal '*' --action all --glob '**/private/**' --priority 5
nexoctl access rule ls|info|rm|move local/media [--id ...] [--priority N]
nexoctl access test local/media/img/2026/a.jpg --principal anonymous --ip 1.2.3.4 --host cdn.example.com
nexoctl access public set public-read local/media | access public set public-prefix local/media --glob 'pub/**'
nexoctl access block-public on|off [local | local/media]
nexoctl access export|import local/media rules.yaml

# Distributions
nexoctl dist create local website --bucket web --prefix /prod --mode website --domain www.example.com --domain example.com
nexoctl dist domain add|rm local website '*.preview.example.com'
nexoctl dist behavior add local website --glob '/assets/**' --cache-control 'public, max-age=31536000, immutable'
nexoctl dist behavior add local website --regex '^/downloads/.*\.zip$' --access signed --key-group releases
nexoctl dist error-page|redirect|headers|restrict ... 
nexoctl dist keygroup create local releases && nexoctl dist sign-url local website /downloads/v1.zip --expire 1h [--ip 1.2.3.4]
nexoctl dist cert set local website --auto | --cert crt.pem --key key.pem
nexoctl dist origin-secret rotate local website
nexoctl dist test local website https://www.example.com/assets/app.js   # shows the resulting behavior, access and headers
nexoctl dist invalidate local website '/assets/**'
nexoctl dist ls|info|edit|disable|enable|rm|export|import
```
All commands support `--json` and YAML import/export (GitOps management of the configurations).

## 10.5 Admin API

| Method | Path |
|--------|------|
| GET/PUT | `/api/v1/buckets/{b}/access-rules` (atomic replacement with `If-Match` on the configuration's ETag) |
| POST | `/api/v1/buckets/{b}/access-rules/test` |
| GET/PUT | `/api/v1/access/block-public`, `/api/v1/buckets/{b}/block-public` |
| GET/POST | `/api/v1/distributions` · GET/PUT/DELETE `/api/v1/distributions/{id}` |
| POST | `/api/v1/distributions/{id}/test` · `/invalidations` · `/origin-secret/rotate` |
| GET/POST/DELETE | `/api/v1/keygroups`, `/api/v1/keygroups/{id}/keys` · POST `/api/v1/distributions/{id}/sign` |
| GET/POST/DELETE | `/api/v1/certificates` |
| GET | `/api/v1/routing/resolve?host=` (diagnostics: which distribution/bucket answers for a host) |

Policy actions: `admin:ManageAccessRules`, `admin:ManageDistributions`, `admin:ManageCertificates`, `admin:SignDistributionURL`.
`admin:ManageDistributions` is also evaluated on the **origin bucket** (resource `arn:aws:s3:::<bucket>`): to create
and modify a distribution (current and new bucket), delete it, rotate its origin secret and modify the keys of a key
group (addition, removal, deletion of the group), authorized on every origin bucket of the distributions that sign
with that group. A user denied on a bucket can neither free its aliases nor sign its premium content.

## 10.6 Tests

### Server
| ID | Case | Expected |
|----|------|--------|
| TAR-01 | Prefix/glob/regex matching: table of > 200 cases (segments, `**`, classes, alternatives, case-insensitive, Unicode, keys with `%`, spaces, `+`) | Expected outcomes |
| TAR-02 | Invalid regex, too long, over the rule limit | 400 error with position |
| TAR-03 | Bypass attempts: `%2F`, double encoding, `./`, `//`, Unicode NFC/NFD variants, uppercase | No bypass |
| TAR-04 | Pathological regexes (`(a+)+$` on long input) | Linear time (< 1 ms on 1 KiB) |
| TAR-05 | A deny in an access rule prevails over an IAM allow and over the bucket policy; allow from a rule without an IAM policy | As in §10.1 |
| TAR-06 | Named groups with `${aws:username}` (home directories) | Each user accesses only their own |
| TAR-07 | IP conditions (IPv4/IPv6, behind a trusted and an untrusted proxy), referer, host, time window, headers | Expected outcomes; IP from an untrusted proxy ignored |
| TAR-08 | Block public access active with a `*` rule | Rejected / ignored depending on configuration |
| TAR-09 | Rule change on node1 → effect on node4 | < 1 s |
| TAR-10 | Benchmark: 200 regex rules, evaluation per request | p99 < 50 µs |
| TDS-01 | Host routing: exact aliases, wildcards, most specific wins, virtual-host, unknown domain | Correct distribution/bucket |
| TDS-02 | Duplicate alias on two distributions | 409 |
| TDS-03 | Website mode: index, SPA fallback, error page with configured status, regex redirect with groups, HTTP→HTTPS, www | Expected responses |
| TDS-04 | s3 mode on a custom domain: SDK with a custom endpoint, SigV4 and presigned URLs generated with that domain | 200 |
| TDS-05 | Behaviors: order, first match wins, disallowed methods → 405/403, cache-control and response headers applied | — |
| TDS-06 | Signed URLs and cookies: valid, expired, altered signature, different IP, wildcard resource, key group rotation | 200 / 403 |
| TDS-07 | Origin secret: OK with the correct header, 403 without or with a wrong one, two secrets active during rotation | — |
| TDS-08 | PROXY protocol v1/v2 and `Forwarded`/`X-Forwarded-*` only from `NEXO_TRUSTED_PROXIES` | Correct real IP, spoofing ignored |
| TDS-09 | Origin `pathPrefix`: impossible to escape the prefix (`/../`, encoded) | 403/404 |
| TDS-10 | TLS: SNI with several certificates, ACME against Pebble (test ACME) including renewal, cluster with the leader renewing | Certificate served by all nodes |
| TDS-11 | Cache and invalidation: see the TCC-* tests in [11](11-domain-cache.md#1112-tests) | — |
| TDS-12 | Range, conditional GET, HEAD, correct `Content-Type` via a distribution | Compliant |
| TDS-13 | Per-IP rate limit (P2) | 429 over the threshold |
| TDS-14 | Fuzzing of the Host header, path and signature parameters | No panic/bypass |

### CLI
| ID | Case |
|----|------|
| TCL-A-01 | `access rule add/ls/rm/move` + `access test` consistent with real requests via curl |
| TCL-A-02 | `access export` → `import` on another bucket: identical rules |
| TCL-D-01 | `dist create` with 2 domains → `curl -H 'Host: ...'` on both returns the content |
| TCL-D-02 | `dist sign-url` → curl OK, 403 after expiry |
| TCL-D-03 | `dist test` shows the expected behavior and headers |
| TCL-D-04 | `dist origin-secret rotate`, `dist invalidate`, `dist cert set --cert/--key` |

### Dashboard
| ID | Case |
|----|------|
| TD-A-01 | Create a regex rule with the tester: example keys highlighted correctly; invalid regex blocked with a message |
| TD-A-02 | Reordering priorities with drag & drop persists and is honored by the server |
| TD-A-03 | public-prefix preset → object accessible anonymously only in the prefix; "Public" badge in the bucket list |
| TD-D-01 | Distribution wizard with 2 domains + DNS instructions; reachability check (in tests: hosts resolved to the server) |
| TD-D-02 | Behavior editor + URL tester; SPA fallback and error pages working from the browser on the configured domain |
| TD-D-03 | Key group: private key shown once; generated signed URL opens the file |
| TD-D-04 | Expiring certificate highlighted; upload of a valid/invalid certificate |
| TD-D-05 | A user without `admin:ManageDistributions` cannot see/modify distributions |

### Cluster and Docker
| ID | Case |
|----|------|
| TCD-01 | Compose with **2 nginx proxies** (different domains, different origin secrets) in front of the 4-node cluster: both access points work, direct access to the nodes without the secret is rejected |
| TCD-02 | A node serving a distribution is stopped: the proxy falls back to the other nodes without errors |
| TCD-03 | Distribution created from the dashboard → active on all nodes without restart |

Implementation of TCD-01..03 (`test/proxy`, build tag `proxy`, `make test-proxy`, CI job `proxy`): the two proxies
are **nginx** (`X-Forwarded-*`) and **HAProxy** (PROXY protocol v2 with SSL TLV) instead of two nginx, to cover both
ways of describing the client (`deploy/docker/compose.multi-proxy.yml`). Without Docker:
- `TestMultiProxy` (SNSD, SNMD, 4×2 cluster in-process): two Go proxies (`httputil.ReverseProxy`) with their own TLS,
  domain and origin secret, load balancing and failover like nginx; distributions created and modified through the
  console's Admin API behind proxy A and served immediately by every node (tens of ms measured, test limit 3 s)
  (TCD-03); direct access to the nodes without a secret or with a wrong secret → 403, with the secret but without
  HTTPS → redirect (TCD-01); real IP and spoofing (`X-Forwarded-For`, `Forwarded`, PROXY header from an untrusted
  peer) through both (TDS-08); node stopped under load → no client errors and no 502, writes and distribution
  changes during the outage, the restarted node serves them (TCD-02); distribution-signed URLs with the real client's
  `IpAddress`; TPS-19 on custom domains (TCC-C-05 in [11](11-domain-cache.md#1112-tests)).
- `TestRealProxies` (4×2 cluster): the **same** nginx and HAProxy **files** as the compose, adapted to loopback
  addresses, with the real binaries (skipped if missing, mandatory with `NEXO_PROXY_REAL=1`), plus the example
  distributions in `multi-proxy/site.yaml`.
