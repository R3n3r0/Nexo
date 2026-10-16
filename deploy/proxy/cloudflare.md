# Cloudflare in front of Nexo

Cloudflare (proxied DNS records, "orange cloud") can sit in front of Nexo for the console, the S3
API and the distributions, alone or in front of one of the proxies of this directory (Cloudflare →
nginx/Caddy/Traefik/HAProxy → Nexo). It needs four things on the Cloudflare side and three on the
Nexo side.

## Cloudflare

1. **SSL/TLS mode "Full (strict)"** (SSL/TLS → Overview). Cloudflare connects to the origin over
   TLS and checks its certificate: give the origin (Nexo with `NEXO_CERTS_DIR`, the web TLS
   listener of the distributions or the proxy behind Cloudflare) a certificate of the domains, for
   instance a Cloudflare Origin CA certificate. "Flexible" would send the requests in clear and
   make every distribution with an HTTPS-only redirect loop.

2. **An origin secret header** added to every request towards the origin (Rules → Transform
   Rules → Modify Request Header → Set static), so that the distributions refuse the requests that
   bypass Cloudflare:

   | Field | Value |
   |-------|-------|
   | When incoming requests match | `(http.host in {"www.example.com" "example.com" "cdn.example.com"})` |
   | Header name | `X-Origin-Secret` |
   | Value | the secret of this access point (`openssl rand -hex 32`) |

   In the same rule, **remove `X-Forwarded-Host`** (Modify Request Header → Remove): Cloudflare
   passes the `X-Forwarded-Host` a client sends through unchanged. Nexo ignores it by default
   (`NEXO_TRUST_FORWARDED_HOST` off: the routing uses the `Host` header, which Cloudflare forwards
   unchanged), but a proxy behind Cloudflare, or a Nexo with that option on, would let a client pick
   another distribution sharing the origin secret, bypassing the per-hostname Cloudflare policies
   (WAF, Access) and forging the `nexo:host` conditions.

   The same rule with the API (the `http_request_late_transform` phase of the zone):

   ```bash
   curl -X PUT "https://api.cloudflare.com/client/v4/zones/$ZONE_ID/rulesets/phases/http_request_late_transform/entrypoint" \
     -H "Authorization: Bearer $CF_API_TOKEN" -H "Content-Type: application/json" \
     --data '{
       "rules": [{
         "description": "Nexo origin secret",
         "expression": "(http.host in {\"www.example.com\" \"example.com\" \"cdn.example.com\"})",
         "action": "rewrite",
         "action_parameters": {
           "headers": {
             "X-Origin-Secret": { "operation": "set", "value": "REPLACE-WITH-THE-SECRET" },
             "X-Forwarded-Host": { "operation": "remove" }
           }
         }
       }]
     }'
   ```

3. **No Cloudflare cache on the S3 API and on the console** (Caching → Cache Rules → Bypass cache
   for `s3.example.com` and `console.example.com`): signed requests, presigned URLs and console
   pages must reach Nexo. For the distributions either bypass the Cloudflare cache too (Nexo caches
   them itself, docs/11) or keep it and let Nexo purge it on every write and invalidation with a
   purge webhook of the distribution:

   ```bash
   nexoctl cache webhook add local website --domain www.example.com \
     --type cloudflare --zone "$ZONE_ID" --token-file cf.token
   ```

4. **Upload size.** Cloudflare refuses request bodies over 100 MB (Free and Pro plans, 200 MB
   Business, 500 MB Enterprise by default): S3 clients behind Cloudflare must upload large objects
   in multipart parts below that size (`nexoctl cp` and the AWS SDKs do with their default part
   sizes of 8–16 MiB; `aws s3 cp` with `multipart_chunksize` below the limit), and a presigned
   single PUT is limited to it. The S3 API of a deployment that receives larger single uploads
   should not go through Cloudflare (a DNS-only record, "grey cloud").

Cloudflare forwards the `Host` header of the client unchanged (SigV4 and presigned URLs keep
working) and adds `X-Forwarded-For`, `X-Forwarded-Proto` and `CF-Connecting-IP`. Spectrum (TCP
proxy, Enterprise) can send the PROXY protocol instead; the HTTP proxy cannot.

## Nexo

```bash
# Cloudflare's address ranges (https://www.cloudflare.com/ips/): only they may describe the client.
NEXO_TRUSTED_PROXIES=$( { curl -fsS https://www.cloudflare.com/ips-v4; echo; curl -fsS https://www.cloudflare.com/ips-v6; } | grep . | paste -sd, -)
NEXO_FORWARDED_HEADERS=x-forwarded        # the default
NEXO_TRUST_FORWARDED_HOST=false           # the default: route on the Host header Cloudflare preserves
NEXO_SECRET_CLOUDFLARE=<the secret of the Transform Rule>
```

and on the distributions served through Cloudflare:

```yaml
restrictions:
  originSecretHeader:
    name: X-Origin-Secret
    values: ["${secret:cloudflare}"]      # resolved from NEXO_SECRET_CLOUDFLARE on every node
```

Keep the list of ranges up to date (Cloudflare publishes changes in advance). With Cloudflare in
front of another proxy, the proxy must keep the `X-Forwarded-For` of Cloudflare and append the
address of Cloudflare to it (nginx `$proxy_add_x_forwarded_for`, Traefik
`forwardedHeaders.trustedIPs`, Caddy `trusted_proxies`): Nexo reads the list from the right and
stops at the first address that is not a trusted proxy, so `NEXO_TRUSTED_PROXIES` lists the
proxy and the Cloudflare ranges.

`CF-Connecting-IP` is not read: Nexo trusts one family of forwarding headers, and
`X-Forwarded-For` carries the same address.
