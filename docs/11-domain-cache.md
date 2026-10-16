# 11. Per-domain cache management

Extends the distributions ([10](10-domains-access-distributions.md)) with a **built-in HTTP cache** and full cache
management **per distribution, per domain and per path**, configurable from the dashboard, CLI and Admin API.
It works in single-node and cluster mode.

Advantage over an external CDN: Nexo is also the origin, so it **knows when an object changes** and can invalidate
the cache automatically and exactly.

## 11.1 Configuration levels

Settings are inherited from the most general level to the most specific; each level can override the previous one:

```
Global (cluster) ─▶ Distribution ─▶ Domain (alias) ─▶ Behavior (path)
```

Example: the `website` distribution has `www.example.com` with a long cache and `preview.example.com` with the cache
disabled, while serving the same bucket.

```yaml
id: website
cache:                              # distribution default
  enabled: true
  policy: standard
domains:
  - host: www.example.com
    cache: { policy: aggressive, quota: 20GiB }
  - host: "*.preview.example.com"
    cache: { enabled: false }       # previews always fresh
  - host: static.example.it
    cache: { policy: standard, quota: 5GiB, purgeWebhooks: [cloudflare-it] }
behaviors:
  - pathPattern: { type: glob, pattern: "/assets/**" }
    cache: { ttl: { default: 365d, min: 1d, max: 365d }, browserCacheControl: "public, max-age=31536000, immutable" }
  - pathPattern: { type: regex, pattern: '^/api/' }
    cache: { enabled: false }
```

## 11.2 Cache policies

A **cache policy** is a reusable object (as in CloudFront) referenced by name. Predefined policies:
`disabled`, `standard`, `aggressive`, `immutable-assets`, `respect-origin`.

| Field | Description | Default (`standard`) |
|-------|-------------|----------------------|
| `ttl.default` / `min` / `max` | TTL in the cache if the object does not specify `Cache-Control`; the limits apply in any case | 1h / 0 / 7d |
| `respectObjectCacheControl` | Uses the `Cache-Control`/`Expires` stored in the object's metadata | true |
| `browserCacheControl` | `Cache-Control` sent to the client (may differ from the internal TTL) | inherited from the object |
| `cacheKey.queryString` | `none` / `all` / `allowlist: [v, w]` / `denylist` | `none` |
| `cacheKey.headers` | Headers included in the key (e.g. `Accept-Language`) | — |
| `cacheKey.cookies` | Cookies included in the key | — |
| `cacheKey.normalize` | Query sorting, lowercasing, removal of tracking parameters (`utm_*`, `fbclid`) | true |
| `cacheKey.includeHost` | Separate key per domain, or shared among the distribution's aliases | true |
| `staleWhileRevalidate` | Serves the expired copy while refreshing it in the background | 60s |
| `staleIfError` | Serves the expired copy if the origin (e.g. a set without quorum) fails | 1d |
| `negativeCaching` | TTL for 404/403 | 10s |
| `bypass` | Conditions that skip the cache: `Authorization` header, cookies, `?nocache` query, methods ≠ GET/HEAD | Authorization present |
| `compression` | gzip/br/zstd variants stored separately (`Vary: Accept-Encoding`) | gzip + br for text types |
| `minSize` / `maxObjectSize` | Objects outside the range are not cached | 0 / 1 GiB |
| `rangeCaching` | Block caching (e.g. 8 MiB) for Range requests and video | true, 8 MiB blocks |
| `contentTypes` | Allowlist/denylist per content type | all |

Security rules: responses to **authenticated** or **signed** requests are never shared between different users; for
`signed` content the signature is verified **before** the cache lookup and does not enter the key.

