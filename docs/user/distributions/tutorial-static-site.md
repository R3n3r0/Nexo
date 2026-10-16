# Tutorial: a static site on two domains behind nginx

In this tutorial you publish a static site on `www.example.com` and `www.example.it` (plus the bare domain
`example.com`, redirected to `www`), with:

- TLS certificates obtained and renewed automatically by Nexo through ACME (Let's Encrypt);
- nginx in front, which also hosts other sites on the same machine;
- HTTP → HTTPS redirect, a 404 page, security headers;
- assets cached for a year, a `/premium/` area reachable only with signed URLs;
- content caching and invalidation after a deploy.

All the commands were tested with the binaries of this repository, with a test ACME server (Pebble)
instead of Let's Encrypt and nginx 1.24.

## Architecture

```text
                         ┌──────────────────── nginx ─────────────────────┐
 browser ── :80  ──────▶ │ http  { server_name www.example.com ... }      │ ──▶ Nexo :8080 (web, HTTP)
                         │        X-Forwarded-For / X-Forwarded-Proto      │
 browser ── :443 ──────▶ │ stream { ssl_preread: SNI of the site domains } │ ──▶ Nexo :8443 (web, TLS)
                         │        TCP passthrough + PROXY protocol         │
                         │        other SNI ─▶ the other nginx sites       │
                         └─────────────────────────────────────────────────┘
```

- On **443** nginx does not terminate TLS for the site's domains: it forwards them to Nexo (TCP passthrough,
  chosen by SNI) adding the **PROXY protocol**, so Nexo knows the real client IP. Nexo itself presents the
  certificates obtained through ACME.
- On **80** nginx forwards the HTTP requests (redirects to HTTPS and ACME HTTP-01 challenges) with the
  `X-Forwarded-*` headers.
- Nexo serves the distributions on two dedicated listeners (`--web-address`, `--web-tls-address`) listening
  only on `127.0.0.1`: the S3 API and the console stay where they are.