Key implementation (version `v2`; on-disk entries of an earlier format are discarded at startup): every component is
encoded unambiguously (object with the path escaped, `/` excluded; scope, bucket, header and cookie names and values,
credential with the query escaped), so an object whose name contains `?`, `|` or `#` never shares the entry of
another object read with a query, a header or a cookie. Every entry also carries its identity (bucket, object,
version) and is served only if it matches the requested one, also by the owner in `shared` mode. Responses keyed by
header or cookie carry `Vary` (the listed headers, `Cookie`), in addition to `Accept-Encoding` for the compressed
variants; a `HEAD` negotiates the same variant as the `GET`.

## 11.3 Cache storage

| Tier | Technology | Use |
|---------|-----------|-----|
| L1 memory | Concurrent cache with W-TinyLFU eviction (an `otter`/`ristretto`-style library) | Small, frequently requested objects |
| L2 disk | Dedicated directory (`--cache-dir`, an SSD/NVMe separate from the data drives is recommended), files with header + body, in-memory index | Large objects, Range blocks |

- **Quotas**: global (`--cache-size`), per distribution and **per domain**; LRU/LFU eviction within the quota.
- **Request collapsing**: simultaneous requests for the same uncached object produce a single read from the origin.
  An in-flight read is shared only if the object has not been invalidated since it started (invalidation barrier
  recorded at the start, checked again when the result arrives): a request that starts after the 200 of a PUT never
  receives content read before it. Revalidations update an entry only with a compare-and-swap on the revalidated
  entry, and a negative response (404) uses the barrier taken before the origin read: a concurrent creation is not
  masked.
- The cache is **optional**: without `--cache-dir` only L1 stays active, with a memory limit.
- In Docker: a separate `/cache` volume in the Compose files.

## 11.4 Cache in a cluster

| Mode | Description | When to use it |
|----------|-------------|---------------|
| `local` (default) | Each node has its own cache | Load balancer with stickiness, or few nodes |
| `shared` | The cache key is assigned to an "owner" node with **consistent hashing** (rendezvous) over the cluster nodes: the node receiving the request asks the owner via RPC. A single copy per object in the cluster, total cache = sum of the caches | Many nodes behind a round-robin load balancer |
| `shared+local` | Like `shared` plus a small local L1 for the hottest objects | Traffic heavily concentrated on a few objects |

- Owner node offline → read directly from the origin (no error), the ring is recomputed. Calls to the owner (reading a
  piece, presence) have their own timeout (`fetchTimeout`, default 5 s, capped by the ack timeout for presence): a
  frozen owner (SIGSTOP, VM pause) is treated as offline and the request falls back to the origin without storing.
- **Invalidations** are propagated to all nodes via peer RPC with a sequence number; a node that comes back after an
  absence empties its cache if it missed invalidations (non-contiguous sequence).
- **Trust lease** (TCC-C-04, TCC-05 in a cluster): a node considers its entries reliable only until
  `start of the last complete sequence comparison with all peers + lease` (default: comparison every 1 s, lease 2 s,
  on the monotonic clock). Once the lease has expired (frozen process, one-way partition, lagging peer), or while a
  peer is ahead (`Seq` greater than the last applied message), the entries are revalidated with the origin before
  being served; a lag that does not close within `retry + ack timeout` empties the cache. Symmetrically, a write that
  cannot deliver the invalidation to a recently reachable peer waits (at most one lease) for that peer's lease to
  expire before answering 200: after the 200 of a PUT no node serves the old content any more, even if it has not
  noticed it was isolated.
- **Origin without quorum** (TCC-07): a distribution's access decision does no I/O (block public access, access rules
  and bucket policy come from the in-memory cache of the bucket metadata). A precautionary reload of the cached state
  (periodic resync or after a freeze) that cannot reread the metadata for lack of quorum keeps deciding with the last
  known configuration, as it would have without the reload; an explicit invalidation of the bucket (a change notified
  by a peer) discards it. So expired entries are served `STALE` (`stale-if-error`), fresh ones `HIT`, and a path never
  cached answers `503` with `Retry-After`, never 403.

## 11.5 Invalidation