> **Alternative**: if you prefer nginx to terminate TLS (with certbot), use
> [`deploy/proxy/nginx.conf`](https://github.com/R3n3r0/Nexo/blob/main/deploy/proxy/nginx.conf): the
> distributions are served on the S3 port and protected with an *origin secret*. The rest of the tutorial
> (distribution, behaviors, cache) does not change; just skip the ACME part (`dist cert set --none`).

## 1. DNS

Point all the domains to the public address of the machine running nginx:

```text
www.example.com.   300  IN  A     203.0.113.10
example.com.       300  IN  A     203.0.113.10
www.example.it.    300  IN  A     203.0.113.10
```

Ports 80 and 443 must be reachable from the Internet: Let's Encrypt verifies control of the domains
with an HTTP request on port 80.

## 2. Configure Nexo

Add to the service environment (`/etc/default/nexo` with the deb/rpm package, or the Docker env file):

```bash
NEXO_WEB_ADDRESS=127.0.0.1:8080        # distributions over HTTP and ACME HTTP-01 challenges
NEXO_WEB_TLS_ADDRESS=127.0.0.1:8443    # distributions over HTTPS, certificates by SNI
NEXO_PROXY_PROTOCOL=web                # the web listeners accept the PROXY protocol...
NEXO_TRUSTED_PROXIES=127.0.0.1         # ...and X-Forwarded-* only from nginx
NEXO_ACME_EMAIL=ops@example.com        # contact of the Let's Encrypt account
```

and restart (`sudo systemctl restart nexo`). Without `NEXO_ACME_DIRECTORY` Nexo uses the Let's Encrypt
production directory; for testing you can use the staging one
(`NEXO_ACME_DIRECTORY=https://acme-staging-v02.api.letsencrypt.org/directory`), which has looser limits but
certificates that browsers do not trust.

The listener that receives the PROXY protocol keeps accepting connections without it (the HTTP requests of
nginx on port 80); a PROXY header coming from an address outside `NEXO_TRUSTED_PROXIES` is ignored.

> **In a cluster**: set the same variables on every node (with the node's private address instead of
> `127.0.0.1`) and put all the nodes in the nginx `upstream` blocks. The ACME certificates are stored
> encrypted in the cluster and the renewal is performed by a single node; the HTTP-01 challenge can reach any
> node.

## 3. Configure nginx

The `stream` block requires the nginx stream module (package `libnginx-mod-stream` on Debian/Ubuntu,
included in `nginx` on RHEL and in the nginx.org packages) and goes in the main file `/etc/nginx/nginx.conf`,
**outside** the `http` block:

```nginx
# /etc/nginx/nginx.conf (main level, next to "http { ... }")
stream {
    # The site domains go to Nexo, everything else to the other nginx sites.
    map $ssl_preread_server_name $sni_backend {
        www.example.com  nexo_web_tls;
        www.example.it   nexo_web_tls;
        example.com      nexo_web_tls;
        default          other_sites;
    }
    upstream nexo_web_tls { server 127.0.0.1:8443; }
    upstream other_sites  { server 127.0.0.1:4443; }

    server {
        listen 443;
        listen [::]:443;
        ssl_preread    on;
        proxy_pass     $sni_backend;
        proxy_protocol on;
    }
}
```

With `proxy_protocol on` the other sites receive the PROXY protocol too: their HTTPS `server` blocks
therefore listen on `127.0.0.1:4443` instead of 443, with `listen 127.0.0.1:4443 ssl proxy_protocol;` and
`set_real_ip_from 127.0.0.1; real_ip_header proxy_protocol;` to keep the clients' IP.

For port 80, a regular `server` block (for example `/etc/nginx/conf.d/site.conf`):

```nginx
upstream nexo_web {
    server 127.0.0.1:8080;
    keepalive 16;
}

server {
    listen 80;
    listen [::]:80;
    server_name www.example.com www.example.it example.com;

    location / {
        proxy_pass         http://nexo_web;
        proxy_http_version 1.1;
        proxy_set_header   Connection        "";
        proxy_set_header   Host              $http_host;
        proxy_set_header   X-Forwarded-For   $remote_addr;
        proxy_set_header   X-Forwarded-Proto $scheme;
        proxy_set_header   Forwarded         "";
    }
}
```

- `Host` must be forwarded unchanged: Nexo chooses the distribution by domain.
- Nexo reads a single family of forwarded headers (`X-Forwarded-*` by default): a client's `Forwarded`
  header must be removed.

```bash
sudo nginx -t && sudo systemctl reload nginx
```

## 4. Upload the site

```console
$ nexoctl mb local/site
Bucket created successfully `local/site`.
$ nexoctl mirror public/ local/site
```

where `public/` is the folder generated by your static site generator, for example:

```text
public/
├── index.html
├── 404.html
├── assets/app.css
└── premium/guide.pdf
```

The bucket stays **private**: the distribution decides what is public on the domains.

## 5. Create the distribution

```console
$ nexoctl dist create local site --bucket site --mode website \
    --domain www.example.com --domain www.example.it --domain example.com --index index.html
Created the distribution `site` (website) of `site` on www.example.com, www.example.it, example.com.
$ nexoctl dist error-page set local site 404 /404.html
Errors 404 of `site` answer /404.html.
$ nexoctl dist redirect add local site --host-from example.com --to-host www.example.com
Added the redirection 1 of `site`: host example.com → www.example.com (301).
$ nexoctl dist redirect add local site --https-only
Added the redirection 2 of `site`: HTTP → HTTPS (301).
$ nexoctl dist headers local site --security
Updated the headers of `site` (4 response headers).
```

- `--index index.html` serves `index.html` for the paths ending with `/` (and `/about` becomes
  `/about/` if `about/index.html` exists);
- the 404 page replaces the error for missing objects (for a single page application use
  `--spa /index.html` instead);
- the redirects apply in order, the first one wins: here the bare domain goes to `www` and every HTTP request
  moves to HTTPS (`301`);
- `--security` adds `Strict-Transport-Security`, `X-Content-Type-Options`, `X-Frame-Options` and
  `Referrer-Policy`.

## 6. Automatic certificates

```console
$ nexoctl dist cert set local site --auto
$ nexoctl dist cert list local
ID                       SOURCE  STATUS         EXPIRES                 DOMAINS                  USED BY
acme-site                acme    valid          2027-01-13 12:10:32 UTC example.com,www.example.com,www.example.it site
```

Within a few seconds Nexo obtains a certificate with all the domains of the distribution (in the logs:
`certs: ordering an ACME certificate` and then `certs: ACME certificate obtained`). Renewal is automatic
before the expiry; when a domain is added or removed (`nexoctl dist domain add|remove`) the certificate
is requested again with the updated names. The status is also in the console (**Distributions** →
**Certificates**) and in the metric `nexo_tls_certificate_expiry_seconds`.

If the request fails (`STATUS` other than `valid`), the reason is in the server logs: usually the DNS does
not point to the machine yet or port 80 does not reach Nexo. With `NEXO_ACME_CHALLENGE=tls-alpn-01` the
verification happens on port 443 instead (it works thanks to the nginx passthrough) and port 80 is not needed.

## 7. Verify

```console
$ curl -sI https://www.example.com/
HTTP/2 200
content-type: text/html; charset=utf-8
etag: "a364651c001eb0c6e796a39165827d26"
referrer-policy: strict-origin-when-cross-origin
server: Nexo
strict-transport-security: max-age=63072000; includeSubDomains
x-cache: MISS
x-content-type-options: nosniff
x-frame-options: SAMEORIGIN
$ curl -sI http://www.example.com/about | grep -i '^HTTP\|^location'
HTTP/1.1 301 Moved Permanently
Location: https://www.example.com/about
$ curl -sI https://example.com/x | grep -i '^HTTP\|^location'
HTTP/2 301
location: https://www.example.com/x
$ curl -s https://www.example.it/missing
<!doctype html><title>404</title><h1>Page not found</h1>
```

`nexoctl dist test` shows how a path would be served without making the request, and `dist resolve` which
distribution answers a domain:

```console
$ nexoctl dist resolve local www.example.it
www.example.it: distribution `site` (alias www.example.it, website), bucket site
```

## 8. Assets with a long cache

If the files in `assets/` have names with a content hash (`app.3f9a1c.css`), they can stay cached for a
year, in browsers and in Nexo:

```console
$ nexoctl dist behavior add local site --glob '/assets/**' --cache-control 'public, max-age=31536000, immutable'
$ nexoctl dist cache set local site --policy standard
Cache of `site`, distribution: policy standard.
$ nexoctl dist cache set local site --behavior '/assets/**' --policy immutable-assets
Cache of `site`, behavior glob /assets/**: policy immutable-assets.
```

**Behaviors** apply to the paths that match the pattern (the first match wins) and inherit the rest from
the default behavior. The Nexo cache keeps the responses in memory (and on disk with `NEXO_CACHE_DIR`):
the `X-Cache` header tells whether a response comes from the cache (`HIT`) or from the origin (`MISS`).

```console
$ nexoctl cache inspect local https://www.example.com/assets/app.css
URL: https://www.example.com/assets/app.css
...
Policy: immutable-assets (behavior)
TTL: default 365d (behavior), min 30d, max 365d
Cache-Control: public, max-age=31536000, immutable
Node local: cached in memory (object), age 0s, fresh for 31535999s, 17B
```

## 9. Restricted content with signed URLs

The `/premium/` area is served only with a signed URL (or cookie), CloudFront style. Create a **key group**:
the private key generated by the server is shown only once and `nexoctl` saves it in its
configuration.

```console
$ nexoctl dist keygroup create local premium
Created the key group `premium` with the key KZONJWDFG2WYLS (ed25519); private key saved to /home/ops/.nexoctl/keys/local/KZONJWDFG2WYLS.pem.
$ nexoctl dist behavior add local site --glob '/premium/**' --access signed --key-group premium
Added a behavior for glob /premium/** to `site` (2 behaviors).
$ curl -s -o /dev/null -w '%{http_code}\n' https://www.example.com/premium/guide.pdf
403
$ nexoctl dist sign-url local site /premium/guide.pdf --expire 1h
https://www.example.com/premium/guide.pdf?Expires=1792069683&Signature=rjZU2T2I...&Key-Pair-Id=KZONJWDFG2WYLS
$ curl -s -o /dev/null -w '%{http_code}\n' 'https://www.example.com/premium/guide.pdf?Expires=1792069683&Signature=rjZU2T2I...&Key-Pair-Id=KZONJWDFG2WYLS'
200
```

- `--ip 203.0.113.7` (or a CIDR) restricts the URL to one client; `--not-before` delays its validity;
- `--cookie --resource 'https://www.example.com/premium/*'` signs instead three `CloudFront-*` cookies valid
  for all the files of the area, to be set by your application after the user logs in;
- the signature covers the full URL, port included: if the site answers on a port other than 443, pass
  `--host www.example.com:PORT`;
- your application can sign by itself with the private key (format compatible with the CloudFront SDKs) or
  ask the Admin API for the signature (`POST /api/v1/distributions/{id}/sign`, permission
  `admin:SignDistributionURL`);
- to rotate the keys: `nexoctl dist keygroup key add local premium`, update whoever signs, then
  `nexoctl dist keygroup key remove local premium OLD_KEYID` (the signatures of the removed key stop
  being valid immediately).

## 10. Update the site and invalidate the cache

When an object of the bucket changes, Nexo discards the cache entries of that object by itself, on every node
(automatic invalidation, on by default):

```console
$ nexoctl mirror --overwrite public/ local/site
$ curl -sI https://www.example.com/ | grep -i x-cache
x-cache: MISS
```

To invalidate by hand (for example after changing the settings, or for content that depends on several
objects), from a deploy pipeline that has only `admin:InvalidateCache`:

```console
$ nexoctl dist invalidate local site '/assets/**' --wait
Invalidation inv-18deb2a317f89272-b0784049 (distribution site, glob /assets/**): completed on 1/1 node, 0 entries removed.
$ nexoctl cache purge local site /index.html --domain www.example.com
Invalidation inv-18deb2a318ce0c49-3c3c374e (distribution site, domain www.example.com, path /index.html): completed on 1/1 node, 1 entry removed.
$ nexoctl cache warm local site --prefix /assets/ --wait
Warm-up warm-18deb2ab7ae2bf2b-32b4ead8 of `site`: completed, 1/1 objects.
```

`nexoctl cache stats local` reports the hit ratio, the bytes served from the cache and the most requested
URLs per domain.

## 11. Access log and GitOps

The complete configuration of the distribution is exported as YAML, to be versioned together with the site:

```console
$ nexoctl dist export local site -o site.yaml
Exported 1 distribution to site.yaml.
$ nexoctl dist cache export local site > site-cache.yaml
```

The access log is enabled in the YAML (`logging` section) and applied with `dist import`:

```yaml
logging:
  accessLog: true
  format: combined      # or json (default)
```

```console
$ nexoctl dist import local site.yaml
Distribution `site` updated.
```

Every request ends up in the server log with the `distribution-access` component and the real client IP
received with the PROXY protocol:

```text
{"level":"INFO","msg":"203.0.113.7 - - [15/Oct/2026:12:09:31 +0000] \"GET / HTTP/2.0\" 200 54 \"-\" \"Mozilla/5.0 ...\"","component":"distribution-access","distribution":"site"}
```

## Command summary

```bash
nexoctl mb local/site
nexoctl mirror public/ local/site
nexoctl dist create local site --bucket site --mode website \
  --domain www.example.com --domain www.example.it --domain example.com --index index.html
nexoctl dist error-page set local site 404 /404.html
nexoctl dist redirect add local site --host-from example.com --to-host www.example.com
nexoctl dist redirect add local site --https-only
nexoctl dist headers local site --security
nexoctl dist cert set local site --auto
nexoctl dist behavior add local site --glob '/assets/**' --cache-control 'public, max-age=31536000, immutable'
nexoctl dist cache set local site --policy standard
nexoctl dist cache set local site --behavior '/assets/**' --policy immutable-assets
nexoctl dist keygroup create local premium
nexoctl dist behavior add local site --glob '/premium/**' --access signed --key-group premium
nexoctl dist sign-url local site /premium/guide.pdf --expire 1h
```

For the concepts (modes, behaviors, access, cache, proxies) see the [distributions overview](README.md).