| Type | How |
|------|------|
| **Automatic** | On PUT/DELETE/CopyObject/CompleteMultipart/a change of an object's metadata/tags/current version, Nexo invalidates all the cache entries concerning it on **all the domains** that serve that bucket/prefix. Configurable (`autoInvalidate: true`) |
| By path | Exact paths or glob/regex (`/assets/**`), limited to one domain or to all the distribution's domains |
| By tag (surrogate key) | Objects can have a `Cache-Tag` (from the `x-amz-meta-cache-tag` metadata or from an S3 tag); purge by tag, e.g. `product-42` |
| By domain | Empties the whole cache of a domain |
| By distribution / global | Empties everything |
| Soft purge | Marks as expired (still served with `stale-while-revalidate`) instead of deleting |

- Every invalidation has an ID and a status (`in progress` / `completed` on N nodes) and appears in the history.
- **External propagation**: for each domain you can configure **purge webhooks** to CDNs/proxies in front of Nexo
  (Cloudflare, Fastly, Akamai, nginx `proxy_cache_purge`, Varnish `BAN`, generic webhook with a template), invoked on
  both manual and automatic invalidations, with retries and a persistent queue.
- **Webhook security** (whoever has `admin:ManageCache` cannot read the node's secrets or probe the internal
  network): the secrets (`token`, `clientSecret`) are plain text or `${secret:webhook-<name>}` references
  (`NEXO_SECRET_WEBHOOK_<NAME>`): no other node secret (the proxies' origin secrets, access rule secrets) and no
  `${secret:}` in free-form headers. A stored secret is masked in responses and stays bound to the destination:
  changing the type, URL, zone, `clientToken` or network requires entering it again. The calls only reach public
  addresses (checked on the address actually connected, so also after a DNS rebinding), never a cluster node; the
  internal networks of an nginx/Varnish are enabled with `--cache-webhook-allow` (`NEXO_CACHE_WEBHOOK_ALLOW`,
  comma-separated CIDRs). No HTTP proxy and no redirects; the test of a webhook reports only the status, never the
  response body.

## 11.6 Warm-up

- `prefetch` of a list of paths or of a prefix/glob for a domain (e.g. after deploying a static site).
- `warmOnWrite` option: written objects that match a pattern are cached immediately.
- Bandwidth/concurrency limit so as not to affect traffic.

## 11.7 Headers and diagnostics

- Response headers: `X-Cache: HIT | MISS | STALE | BYPASS | EXPIRED | REVALIDATED`, `Age`, `X-Cache-Node` (optional),
  `X-Cache-Key` (only with an authorized debug header).
- Full support for conditional requests from the client (`ETag`, `Last-Modified`, `304`).
- Inspection endpoint: given a URL, shows the computed cache key, the applied policy (with the level it comes from),
  presence in the cache on each node, age, expiry.

## 11.8 Per-domain metrics

`nexo_cache_requests_total{distribution,host,result}`, `nexo_cache_hit_ratio`, `nexo_cache_bytes_served_total{source=cache|origin}`,
`nexo_cache_size_bytes{host,tier}`, `nexo_cache_evictions_total`, `nexo_cache_invalidations_total`, HIT vs MISS latency,
origin bandwidth saved. Dedicated dashboard charts for each domain and "top objects" (most requested, most MISSes).

## 11.9 Dashboard

| Code | Feature |
|--------|--------------|
| D-CCH-01 | Global **Cache** page: size used per tier (memory/disk), hit ratio, bandwidth saved, top domains |
| D-CCH-02 | Distribution → **Cache** tab: table of the domains with policy, quota, usage, hit ratio, enabled/disabled, per-domain override |
| D-CCH-03 | Editor of reusable **cache policies** (TTL, key, bypass, stale, compression) with a preview of the resulting `Cache-Control` header |
| D-CCH-04 | **Invalidation**: by path/glob/regex, by tag, by domain, soft/hard; history with per-node status |
| D-CCH-05 | **URL inspector**: key, effective policy and the level it comes from, cache status on each node |
| D-CCH-06 | Warm-up: paste a list of URLs or choose a prefix, progress |
| D-CCH-07 | External purge webhooks per domain with a connection test |
| D-CCH-08 | Per-domain charts: HIT/MISS/STALE, bytes from cache vs origin, top objects |

## 11.10 CLI

```bash
nexoctl cache policy create|ls|info|edit|rm local aggressive --ttl-default 1d --ttl-max 30d --query allowlist:v --stale-while-revalidate 5m
nexoctl dist cache set local website --policy standard
nexoctl dist cache set local website --domain www.example.com --policy aggressive --quota 20GiB
nexoctl dist cache set local website --domain '*.preview.example.com' --disable
nexoctl dist cache set local website --behavior '/assets/**' --ttl-default 365d
nexoctl cache purge local website --domain www.example.com '/assets/**' [--soft]
nexoctl cache purge local website --tag product-42
nexoctl cache purge local website --domain static.example.it --all
nexoctl cache purge status local <id>
nexoctl cache inspect local https://www.example.com/assets/app.js
nexoctl cache warm local website --domain www.example.com --from urls.txt | --prefix /assets/
nexoctl cache stats local [website] [--domain www.example.com] [--watch]
nexoctl cache webhook add|ls|rm|test local website --domain static.example.it --type cloudflare --zone ... --token-file ...
```

## 11.11 Admin API

| Method | Path |
|--------|------|
| GET/POST/PUT/DELETE | `/api/v1/cache/policies[/{name}]` |
| GET/PUT | `/api/v1/distributions/{id}/cache` (default + per-domain and per-behavior overrides) |
| POST | `/api/v1/distributions/{id}/invalidations` (`{paths|patterns|tags|domain|all, soft}`) · GET `/invalidations/{inv}` |
| POST | `/api/v1/distributions/{id}/cache/warm` · GET `/warm/{job}` |
| GET | `/api/v1/cache/inspect?url=` · `/api/v1/cache/stats?distribution=&host=` |
| GET/POST/DELETE | `/api/v1/distributions/{id}/cache/webhooks` · POST `/webhooks/{w}/test` |

Policy actions: `admin:ManageCache`, `admin:InvalidateCache` (assignable separately, e.g. to a deploy pipeline).

## 11.12 Tests

### Server
| ID | Case | Expected |
|----|------|--------|
| TCC-01 | Inheritance global → distribution → domain → behavior | Correct effective policy for every combination |
| TCC-02 | Same bucket on two domains with different policies | Different TTLs and headers per domain; a domain with the cache disabled is always MISS/BYPASS |
| TCC-03 | Cache key: query none/allowlist/denylist, normalization, headers, cookies, `includeHost` true/false | Expected HIT/MISS |
| TCC-04 | TTL: the object's `Cache-Control` honored/capped by min/max; `Expires` | Correct expiries (fake clock) |
| TCC-05 | **Automatic invalidation**: PUT new content → the next request on all domains returns the new content | Never old content after the PUT's 200 (single node and cluster) |
| TCC-06 | Invalidation by path, glob, regex, tag, domain, global; soft purge | Only the intended entries removed/marked |
| TCC-07 | `stale-while-revalidate` and `stale-if-error` with an origin without quorum (drives turned off) | Stale copy served, `X-Cache: STALE` |
| TCC-08 | Request collapsing: 1000 concurrent requests on a MISS | A single read from the origin |
| TCC-09 | Range caching: Range requests on a 2 GiB video | Blocks in the cache, byte-identical content |
| TCC-10 | Compression variants and `Vary` | A client without `Accept-Encoding` receives uncompressed content |
| TCC-11 | Security: an authenticated/signed request does not serve cached content to an anonymous user; `signed` content with an expired signature → 403 even if cached; a change of access rules invalidates the decisions | No leak |
| TCC-12 | Negative caching of a 404 → object created → visible immediately (automatic invalidation) | 200 |
| TCC-13 | Per-domain quotas and eviction; cache disk full | Usage within the quota, no error to the client |
| TCC-14 | Cache directory corrupted / deleted at runtime | Entries discarded, served from the origin |
| TCC-15 | External purge webhooks: correct payloads (Cloudflare, Varnish, generic), retries with the endpoint offline | Delivered after recovery |
| TCC-16 | Benchmark HIT from L1/L2 vs MISS | L1 HIT p99 < 1 ms on 64 KiB objects |

### Cluster
| ID | Case | Expected |
|----|------|--------|
| TCC-C-01 | `shared` mode: the same URL requested from 4 nodes | 1 MISS, then HIT; a single copy in the cluster |
| TCC-C-02 | Owner node stopped | No error, reads from the origin, ring recomputed |
| TCC-C-03 | PUT on node1 → GET from node3 (local and shared) | New content |
| TCC-C-04 | Node isolated during an invalidation and then back | Empties its cache (non-contiguous sequence), no old content |
| TCC-C-05 | Compose with 2 proxies and different domains: a purge for one domain does not touch the other | — |

TCC-C-05 is verified by `test/proxy` (`make test-proxy`) without Docker: two proxies (Go in-process in SNSD, SNMD and
cluster; real nginx and HAProxy with the files of `deploy/docker/compose.multi-proxy.yml` on the cluster) with
different domains and origin secrets in front of the same distribution; each node's cache is filled through both, the
purge of one domain (`POST /distributions/{id}/invalidations` with `domain` and `all`, completed on all nodes) leaves
the other domain `HIT` on every node, the purge of a path on one domain does not touch the other, and an S3 write
immediately invalidates both domains (TCC-05 through the proxies). Requests arrive with the proxy's `Host` header,
port included: the cache key and the per-domain purge use the host without the port. In the compose each node has its
own `/cache` volume (`NEXO_CACHE_DIR`).

Other end-to-end tests (SNSD/SNMD/cluster matrix of `test/integration`, `test/cluster`):
- `TestCacheKeyIsUnambiguous` (TCC-03, TCC-11): a `signed` regex behavior with the key `allowlist: [v]`; the same byte
  sequence with an encoded `?` (`/downloads/v1.zip%3Fv=1`) never serves the signed content, an anonymous 404
  (`/app.js%3Fv=2`) does not poison the entry of `/app.js?v=2`, objects with `?` and `|` in the name have their own
  entries.
- `TestCacheStaleIfOriginDown` (TCC-07, SNMD and cluster): more drives than the parity renamed and a resync of the
  nodes' state: expired entry `STALE`, fresh entry `HIT`, path never cached `503` with `Retry-After`.
- `TestTCCC04PausedNode` (`make test-cluster`, TCC-C-04 with 4 processes, `local` and `shared` modes): a node frozen
  with SIGSTOP during the PUT never serves the old content when it resumes.

### CLI
| ID | Case |
|----|------|
| TCL-C-01 | `cache policy create` + `dist cache set --domain` → `curl` shows the expected `Cache-Control` and `X-Cache` |
| TCL-C-02 | `cache purge --domain ... '/assets/**'` → `purge status` completed on all nodes → `curl` MISS |
| TCL-C-03 | `cache inspect` consistent with the real behavior |
| TCL-C-04 | `cache warm --from urls.txt` → subsequent requests HIT |
| TCL-C-05 | `cache stats --json` valid against the schema |

### Dashboard
| ID | Case |
|----|------|
| TD-C-01 | Policy override for a domain in the Cache tab → effect verified with a request to the domain |
| TD-C-02 | Policy editor: `Cache-Control` preview updated in real time, TTL validation (min ≤ default ≤ max) |
| TD-C-03 | Invalidation by glob from the dashboard → per-node status until "completed" |
| TD-C-04 | The URL inspector shows the effective policy and the level it comes from |
| TD-C-05 | Per-domain hit ratio charts with real data generated by the test |
| TD-C-06 | A user with only `admin:InvalidateCache` can invalidate but not modify policies |
